// Package fakenomad is an in-memory stand-in for the subset of the Nomad
// HTTP API a Gas City Nomad runtime provider calls: job register/dispatch/
// deregister, job and allocation reads (both blocking-capable via the
// index/wait query params against an X-Nomad-Index response header, per the
// Nomad consistency model), the interactive alloc-exec WebSocket, and the
// system GC endpoint. It exists so `gc runtime check`/`gc runtime
// conformance` (and this pack's own tests) can exercise the full wire
// contract without a live Nomad cluster. It implements exactly the endpoint
// families the provider calls: jobs, jobs-list, dispatch, deregister,
// job-read, allocations, alloc-exec-WS, client-fs-cat, system — and fault
// hooks (FailNext) for scripted failure injection (L2 in the test
// pyramid), plus a request Trace() for asserting call order. Job
// deregister (NRT-P1-03) was added once the lifecycle ops needed it;
// client-fs-cat (NRT-P1-07) was added for the stop-path transcript/
// evidence egress read; jobs-list (NRT-P1-08b) was added for list-running's
// children-of-parent enumeration and is reused by NRT-P1-09's
// positive-attribution adoption (04 §2.1 rule 6); register/dispatch/
// job-read/allocations/alloc-exec-WS/system are the original NRT-P1-02
// scope.
//
// Out of scope (by design): fidelity beyond the endpoints a provider calls,
// and real WS TLS.
package fakenomad

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultWait is the blocking-query wait used when the caller's wait param
// is absent or unparsable; maxWait mirrors Nomad's documented 10-minute cap
// (kept far shorter here since this is an in-memory fake with no network
// latency to hide).
const (
	defaultWait = 5 * time.Second
	maxWait     = 10 * time.Second
)

// Job is the fake's minimal record of a registered or dispatched job.
type Job struct {
	ID          string
	Namespace   string
	NodePool    string
	ParentID    string
	Dispatched  bool
	Meta        map[string]string
	ModifyIndex uint64
}

// Allocation is the fake's minimal record of a placed allocation.
type Allocation struct {
	ID            string
	JobID         string
	Namespace     string
	DesiredStatus string
	ClientStatus  string
	ModifyIndex   uint64
}

// defaultAllocFiles seeds every dispatched allocation with the two files a
// stop-path egress reads (transcript/evidence) via the client fs "cat"
// endpoint, so a provider driving a fake run always has something real to
// copy without a separate seeding API.
func defaultAllocFiles(allocID string) map[string]string {
	return map[string]string{
		"alloc/logs/transcript.log": fmt.Sprintf("fakenomad transcript for %s\n", allocID),
		"alloc/data/evidence.json":  fmt.Sprintf(`{"alloc_id":%q}`+"\n", allocID),
	}
}

// fault is a scripted failure or delay matched by exact method+path. By
// default it is one-shot (consumed on first match) and answers status/body
// instead of routing normally; sticky keeps it queued across matches
// (persistent failure modes, e.g. permanent-auth) and passthrough applies
// only the delay before falling through to the normal handler (latency
// scenarios that still succeed, e.g. slow-server).
type fault struct {
	method      string
	path        string
	status      int
	body        string
	delay       time.Duration
	sticky      bool
	passthrough bool
}

// Server is a fake Nomad API server. Zero value is not usable; construct
// with NewServer. Safe for concurrent use.
type Server struct {
	mu         sync.Mutex
	index      uint64
	waitCh     chan struct{}
	jobs       map[string]*Job
	allocs     map[string]*Allocation
	allocFiles map[string]map[string]string // allocID -> path -> content
	execDirs   map[string]string            // allocID -> its exec scratch dir (lazy)
	faults     []fault
	dispSeq    uint64
	trace      []string

	execRoot string // parent of every per-alloc exec scratch dir
	httpSrv  *httptest.Server
	closed   bool // guards Close against running its teardown twice
}

// NewServer starts a fake Nomad server on a loopback listener and returns
// it. Call Close when done.
func NewServer() *Server {
	s := newServer()
	s.httpSrv = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	return s
}

// NewTLSServer is NewServer's TLS-terminated twin: same handler, served over
// a self-signed TLS listener. It exists for the TLS-fail fault row — a
// client that doesn't trust the self-signed cert gets a real TLS handshake
// failure rather than a scripted stand-in.
func NewTLSServer() *Server {
	s := newServer()
	s.httpSrv = httptest.NewTLSServer(http.HandlerFunc(s.serveHTTP))
	return s
}

func newServer() *Server {
	// Rooted at /tmp rather than os.TempDir(): macOS's per-user $TMPDIR
	// (/var/folders/.../T) is deep enough that execRoot + the per-alloc
	// scratch dir + "tmux-<uid>/default" blows past AF_UNIX's sun_path
	// limit (104 bytes on macOS), so tmux's own socket connect fails.
	execRoot, err := os.MkdirTemp("/tmp", "fakenomad-exec-")
	if err != nil {
		execRoot, err = os.MkdirTemp("", "fakenomad-exec-")
	}
	if err != nil {
		// Exceedingly unlikely (a functioning os.TempDir is a test-harness
		// precondition); fall back to a fixed-but-still-scratch location
		// rather than making NewServer fallible for this.
		execRoot = filepath.Join(os.TempDir(), fmt.Sprintf("fakenomad-exec-%d", time.Now().UnixNano()))
	}
	return &Server{
		waitCh:     make(chan struct{}),
		jobs:       map[string]*Job{},
		allocs:     map[string]*Allocation{},
		allocFiles: map[string]map[string]string{},
		execDirs:   map[string]string{},
		execRoot:   execRoot,
	}
}

// URL returns the fake server's base URL (scheme+host).
func (s *Server) URL() string { return s.httpSrv.URL }

// Close shuts down the underlying HTTP server. It also kills any tmux
// server a caller's exec'd command started inside a per-alloc scratch dir
// (allocScratchDir) and removes those dirs — exec genuinely runs commands
// (runCommand), so a caller driving the launch command
// (`tmux new-session -d -s main`) over exec leaves a real background tmux
// server behind unless something reaps it.
//
// Close is idempotent: it is common (and legitimate — an explicit Close
// plus a belt-and-suspenders t.Cleanup(srv.Close)) for a caller to invoke
// it more than once. A second run must be a no-op rather than re-running
// the tmux teardown: by then execRoot is already removed, so the per-alloc
// dir kill-server would target no longer exists, isolatedTmuxEnv's
// TMUX_TMPDIR resolves to nothing, and tmux silently falls back to the
// caller's own default socket — killing the real host tmux server (and any
// sibling session, e.g. a worker's) instead of the fake's isolated one.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	dirs := make([]string, 0, len(s.execDirs))
	for _, d := range s.execDirs {
		dirs = append(dirs, d)
	}
	s.mu.Unlock()

	s.httpSrv.Close()
	for _, d := range dirs {
		cmd := exec.Command("tmux", "kill-server")
		cmd.Env = isolatedTmuxEnv(d, "")
		_ = cmd.Run() // best-effort: no server running is not an error worth surfacing
	}
	_ = os.RemoveAll(s.execRoot)
}

// FailNext queues a one-shot fault: the next request matching method and
// exact path is answered with status and body instead of being routed to
// the normal handler. Matches are consumed in FIFO order.
func (s *Server) FailNext(method, path string, status int, body string) {
	s.queueFault(fault{method: method, path: path, status: status, body: body})
}

// FailSticky queues a fault that answers status/body to every request
// matching method and path, not just the first — for persistent failure
// modes (e.g. permanent-auth) as opposed to a single transient blip. Clear
// it with ClearFault.
func (s *Server) FailSticky(method, path string, status int, body string) {
	s.queueFault(fault{method: method, path: path, status: status, body: body, sticky: true})
}

// ClearFault removes any scripted fault (one-shot or sticky) matching
// method and path.
func (s *Server) ClearFault(method, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, f := range s.faults {
		if f.method == method && f.path == path {
			s.faults = append(s.faults[:i], s.faults[i+1:]...)
			return
		}
	}
}

// DelayNext queues a one-shot response delay: the next request matching
// method and path sleeps for delay, then is served normally. It exists for
// latency scenarios that still eventually succeed (slow-server) or that
// outlast a caller's own deadline (timeout-mid-dispatch), as opposed to
// FailNext's scripted failure response.
func (s *Server) DelayNext(method, path string, delay time.Duration) {
	s.queueFault(fault{method: method, path: path, delay: delay, passthrough: true})
}

func (s *Server) queueFault(f fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, f)
}

// Trace returns the method+path of every request served so far, in
// arrival order — the basis for asserting call ordering (e.g. that a
// stop-path fs egress read completes before the deregister call it must
// precede).
func (s *Server) Trace() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.trace))
	copy(out, s.trace)
	return out
}

// SetAllocStatus updates an allocation's client status and advances the
// server index, unblocking any blocking reads waiting on it. It exists so
// tests can deterministically drive the "blocking query resolved by index
// advance" scenario without sleeping.
func (s *Server) SetAllocStatus(allocID, clientStatus string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.allocs[allocID]
	if !ok {
		return false
	}
	a.ClientStatus = clientStatus
	a.ModifyIndex = s.bumpIndexLocked()
	return true
}

// PlaceAlloc adds a new allocation for jobID with the given ClientStatus and
// returns its ID. It exists so tests can simulate a Nomad reschedule placing
// a replacement allocation under the same job (the "replacement alloc"
// fault row) without a full scheduler.
func (s *Server) PlaceAlloc(jobID, clientStatus string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dispSeq++
	allocID := fmt.Sprintf("alloc-%06x", s.dispSeq)
	idx := s.bumpIndexLocked()
	s.allocs[allocID] = &Allocation{
		ID:            allocID,
		JobID:         jobID,
		DesiredStatus: "run",
		ClientStatus:  clientStatus,
		ModifyIndex:   idx,
	}
	return allocID
}

// bumpIndexLocked increments the index and wakes any blocked readers. Must
// be called with s.mu held.
func (s *Server) bumpIndexLocked() uint64 {
	s.index++
	close(s.waitCh)
	s.waitCh = make(chan struct{})
	return s.index
}

// blockUntil waits until the server index advances past since, or wait
// elapses, whichever comes first, then returns the current index. A zero
// since with no advance simply returns the current index immediately.
func (s *Server) blockUntil(since uint64, wait time.Duration) uint64 {
	s.mu.Lock()
	if s.index > since {
		cur := s.index
		s.mu.Unlock()
		return cur
	}
	ch := s.waitCh
	s.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.index
}

func (s *Server) takeFault(method, path string) (fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, f := range s.faults {
		if f.method == method && f.path == path {
			if !f.sticky {
				s.faults = append(s.faults[:i], s.faults[i+1:]...)
			}
			return f, true
		}
	}
	return fault{}, false
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.trace = append(s.trace, r.Method+" "+r.URL.Path)
	s.mu.Unlock()

	if f, ok := s.takeFault(r.Method, r.URL.Path); ok {
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		if !f.passthrough {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.status)
			if f.body != "" {
				_, _ = w.Write([]byte(f.body))
			}
			return
		}
	}

	parts := splitPath(r.URL.Path)

	// Job routes need special handling: dispatched child job IDs contain a
	// literal "/" (e.g. "parent/dispatch-<ts>-<hash>", per e2a-child-job-naming),
	// so the ID cannot be assumed to be exactly one path segment. Peel off a
	// recognized trailing verb (dispatch, allocations) if present; otherwise
	// every remaining segment belongs to the ID.
	if len(parts) >= 3 && parts[0] == "v1" && parts[1] == "job" {
		rest := parts[2:]
		last := rest[len(rest)-1]
		switch {
		case r.Method == http.MethodPost && last == "dispatch" && len(rest) >= 2:
			s.dispatchJob(w, r, strings.Join(rest[:len(rest)-1], "/"))
		case r.Method == http.MethodGet && last == "allocations" && len(rest) >= 2:
			s.listAllocsForJob(w, r, strings.Join(rest[:len(rest)-1], "/"))
		case r.Method == http.MethodPost:
			s.registerJob(w, r, strings.Join(rest, "/"))
		case r.Method == http.MethodGet:
			s.readJob(w, r, strings.Join(rest, "/"))
		case r.Method == http.MethodDelete:
			s.deregisterJob(w, r, strings.Join(rest, "/"))
		default:
			writeJSONError(w, http.StatusNotFound, "not found")
		}
		return
	}

	switch {
	case r.Method == http.MethodPost && len(parts) == 2 && parts[0] == "v1" && parts[1] == "jobs":
		s.registerJob(w, r, "")
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "v1" && parts[1] == "jobs":
		s.listJobs(w, r)
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "v1" && parts[1] == "allocations":
		s.listAllocs(w, r)
	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "v1" && parts[1] == "allocation":
		s.readAlloc(w, r, parts[2])
	case r.Method == http.MethodGet && len(parts) == 5 && parts[0] == "v1" && parts[1] == "client" && parts[2] == "allocation" && parts[4] == "exec":
		s.handleExecWS(w, r, parts[3])
	case r.Method == http.MethodGet && len(parts) == 5 && parts[0] == "v1" && parts[1] == "client" && parts[2] == "fs" && parts[3] == "cat":
		s.catAllocFile(w, r, parts[4])
	case r.Method == http.MethodPut && len(parts) == 3 && parts[0] == "v1" && parts[1] == "system" && parts[2] == "gc":
		s.systemGC(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) registerJob(w http.ResponseWriter, r *http.Request, pathID string) {
	var req struct {
		Job struct {
			ID        string            `json:"ID"`
			Namespace string            `json:"Namespace"`
			NodePool  string            `json:"NodePool"`
			Meta      map[string]string `json:"Meta"`
		} `json:"Job"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	id := req.Job.ID
	if id == "" {
		id = pathID
	}
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing job ID")
		return
	}
	ns := req.Job.Namespace
	if ns == "" {
		ns = "default"
	}

	s.mu.Lock()
	idx := s.bumpIndexLocked()
	s.jobs[id] = &Job{ID: id, Namespace: ns, NodePool: req.Job.NodePool, Meta: req.Job.Meta, ModifyIndex: idx}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"EvalID":         "",
		"JobModifyIndex": idx,
		"Warnings":       "",
		"Index":          idx,
		"KnownLeader":    true,
		"LastContact":    0,
	})
}

// jobListEntry is one row of the `GET /v1/jobs` response: the subset of a
// Nomad job summary a children-of-parent list needs (04 §2.1 rule 2/3/6).
// Meta is included only when the request carries `?meta=true` (real Nomad
// omits it from the list endpoint by default to save bandwidth — both the
// children-of-parent recovery path and dispatch's orphan-adoption lookup
// rely on that param, e2a-amend-jobs-list-params) and Status reflects
// whether the job currently has any non-terminal allocation ("running") or
// not ("dead"), the job-level non-terminal signal `list-running`'s and
// dispatch's children-of-parent enumeration filter on.
type jobListEntry struct {
	ID        string
	ParentID  string
	Namespace string
	NodePool  string
	Status    string
	Meta      map[string]string `json:"Meta,omitempty"`
}

// listJobs answers `GET /v1/jobs` (optionally `?meta=true`) — the
// children-of-parent enumeration both a list-running cluster-recovery path
// and dispatch's positive-attribution adoption (04 §2.1 rule 2/3/6) read:
// every job filters client-side on ParentID, since this fake mirrors real
// Nomad's jobs-list endpoint, which has no parent filter param of its own.
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	includeMeta := r.URL.Query().Get("meta") == "true"

	s.mu.Lock()
	idx := s.index
	out := make([]jobListEntry, 0, len(s.jobs))
	for _, j := range s.jobs {
		status := "dead"
		for _, a := range s.allocs {
			if a.JobID == j.ID && !terminalAllocStatus(a.ClientStatus) {
				status = "running"
				break
			}
		}
		entry := jobListEntry{ID: j.ID, ParentID: j.ParentID, Namespace: j.Namespace, NodePool: j.NodePool, Status: status}
		if includeMeta {
			entry.Meta = j.Meta
		}
		out = append(out, entry)
	}
	s.mu.Unlock()

	w.Header().Set("X-Nomad-Index", strconv.FormatUint(idx, 10))
	writeJSON(w, http.StatusOK, out)
}

// terminalAllocStatus reports whether a Nomad alloc ClientStatus is
// terminal — mirrors the provider's own isTerminalStatus (runtime/ops.go);
// duplicated here since fakenomad is a standalone module with zero deps on
// the runtime package.
func terminalAllocStatus(status string) bool {
	switch status {
	case "complete", "failed", "lost":
		return true
	default:
		return false
	}
}

func (s *Server) dispatchJob(w http.ResponseWriter, r *http.Request, parentID string) {
	s.mu.Lock()
	parent, ok := s.jobs[parentID]
	if !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "job not found")
		return
	}

	var req struct {
		Meta    map[string]string `json:"Meta"`
		Payload string            `json:"Payload"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	s.dispSeq++
	childID := fmt.Sprintf("%s/dispatch-%d-%06x", parentID, time.Now().UTC().Unix(), s.dispSeq)
	idx := s.bumpIndexLocked()
	s.jobs[childID] = &Job{ID: childID, Namespace: parent.Namespace, NodePool: parent.NodePool, ParentID: parentID, Dispatched: true, Meta: req.Meta, ModifyIndex: idx}

	allocID := fmt.Sprintf("alloc-%06x", s.dispSeq)
	allocIdx := s.bumpIndexLocked()
	s.allocs[allocID] = &Allocation{
		ID:            allocID,
		JobID:         childID,
		Namespace:     parent.Namespace,
		DesiredStatus: "run",
		ClientStatus:  "pending",
		ModifyIndex:   allocIdx,
	}
	s.allocFiles[allocID] = defaultAllocFiles(allocID)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"DispatchedJobID": childID,
		"EvalID":          "eval-" + childID,
		"JobCreateIndex":  idx,
		"Index":           allocIdx,
	})
}

// blockingParams parses the index/wait query params shared by every
// blocking-capable read endpoint.
func blockingParams(r *http.Request) (since uint64, wait time.Duration, blocking bool) {
	q := r.URL.Query()
	rawIdx := q.Get("index")
	if rawIdx == "" {
		return 0, 0, false
	}
	since, err := strconv.ParseUint(rawIdx, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	wait = defaultWait
	if rawWait := q.Get("wait"); rawWait != "" {
		if d, err := time.ParseDuration(rawWait); err == nil && d > 0 {
			wait = d
		}
	}
	if wait > maxWait {
		wait = maxWait
	}
	return since, wait, true
}

func (s *Server) readJob(w http.ResponseWriter, r *http.Request, id string) {
	if since, wait, blocking := blockingParams(r); blocking {
		s.blockUntil(since, wait)
	}
	s.mu.Lock()
	job, ok := s.jobs[id]
	idx := s.index
	s.mu.Unlock()
	if !ok {
		w.Header().Set("X-Nomad-Index", strconv.FormatUint(idx, 10))
		writeJSONError(w, http.StatusNotFound, "job not found")
		return
	}
	w.Header().Set("X-Nomad-Index", strconv.FormatUint(idx, 10))
	writeJSON(w, http.StatusOK, map[string]any{
		"ID":          job.ID,
		"Namespace":   job.Namespace,
		"NodePool":    job.NodePool,
		"ParentID":    job.ParentID,
		"Dispatched":  job.Dispatched,
		"Meta":        job.Meta,
		"ModifyIndex": job.ModifyIndex,
	})
}

func (s *Server) listAllocsForJob(w http.ResponseWriter, r *http.Request, jobID string) {
	if since, wait, blocking := blockingParams(r); blocking {
		s.blockUntil(since, wait)
	}
	s.mu.Lock()
	idx := s.index
	var out []*Allocation
	for _, a := range s.allocs {
		if a.JobID == jobID {
			out = append(out, a)
		}
	}
	s.mu.Unlock()
	w.Header().Set("X-Nomad-Index", strconv.FormatUint(idx, 10))
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listAllocs(w http.ResponseWriter, r *http.Request) {
	if since, wait, blocking := blockingParams(r); blocking {
		s.blockUntil(since, wait)
	}
	s.mu.Lock()
	idx := s.index
	var out []*Allocation
	for _, a := range s.allocs {
		out = append(out, a)
	}
	s.mu.Unlock()
	w.Header().Set("X-Nomad-Index", strconv.FormatUint(idx, 10))
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) readAlloc(w http.ResponseWriter, r *http.Request, id string) {
	if since, wait, blocking := blockingParams(r); blocking {
		s.blockUntil(since, wait)
	}
	s.mu.Lock()
	a, ok := s.allocs[id]
	idx := s.index
	s.mu.Unlock()
	if !ok {
		w.Header().Set("X-Nomad-Index", strconv.FormatUint(idx, 10))
		writeJSONError(w, http.StatusNotFound, "alloc not found")
		return
	}
	w.Header().Set("X-Nomad-Index", strconv.FormatUint(idx, 10))
	writeJSON(w, http.StatusOK, map[string]any{
		"ID":            a.ID,
		"JobID":         a.JobID,
		"Namespace":     a.Namespace,
		"DesiredStatus": a.DesiredStatus,
		"ClientStatus":  a.ClientStatus,
		"ModifyIndex":   a.ModifyIndex,
	})
}

// deregisterJob answers DELETE /v1/job/:id (Nomad's job-deregister family).
// Without ?purge=true the job record stays (a subsequent GET still returns
// 200 — matching real Nomad's stop-without-purge contract, e2a-stop-vs-purge)
// but every non-terminal allocation for it is driven to a terminal state, so
// a caller's confirm-terminal blocking read resolves. With ?purge=true the
// job is removed outright, so a subsequent GET 404s (confirmed absence).
func (s *Server) deregisterJob(w http.ResponseWriter, r *http.Request, id string) {
	purge := r.URL.Query().Get("purge") == "true"

	s.mu.Lock()
	if _, ok := s.jobs[id]; !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "job not found")
		return
	}
	idx := s.bumpIndexLocked()
	if purge {
		delete(s.jobs, id)
	}
	for _, a := range s.allocs {
		if a.JobID != id {
			continue
		}
		if a.ClientStatus == "complete" || a.ClientStatus == "failed" {
			continue
		}
		a.DesiredStatus = "stop"
		a.ClientStatus = "complete"
		a.ModifyIndex = idx
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"EvalID":          "",
		"EvalCreateIndex": idx,
		"JobModifyIndex":  idx,
		"Index":           idx,
	})
}

// catAllocFile answers `GET /v1/client/fs/cat/:allocID?path=...` — the
// client fs "cat" endpoint a stop-path egress reads transcript/evidence
// files through before the job is deregistered. A missing alloc or path is
// a 404, matching real Nomad's fs-cat behavior for an unknown target.
func (s *Server) catAllocFile(w http.ResponseWriter, r *http.Request, allocID string) {
	path := r.URL.Query().Get("path")
	s.mu.Lock()
	_, ok := s.allocs[allocID]
	var content string
	var fileOK bool
	if ok {
		content, fileOK = s.allocFiles[allocID][path]
	}
	s.mu.Unlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, "alloc not found")
		return
	}
	if !fileOK {
		writeJSONError(w, http.StatusNotFound, "file not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

func (s *Server) systemGC(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// allocScratchDir lazily creates and returns allocID's own exec scratch
// directory under s.execRoot, encoded so an arbitrary allocID (real Nomad
// alloc IDs are UUIDs; this fake's are "alloc-<hex>") can never collide or
// escape it (mirrors the runtime pack's own sidecar path scheme).
func (s *Server) allocScratchDir(allocID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dir, ok := s.execDirs[allocID]; ok {
		return dir
	}
	dir := filepath.Join(s.execRoot, base64.RawURLEncoding.EncodeToString([]byte(allocID)))
	_ = os.MkdirAll(dir, 0o700)
	s.execDirs[allocID] = dir
	return dir
}

// allocSecretsDir lazily creates and returns allocID's own secrets
// directory (NRT-P1-06 data contract: exec-stdin delivery into the task's
// secrets dir, never job env/meta/argv — E1a §4.5). Real Nomad exposes this
// as NOMAD_SECRETS_DIR pointing at a tmpfs unreadable via the client fs API;
// this fake settles for an isolated on-disk directory under the alloc's own
// scratch dir, which is enough to prove the provider writes secret files to
// the right place rather than the workspace or job spec.
func (s *Server) allocSecretsDir(allocID string) string {
	dir := filepath.Join(s.allocScratchDir(allocID), "secrets")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// runCommand actually executes command as a real subprocess and returns its
// exit code plus combined stdout+stderr (e2a-exec-protocol: the fake proves
// wire-level exit-code/output fidelity, not a canned reply). Each alloc gets
// its own TMUX_TMPDIR (and CWD) via allocScratchDir, so a launch command
// like `tmux new-session -d -s main` — the in-box tmux session name is a
// fixed wire-contract constant, not something this fake can rename — never
// collides across allocs sharing this one test machine's default tmux
// server. An empty command (no caller sends one; kept for robustness)
// preserves the pre-NRT-P1-05 scripted reply. stdin is wired to the real
// subprocess (NRT-P1-06): staging's tar-over-exec-stdin workspace/secrets
// payloads need a genuine pipe, not a canned reply, for the M3 receipt's
// env.workspace probe to mean anything.
func (s *Server) runCommand(allocID string, command []string, stdin []byte) (int, []byte) {
	if len(command) == 0 {
		return 0, []byte("ok\n")
	}
	dir := s.allocScratchDir(allocID)
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = isolatedTmuxEnv(dir, s.allocSecretsDir(allocID))
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return 0, out.Bytes()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), out.Bytes()
	}
	// Not even an exit-code failure (binary missing, etc.): surface as a
	// generic failure with the error appended to whatever output exists.
	out.WriteString(err.Error())
	return 1, out.Bytes()
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func splitPath(p string) []string {
	var out []string
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// isolatedTmuxEnv builds the environment for a command that may invoke tmux
// inside an alloc scratch dir. It strips any inherited TMUX so the tmux
// client can never resolve the caller's ambient server (a gc worker runs
// inside tmux; an inherited $TMUX made kill-server take down the whole
// host server and every sibling worker), then pins the socket dir.
// secretsDir, if non-empty, is also exported as NOMAD_SECRETS_DIR (NRT-P1-06)
// so a command run inside this env sees the same variable the real Nomad
// exec driver sets.
func isolatedTmuxEnv(dir, secretsDir string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_TMPDIR=") || strings.HasPrefix(kv, "NOMAD_SECRETS_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "TMUX_TMPDIR="+dir)
	if secretsDir != "" {
		env = append(env, "NOMAD_SECRETS_DIR="+secretsDir)
	}
	return env
}
