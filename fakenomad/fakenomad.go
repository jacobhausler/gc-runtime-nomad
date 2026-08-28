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
	"text/template"
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

// basePort anchors this fake's dynamic-port assignment (fnrt-t4l.24):
// each label declared in a job's Networks[0].DynamicPorts gets
// basePort+portSeq on every dispatch, standing in for the real port number
// a live Nomad client would bind and inject as NOMAD_PORT_<Label>.
const basePort = 21000

// Job is the fake's minimal record of a registered or dispatched job.
type Job struct {
	ID          string
	Namespace   string
	NodePool    string
	ParentID    string
	Dispatched  bool
	Meta        map[string]string
	ModifyIndex uint64

	// Tasks is the first task group's own tasks, each an exec-driver
	// command (Config's "command"/"args" pair, task-lifetime modeling for
	// NRT-P2-06.1/fnrt-t4l.13) — empty for a job registered with no task
	// Config, which keeps every pre-existing caller that registers a bare
	// `{"ID": ...}` job unaffected. A dispatched child inherits its
	// parent's Tasks (dispatch requests never carry their own TaskGroups).
	// Tasks[0] is this fake's modeled "main"/leader task: it alone drives
	// the alloc's ClientStatus (startAllTasks), mirroring real Nomad's own
	// per-task-state aggregation — an alloc stays "running" as long as ANY
	// task is still running, so a non-main task (e.g. a log-shipper
	// sidecar) crashing must never flip ClientStatus away from "running".
	Tasks []TaskSpec

	// PortLabels are the group's Networks[0].DynamicPorts labels (e.g.
	// "metrics" for the log-shipper's prometheus_exporter sink,
	// fnrt-t4l.13) — real Nomad assigns each one a real port number per
	// allocation and injects it into every task's environment as
	// NOMAD_PORT_<Label>. A dispatched child inherits its parent's
	// PortLabels (dispatch requests never carry their own Networks).
	PortLabels []string
}

// TaskSpec is one task within a dispatched job's single task group — the
// subset fakenomad needs to run each task as a real subprocess
// (fnrt-t4l.13 N-task group modeling) and render its template stanzas
// (fnrt-t4l.24).
type TaskSpec struct {
	Name    string
	Command []string
	// Env is the task's own Env block — part of the env a template stanza
	// renders against (fnrt-t4l.24), alongside NOMAD_PORT_<Label> and
	// NOMAD_ALLOC_DIR/NOMAD_TASK_DIR.
	Env map[string]string
	// Templates are the task's Nomad template stanzas (EmbeddedTmpl+
	// DestPath), rendered into the alloc's scratch dir before the task's
	// command starts (fnrt-t4l.24 — a live-Nomad proof showed a job spec
	// that relies on Nomad's own `{{ env "NOMAD_PORT_metrics" }}`
	// interpolation needs that rendering pass actually exercised, not just
	// escaped/string-matched by a test-local simulator).
	Templates []TemplateSpec
}

// TemplateSpec mirrors the subset of Nomad's template stanza fakenomad
// renders: EmbeddedTmpl is parsed and executed with Go's text/template
// package — the same "{{ }}" delimiter syntax and `env` function Nomad's
// own template stanza exposes — and the result is written to DestPath
// under the task's alloc dir.
type TemplateSpec struct {
	EmbeddedTmpl string
	DestPath     string
}

// Allocation is the fake's minimal record of a placed allocation.
type Allocation struct {
	ID            string
	JobID         string
	Namespace     string
	DesiredStatus string
	ClientStatus  string
	ModifyIndex   uint64

	// Ports is this allocation's own dynamic-port assignment, label ->
	// port number (fnrt-t4l.24) — the real per-allocation value every
	// task's NOMAD_PORT_<Label> env var (and any template stanza's
	// `env "NOMAD_PORT_<Label>"` call) resolves to.
	Ports map[string]int

	// TaskStates is the fake's per-task record, task name -> "running" /
	// "complete" / "failed" (fnrt-t4l.24). The main task's entry tracks its
	// real live state exactly like ClientStatus (startMainTask). A
	// non-main task's entry is set once, to "running", the moment its
	// command starts and is never updated again — mirroring this fake's
	// existing choice (startSideTask) that a side task's own crash or exit
	// is invisible to the alloc's aggregate state; a caller asking whether
	// that task ever reached running gets a stable answer instead of one
	// that can flip out from under it depending on how fast the task's own
	// process happens to exit.
	TaskStates map[string]string
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

// taskProc is the fake's live handle on one of a dispatched job's own
// task-command subprocesses (task-lifetime modeling, NRT-P2-06.1) — as
// opposed to a caller's alloc-exec commands, which run and complete
// synchronously via runCommand. done closes once the background Wait()
// goroutine has resolved (the alloc's ClientStatus for the main task, or
// just the process's own exit for a side task — see startAllTasks), so
// Close can bound how long it waits on a killed process before giving up.
type taskProc struct {
	cmd  *exec.Cmd
	done chan struct{}
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
	taskProcs  map[string][]*taskProc       // allocID -> its running task-command subprocesses, main task first (lazy)
	faults     []fault
	execFails  int // transient alloc-exec streams to close after stdin
	dispSeq    uint64
	portSeq    uint64 // dynamic-port assignment counter (fnrt-t4l.24)
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
		taskProcs:  map[string][]*taskProc{},
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
	var procs []*taskProc
	for _, ps := range s.taskProcs {
		procs = append(procs, ps...)
	}
	s.mu.Unlock()

	s.httpSrv.Close()
	// Kill every still-live task-command subprocess (task-lifetime
	// modeling, NRT-P2-06.1) before the tmux-kill-server sweep below: a
	// session supervisor script polls on a 5s interval, far longer than a
	// caller should ever wait on Close, so this reaps it directly instead
	// of waiting for it to notice its tmux server died.
	for _, p := range procs {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
		}
	}
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

// FailExecNext makes the next count alloc-exec WebSocket calls close after
// receiving the client's stdin-close frame, before sending a command result.
// This models the transient EOF Nomad can return while a freshly dispatched
// allocation is not ready for exec yet.
func (s *Server) FailExecNext(count int) {
	if count <= 0 {
		return
	}
	s.mu.Lock()
	s.execFails += count
	s.mu.Unlock()
}

func (s *Server) takeExecFailure() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.execFails == 0 {
		return false
	}
	s.execFails--
	return true
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
			ID         string            `json:"ID"`
			Namespace  string            `json:"Namespace"`
			NodePool   string            `json:"NodePool"`
			Meta       map[string]string `json:"Meta"`
			TaskGroups []struct {
				Networks []struct {
					DynamicPorts []struct {
						Label string `json:"Label"`
					} `json:"DynamicPorts"`
				} `json:"Networks"`
				Tasks []struct {
					Name      string            `json:"Name"`
					Config    map[string]any    `json:"Config"`
					Env       map[string]string `json:"Env"`
					Templates []struct {
						EmbeddedTmpl string `json:"EmbeddedTmpl"`
						DestPath     string `json:"DestPath"`
					} `json:"Templates"`
				} `json:"Tasks"`
			} `json:"TaskGroups"`
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
	var tasks []TaskSpec
	var portLabels []string
	if len(req.Job.TaskGroups) > 0 {
		group := req.Job.TaskGroups[0]
		for _, t := range group.Tasks {
			var templates []TemplateSpec
			for _, tmpl := range t.Templates {
				templates = append(templates, TemplateSpec{EmbeddedTmpl: tmpl.EmbeddedTmpl, DestPath: tmpl.DestPath})
			}
			tasks = append(tasks, TaskSpec{Name: t.Name, Command: taskCommand(t.Config), Env: t.Env, Templates: templates})
		}
		for _, n := range group.Networks {
			for _, p := range n.DynamicPorts {
				portLabels = append(portLabels, p.Label)
			}
		}
	}

	s.mu.Lock()
	idx := s.bumpIndexLocked()
	s.jobs[id] = &Job{ID: id, Namespace: ns, NodePool: req.Job.NodePool, Meta: req.Job.Meta, Tasks: tasks, PortLabels: portLabels, ModifyIndex: idx}
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
	tasks := parent.Tasks
	s.jobs[childID] = &Job{ID: childID, Namespace: parent.Namespace, NodePool: parent.NodePool, ParentID: parentID, Dispatched: true, Meta: req.Meta, Tasks: tasks, PortLabels: parent.PortLabels, ModifyIndex: idx}

	// Assign this allocation its own dynamic ports (fnrt-t4l.24) — real
	// Nomad picks a real port per allocation and injects it into every
	// task's environment as NOMAD_PORT_<Label>, which is exactly what a
	// template stanza's `env "NOMAD_PORT_<Label>"` call resolves against
	// (startAllTasks, renderTemplates).
	ports := make(map[string]int, len(parent.PortLabels))
	portEnv := make(map[string]string, len(parent.PortLabels))
	for _, label := range parent.PortLabels {
		s.portSeq++
		port := basePort + int(s.portSeq)
		ports[label] = port
		portEnv["NOMAD_PORT_"+label] = strconv.Itoa(port)
	}

	allocID := fmt.Sprintf("alloc-%06x", s.dispSeq)
	allocIdx := s.bumpIndexLocked()
	s.allocs[allocID] = &Allocation{
		ID:            allocID,
		JobID:         childID,
		Namespace:     parent.Namespace,
		DesiredStatus: "run",
		ClientStatus:  "pending",
		ModifyIndex:   allocIdx,
		Ports:         ports,
	}
	s.allocFiles[allocID] = defaultAllocFiles(allocID)
	s.mu.Unlock()

	// NOMAD_ALLOC_ID and NOMAD_META_<KEY> are real Nomad runtime env vars
	// injected into every task alongside the dynamic ports above — a
	// template stanza's `env "NOMAD_ALLOC_ID"` or `env "NOMAD_META_GC_SESSION"`
	// call resolves against these the same way `env "NOMAD_PORT_<Label>"`
	// resolves against portEnv (fnrt-3bvg, sibling of the t4l.24 port fix).
	portEnv["NOMAD_ALLOC_ID"] = allocID
	for k, v := range req.Meta {
		portEnv["NOMAD_META_"+strings.ToUpper(k)] = v
	}

	// Actually run each task's own command as a real subprocess so the
	// alloc's ClientStatus reflects its real lifetime (task-lifetime
	// modeling, NRT-P2-06.1/fnrt-t4l.13) — a job registered with no task
	// Config (every pre-existing bare `{"ID": ...}` caller) leaves tasks
	// empty and this is a no-op, keeping ClientStatus "pending" exactly as
	// before. The main task (tasks[0]) starts synchronously so the
	// dispatch response never races ahead of it: by the time this call
	// returns, it has either started (ClientStatus "running") or failed to
	// start (ClientStatus "failed"). Every other task starts in the
	// background — see startAllTasks. Each task's template stanzas (if any)
	// render before its command starts (fnrt-t4l.24).
	s.startAllTasks(allocID, tasks, portEnv)

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

// copyAlloc snapshots *a's fields, including a fresh copy of its
// TaskStates map, for use outside s.mu. TaskStates (unlike Ports, which is
// set once at dispatch and never touched again) is mutated by a background
// task-lifetime goroutine at any time (startMainTask), so sharing the map
// reference itself into a value that escapes the lock would be a genuine
// data race (caught by `go test -race`), the same concern
// listAllocsForJob's own doc comment already covers for the Allocation
// pointer.
func copyAlloc(a *Allocation) Allocation {
	out := *a
	if a.TaskStates != nil {
		out.TaskStates = make(map[string]string, len(a.TaskStates))
		for k, v := range a.TaskStates {
			out.TaskStates[k] = v
		}
	}
	return out
}

// listAllocsForJob and its siblings below copy each matched Allocation by
// value (via copyAlloc) while still holding s.mu, rather than letting a
// *Allocation escape the lock into writeJSON's later JSON-encode:
// task-lifetime modeling (startTask) now mutates a live Allocation's
// ClientStatus/ModifyIndex/TaskStates from a background goroutine at any
// time, so a pointer read here without the lock held is a genuine data race
// (caught by `go test -race` once a dispatched job's task actually runs),
// not just a theoretical one.
func (s *Server) listAllocsForJob(w http.ResponseWriter, r *http.Request, jobID string) {
	if since, wait, blocking := blockingParams(r); blocking {
		s.blockUntil(since, wait)
	}
	s.mu.Lock()
	idx := s.index
	var out []Allocation
	for _, a := range s.allocs {
		if a.JobID == jobID {
			out = append(out, copyAlloc(a))
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
	out := make([]Allocation, 0, len(s.allocs))
	for _, a := range s.allocs {
		out = append(out, copyAlloc(a))
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
	var alloc Allocation
	if ok {
		alloc = copyAlloc(a)
	}
	idx := s.index
	s.mu.Unlock()
	if !ok {
		w.Header().Set("X-Nomad-Index", strconv.FormatUint(idx, 10))
		writeJSONError(w, http.StatusNotFound, "alloc not found")
		return
	}
	w.Header().Set("X-Nomad-Index", strconv.FormatUint(idx, 10))
	writeJSON(w, http.StatusOK, map[string]any{
		"ID":            alloc.ID,
		"JobID":         alloc.JobID,
		"Namespace":     alloc.Namespace,
		"DesiredStatus": alloc.DesiredStatus,
		"ClientStatus":  alloc.ClientStatus,
		"ModifyIndex":   alloc.ModifyIndex,
		"Ports":         alloc.Ports,
		"TaskStates":    alloc.TaskStates,
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
	var toKill []*exec.Cmd
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
		for _, p := range s.taskProcs[a.ID] {
			toKill = append(toKill, p.cmd)
		}
	}
	s.mu.Unlock()

	// Kill every force-completed alloc's real task-command subprocesses —
	// ALL of them, not just the main task (task-lifetime modeling,
	// NRT-P2-06.1/fnrt-t4l.13) — mirrors real Nomad killing every task on
	// deregister. The background Wait() goroutines (startAllTasks) see the
	// alloc already terminal and leave ClientStatus alone.
	for _, cmd := range toKill {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}

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

// taskCommand extracts the exec-driver "command"/"args" pair from a Nomad
// task Config block (map[string]any, matching the wire JSON shape a real
// job register body carries) — the subset this fake needs to actually run a
// dispatched job's task as a real subprocess (task-lifetime modeling,
// NRT-P2-06.1). Returns nil for a Config with no (or non-string) "command",
// which callers treat as "no task to run" rather than an error.
func taskCommand(cfg map[string]any) []string {
	command, _ := cfg["command"].(string)
	if command == "" {
		return nil
	}
	out := []string{command}
	if rawArgs, ok := cfg["args"].([]any); ok {
		for _, a := range rawArgs {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// startAllTasks runs every one of tasks as a real subprocess (fnrt-t4l.13
// N-task group modeling). tasks[0] is this fake's modeled "main"/leader
// task and alone drives allocID's ClientStatus (startMainTask); every other
// task runs independently via startSideTask, whose own lifetime — start
// failure, crash, or clean exit — never touches ClientStatus, mirroring
// real Nomad's per-task-state aggregation (an alloc stays "running" as long
// as ANY task is still running). A task with an empty Command (no task
// Config, e.g. every pre-existing bare `{"ID": ...}` caller) is skipped
// entirely — for tasks[0] that keeps ClientStatus "pending" exactly as
// before this modeling existed. portEnv carries this allocation's own
// NOMAD_PORT_<Label> assignments (dispatchJob), which — together with each
// task's own Env and the alloc's NOMAD_ALLOC_DIR/NOMAD_TASK_DIR — is what
// that task's template stanzas (if any) render against before its command
// starts (fnrt-t4l.24).
func (s *Server) startAllTasks(allocID string, tasks []TaskSpec, portEnv map[string]string) {
	for i, t := range tasks {
		if len(t.Command) == 0 {
			continue
		}
		if i == 0 {
			s.startMainTask(allocID, t, portEnv)
		} else {
			s.startSideTask(allocID, t, portEnv)
		}
	}
}

// renderTemplates writes each of specs' EmbeddedTmpl through Go's
// text/template package — the same "{{ }}" delimiter syntax and `env`
// function Nomad's own template stanza exposes — into DestPath under dir,
// backed by env (fnrt-t4l.24: a live-Nomad proof showed a job spec that
// relies on Nomad's own `{{ env "NOMAD_PORT_metrics" }}` interpolation
// needs that rendering pass actually exercised, not just escaped/
// string-matched by a test-local simulator). Every EmbeddedTmpl this pack's
// production job specs build only ever uses Nomad's own "{{ }}" syntax for
// real Nomad interpolation; any "${...}" placeholders they also carry are a
// distinct substitution a task's own process resolves itself at runtime, so
// Go's text/template — which only ever recognizes its own "{{ }}"
// delimiters — correctly leaves them untouched.
func renderTemplates(dir string, specs []TemplateSpec, env map[string]string) error {
	for _, spec := range specs {
		tmpl, err := template.New(spec.DestPath).Funcs(template.FuncMap{
			"env": func(name string) string { return env[name] },
		}).Parse(spec.EmbeddedTmpl)
		if err != nil {
			return fmt.Errorf("parse template %s: %w", spec.DestPath, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, nil); err != nil {
			return fmt.Errorf("render template %s: %w", spec.DestPath, err)
		}
		dest := filepath.Join(dir, spec.DestPath)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("create directory for template %s: %w", spec.DestPath, err)
		}
		if err := os.WriteFile(dest, buf.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write template %s: %w", spec.DestPath, err)
		}
	}
	return nil
}

// taskTemplateEnv builds the env a task's template stanzas render against
// (fnrt-t4l.24): the task's own Env block, the allocation's dynamic ports as
// NOMAD_PORT_<Label> (real Nomad's own env-injection contract), and
// NOMAD_ALLOC_DIR/NOMAD_TASK_DIR pointing at the alloc's shared scratch dir
// — this fake keeps every task's directory the same rather than modeling
// real Nomad's separate per-task local/ subdirectory, since no template
// stanza this pack renders needs that distinction.
func taskTemplateEnv(dir string, taskEnv, portEnv map[string]string) map[string]string {
	env := map[string]string{
		"NOMAD_ALLOC_DIR": dir,
		"NOMAD_TASK_DIR":  dir,
	}
	for k, v := range taskEnv {
		env[k] = v
	}
	for k, v := range portEnv {
		env[k] = v
	}
	return env
}

// startMainTask renders task's template stanzas (if any) and then runs its
// command as a real subprocess standing in for Nomad's exec driver actually
// running a dispatched job's main task (task-lifetime modeling,
// NRT-P2-06.1) and drives allocID's ClientStatus accordingly: "running"
// once it starts, "failed" if a template fails to render or the command
// never starts, and "complete"/"failed" once it exits — the model a
// fixed-status alloc cannot express, and the one NRT-P2-06.1 needs to catch
// a task command (like the placeholder /bin/true this pack used to
// dispatch) that exits before launch's alloc-exec call can ever reach the
// box. Both the render and the start are synchronous (the caller —
// dispatchJob, via startAllTasks — blocks on them) so the dispatch response
// is never observed before the alloc's ClientStatus reflects whether the
// task actually started; only the exit wait runs in the background.
func (s *Server) startMainTask(allocID string, task TaskSpec, portEnv map[string]string) {
	dir := s.allocScratchDir(allocID)

	if err := renderTemplates(dir, task.Templates, taskTemplateEnv(dir, task.Env, portEnv)); err != nil {
		s.mu.Lock()
		if a, ok := s.allocs[allocID]; ok {
			a.ClientStatus = "failed"
			setTaskStateLocked(a, task.Name, "failed")
			a.ModifyIndex = s.bumpIndexLocked()
		}
		s.mu.Unlock()
		return
	}

	cmd := exec.Command(task.Command[0], task.Command[1:]...)
	cmd.Dir = dir
	cmd.Env = isolatedTmuxEnv(dir, "")

	err := cmd.Start()

	s.mu.Lock()
	a, ok := s.allocs[allocID]
	if err != nil {
		if ok {
			a.ClientStatus = "failed"
			setTaskStateLocked(a, task.Name, "failed")
			a.ModifyIndex = s.bumpIndexLocked()
		}
		s.mu.Unlock()
		return
	}
	done := make(chan struct{})
	s.taskProcs[allocID] = append(s.taskProcs[allocID], &taskProc{cmd: cmd, done: done})
	if ok {
		a.ClientStatus = "running"
		setTaskStateLocked(a, task.Name, "running")
		a.ModifyIndex = s.bumpIndexLocked()
	}
	s.mu.Unlock()

	go func() {
		waitErr := cmd.Wait()
		s.mu.Lock()
		if a, ok := s.allocs[allocID]; ok && !terminalAllocStatus(a.ClientStatus) {
			if waitErr == nil {
				a.ClientStatus = "complete"
				setTaskStateLocked(a, task.Name, "complete")
			} else {
				a.ClientStatus = "failed"
				setTaskStateLocked(a, task.Name, "failed")
			}
			a.ModifyIndex = s.bumpIndexLocked()
		}
		close(done)
		s.mu.Unlock()
	}()
}

// startSideTask renders task's template stanzas (if any) and then runs its
// command as a real subprocess standing in for a non-main task in the group
// (e.g. fnrt-t4l.13's log-shipper) — best effort, and deliberately never
// touches allocID's ClientStatus: a side task that fails to render, fails
// to start, crashes, or exits is invisible to the alloc's aggregate state,
// matching real Nomad (an alloc stays "running" as long as its main task
// is). Its own TaskStates entry is set once, to "running", the moment its
// command starts, and — like ClientStatus — is never updated again by this
// function: a caller asking whether this task ever reached running gets a
// stable answer rather than one that can flip out from under it depending
// on how fast the task's own process happens to exit (fnrt-t4l.24). A
// render or start failure is recorded as "failed" and the command is never
// started. A started task is still recorded in s.taskProcs so Close and
// deregisterJob's kill sweep reap it like any other task subprocess.
func (s *Server) startSideTask(allocID string, task TaskSpec, portEnv map[string]string) {
	dir := s.allocScratchDir(allocID)

	if err := renderTemplates(dir, task.Templates, taskTemplateEnv(dir, task.Env, portEnv)); err != nil {
		s.setTaskState(allocID, task.Name, "failed")
		return
	}

	cmd := exec.Command(task.Command[0], task.Command[1:]...)
	cmd.Dir = dir
	cmd.Env = isolatedTmuxEnv(dir, "")

	if err := cmd.Start(); err != nil {
		s.setTaskState(allocID, task.Name, "failed")
		return
	}
	s.setTaskState(allocID, task.Name, "running")

	done := make(chan struct{})
	s.mu.Lock()
	s.taskProcs[allocID] = append(s.taskProcs[allocID], &taskProc{cmd: cmd, done: done})
	s.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		close(done)
	}()
}

// setTaskStateLocked records taskName's state on a — the caller must
// already hold s.mu (mirrors bumpIndexLocked's naming convention).
func setTaskStateLocked(a *Allocation, taskName, state string) {
	if a.TaskStates == nil {
		a.TaskStates = map[string]string{}
	}
	a.TaskStates[taskName] = state
}

// setTaskState is setTaskStateLocked's lock-acquiring counterpart, for
// callers (startSideTask) that don't already hold s.mu and have no other
// alloc field to update alongside it.
func (s *Server) setTaskState(allocID, taskName, state string) {
	s.mu.Lock()
	if a, ok := s.allocs[allocID]; ok {
		setTaskStateLocked(a, taskName, state)
		a.ModifyIndex = s.bumpIndexLocked()
	}
	s.mu.Unlock()
}

// AssignedPort returns the dynamic port fakenomad assigned to label on
// allocID's own allocation (fnrt-t4l.24's NOMAD_PORT_<Label> env-injection
// contract), and whether that allocation/label combination exists — so a
// test asserting a rendered template's port value has something real to
// compare it against instead of a hardcoded constant.
func (s *Server) AssignedPort(allocID, label string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.allocs[allocID]
	if !ok {
		return 0, false
	}
	port, ok := a.Ports[label]
	return port, ok
}

// TaskState returns the fake's last-known state for taskName within
// allocID's allocation ("running"/"complete"/"failed"), and whether that
// task has ever been recorded at all. See Allocation.TaskStates for what
// "state" means for a non-main task.
func (s *Server) TaskState(allocID, taskName string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.allocs[allocID]
	if !ok {
		return "", false
	}
	state, ok := a.TaskStates[taskName]
	return state, ok
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
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_TMPDIR=") || strings.HasPrefix(kv, "NOMAD_SECRETS_DIR=") || strings.HasPrefix(kv, "NOMAD_ALLOC_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "TMUX_TMPDIR="+dir)
	env = append(env, "NOMAD_ALLOC_DIR="+dir)
	if secretsDir != "" {
		env = append(env, "NOMAD_SECRETS_DIR="+secretsDir)
	}
	return env
}
