// Package fakenomad is an in-memory stand-in for the subset of the Nomad
// HTTP API a Gas City Nomad runtime provider calls: job register/dispatch,
// job and allocation reads (both blocking-capable via the index/wait query
// params against an X-Nomad-Index response header, per the Nomad consistency
// model), the interactive alloc-exec WebSocket, and the system GC endpoint.
// It exists so `gc runtime check`/`gc runtime conformance` (and this pack's
// own tests) can exercise the full wire contract without a live Nomad
// cluster. It implements exactly the endpoint families NRT-P1-02 scopes:
// jobs, dispatch, job-read, allocations, alloc-exec-WS, system — and fault
// hooks (FailNext) for scripted failure injection (L2 in the test pyramid).
//
// Out of scope (by design, per the epic's NRT-P1-02 out-of-scope note):
// fidelity beyond the endpoints a provider calls, and real WS TLS.
package fakenomad

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// fault is a one-shot scripted failure matched by exact method+path.
type fault struct {
	method string
	path   string
	status int
	body   string
}

// Server is a fake Nomad API server. Zero value is not usable; construct
// with NewServer. Safe for concurrent use.
type Server struct {
	mu      sync.Mutex
	index   uint64
	waitCh  chan struct{}
	jobs    map[string]*Job
	allocs  map[string]*Allocation
	faults  []fault
	dispSeq uint64

	httpSrv *httptest.Server
}

// NewServer starts a fake Nomad server on a loopback listener and returns
// it. Call Close when done.
func NewServer() *Server {
	s := &Server{
		waitCh: make(chan struct{}),
		jobs:   map[string]*Job{},
		allocs: map[string]*Allocation{},
	}
	s.httpSrv = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	return s
}

// URL returns the fake server's base URL (scheme+host).
func (s *Server) URL() string { return s.httpSrv.URL }

// Close shuts down the underlying HTTP server.
func (s *Server) Close() { s.httpSrv.Close() }

// FailNext queues a one-shot fault: the next request matching method and
// exact path is answered with status and body instead of being routed to
// the normal handler. Matches are consumed in FIFO order.
func (s *Server) FailNext(method, path string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, fault{method: method, path: path, status: status, body: body})
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
			s.faults = append(s.faults[:i], s.faults[i+1:]...)
			return f, true
		}
	}
	return fault{}, false
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if f, ok := s.takeFault(r.Method, r.URL.Path); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		if f.body != "" {
			_, _ = w.Write([]byte(f.body))
		}
		return
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
		default:
			writeJSONError(w, http.StatusNotFound, "not found")
		}
		return
	}

	switch {
	case r.Method == http.MethodPost && len(parts) == 2 && parts[0] == "v1" && parts[1] == "jobs":
		s.registerJob(w, r, "")
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "v1" && parts[1] == "allocations":
		s.listAllocs(w, r)
	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "v1" && parts[1] == "allocation":
		s.readAlloc(w, r, parts[2])
	case r.Method == http.MethodGet && len(parts) == 5 && parts[0] == "v1" && parts[1] == "client" && parts[2] == "allocation" && parts[4] == "exec":
		s.handleExecWS(w, r, parts[3])
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
	s.jobs[id] = &Job{ID: id, Namespace: ns, Meta: req.Job.Meta, ModifyIndex: idx}
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
	s.jobs[childID] = &Job{ID: childID, Namespace: parent.Namespace, ParentID: parentID, Dispatched: true, Meta: req.Meta, ModifyIndex: idx}

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

func (s *Server) systemGC(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
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
