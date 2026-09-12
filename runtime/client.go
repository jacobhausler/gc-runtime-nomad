package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
)

const (
	defaultTimeout  = 10 * time.Second
	blockingWait    = 5 * time.Second
	execTimeout     = 30 * time.Second
	maxResponseByte = 1 << 20

	// createdAtMetaKey carries the provider's dispatch timestamp through
	// Nomad's durable child-job metadata. The jobs-list response exposes the
	// session attribution metadata but no creation timestamp, so recovery needs
	// this non-secret value to preserve the adoption-age boundary after a local
	// sidecar loss.
	createdAtMetaKey = "gc_created_at"
)

// errJobGone marks a 404 from the Nomad API against a job route — the
// confirmed-absence signal (04 §2.1 rule 7): a DEFAULT-consistency 404 on a
// direct job GET, or (for deregister/dispatch) the job never having existed.
var errJobGone = errors.New("nomad: job gone")

// errJobForbidden marks a 403 from the Nomad API against a job route — the
// lab ACL model's (04 §4) signal that the calling token lacks a capability
// it needs for the attempted call, most notably submit-job on a register
// call from a token scoped to dispatch/read/deregister only
// (NRT-P2-06.1). registerJob's caller uses this to turn a bare 403 into an
// error that names the missing capability instead of an opaque status code.
var errJobForbidden = errors.New("nomad: forbidden")

// errAllocFileGone marks a 404 from the client fs "cat" endpoint — either
// the allocation or the requested path is gone/never existed. The
// stop-path egress treats this as "nothing to copy" rather than a failure:
// not every allocation produces every file.
var errAllocFileGone = errors.New("nomad: alloc file gone")

// client speaks the subset of the Nomad API the lifecycle ops need. The
// native SDK is the production implementation for the blocking allocation
// query and dispatch write; the remaining routes stay on the existing
// transport until their independently reversible adapter slices land. This
// keeps the pack's zero-gascity-imports contract while using Nomad's native
// client boundary.
type client struct {
	addr      *url.URL
	token     string
	namespace string
	http      *http.Client
	nomad     *nomadapi.Client
	tls       *tls.Config
}

func newClient(addr, token, namespace string) (*client, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, fmt.Errorf("nomad API address is required (set %s)", envAddr)
	}
	parsed, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("parsing nomad API address: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("nomad API address must be an absolute URL, got %q", addr)
	}
	if namespace == "" {
		namespace = "default"
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("default HTTP transport is %T, want *http.Transport", http.DefaultTransport)
	}
	transport := base.Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		if transport.TLSClientConfig.MinVersion == 0 {
			transport.TLSClientConfig.MinVersion = tls.VersionTLS12
		}
	}
	config := nomadapi.DefaultConfig()
	if config.TLSConfig == nil {
		config.TLSConfig = &nomadapi.TLSConfig{}
	}
	applyGCNomadTLSConfig(config.TLSConfig)
	baseHTTPClient := &http.Client{Transport: transport}
	if err := nomadapi.ConfigureTLS(baseHTTPClient, config.TLSConfig); err != nil {
		return nil, fmt.Errorf("configuring nomad TLS: %w", err)
	}
	tlsConfig := transport.TLSClientConfig.Clone()
	if w := tlsVerificationWarning(tlsConfig); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}
	httpClient := &http.Client{
		Timeout:   defaultTimeout,
		Transport: &nomadWriteMetaCompatTransport{base: transport},
	}
	sdkHTTPClient := &http.Client{
		Timeout:   defaultTimeout,
		Transport: &nomadWriteMetaCompatTransport{base: transport},
	}
	config.Address = parsed.String()
	config.SecretID = token
	config.Namespace = namespace
	config.HttpClient = sdkHTTPClient
	nomadClient, err := nomadapi.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("creating nomad API client: %w", err)
	}
	return &client{
		addr:      parsed,
		token:     token,
		namespace: namespace,
		http:      httpClient,
		nomad:     nomadClient,
		tls:       tlsConfig,
	}, nil
}

// applyGCNomadTLSConfig adds the pack-prefixed aliases without changing the
// Nomad SDK's own environment contract. GC_NOMAD_* wins when both forms are
// present, which lets a supervisor pin the runtime's cluster credentials while
// leaving unrelated NOMAD_* settings in the parent environment untouched.
func applyGCNomadTLSConfig(config *nomadapi.TLSConfig) {
	config.CACert = envAlias("GC_NOMAD_CACERT", "NOMAD_CACERT", config.CACert)
	config.ClientCert = envAlias("GC_NOMAD_CLIENT_CERT", "NOMAD_CLIENT_CERT", config.ClientCert)
	config.ClientKey = envAlias("GC_NOMAD_CLIENT_KEY", "NOMAD_CLIENT_KEY", config.ClientKey)
	config.TLSServerName = envAlias("GC_NOMAD_TLS_SERVER_NAME", "NOMAD_TLS_SERVER_NAME", config.TLSServerName)
}

// tlsVerificationWarning reports the operator warning owed for a transport
// that skips certificate verification, or "" when verification is on.
//
// The pack deliberately does NOT alias NOMAD_SKIP_VERIFY the way it aliases
// the other four TLS knobs: there is no legitimate reason for a runtime to
// offer its own spelling of "turn verification off". But the SDK's
// DefaultConfig honours the variable regardless, and the resulting config is
// cloned onto the client and used by the alloc-exec WebSocket dialer — which
// is how every staged secret reaches a box. Verification being off on that
// channel is a thing an operator must be told, every time, rather than a
// thing they discover.
func tlsVerificationWarning(cfg *tls.Config) string {
	if cfg == nil || !cfg.InsecureSkipVerify {
		return ""
	}
	return tlsVerificationDisabledWarning
}

// tlsVerificationDisabledWarning is the single wording, shared by the client
// construction path and by the `check` pre-flight, so the two can never
// diverge.
const tlsVerificationDisabledWarning = "warning: TLS certificate verification is DISABLED " +
	"for the Nomad API and for the alloc-exec channel that carries staged secrets " +
	"(NOMAD_SKIP_VERIFY is set in this environment). Unset it, or accept that secrets " +
	"are staged over an unauthenticated TLS connection."

// skipVerifyRequested reports whether the environment asks the Nomad SDK to
// skip verification. `check` builds no client, so it reads the variable the
// SDK itself reads (DefaultConfig, api.go) rather than inspecting a transport.
func skipVerifyRequested() bool {
	v := strings.TrimSpace(os.Getenv("NOMAD_SKIP_VERIFY"))
	if v == "" {
		return false
	}
	insecure, err := strconv.ParseBool(v)
	return err == nil && insecure
}

func envAlias(primary, fallback, current string) string {
	if value := os.Getenv(primary); value != "" {
		return value
	}
	if value := os.Getenv(fallback); value != "" {
		return value
	}
	return current
}

// nomadWriteMetaCompatTransport keeps the unmodified fakenomad test double
// usable while the fake-contract slice is still pending. The fake mutates
// successfully but omits X-Nomad-Index on write responses; the native SDK
// requires that header to parse WriteMeta. Real Nomad supplies it. Restrict
// the compatibility value to successful writes so query metadata remains
// authoritative and transport failures are never turned into observations.
type nomadWriteMetaCompatTransport struct {
	base http.RoundTripper
}

func (t *nomadWriteMetaCompatTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	isDispatch := req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/dispatch")
	var requestBody []byte
	if isDispatch && req.Body != nil {
		var err error
		requestBody, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(requestBody))
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if isDispatch && resp.StatusCode == http.StatusNotFound {
		data, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		// fakenomad's pre-D4 router returns this exact response for the
		// native PUT method. A missing parent has a different error body,
		// so it is not retried and retains normal 404 semantics.
		if bytes.Equal(bytes.TrimSpace(data), []byte(`{"error":"not found"}`)) {
			retry := req.Clone(req.Context())
			retry.Method = http.MethodPost
			retry.Body = io.NopCloser(bytes.NewReader(requestBody))
			retry.ContentLength = int64(len(requestBody))
			return t.base.RoundTrip(retry)
		}
		resp.Body = io.NopCloser(bytes.NewReader(data))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/allocations") {
		data, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		resp.Body = io.NopCloser(bytes.NewReader(normalizeFakeAllocationTaskStates(data)))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.Header.Get("X-Nomad-Index") == "" {
		switch req.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			resp.Header.Set("X-Nomad-Index", "0")
		}
	}
	return resp, nil
}

// normalizeFakeAllocationTaskStates adapts the pre-D4 fake's compact
// TaskStates map (task name -> state string) to the native SDK's allocation
// shape (task name -> TaskState object). Real Nomad already sends objects, so
// its response bytes pass through unchanged. Keeping this at the response
// boundary lets the fake remain unmodified until the separately scoped
// contract-expansion slice.
func normalizeFakeAllocationTaskStates(data []byte) []byte {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		return data
	}
	changed := false
	for _, row := range rows {
		raw, ok := row["TaskStates"]
		if !ok {
			continue
		}
		var states map[string]json.RawMessage
		if err := json.Unmarshal(raw, &states); err != nil {
			continue
		}
		rowChanged := false
		for name, state := range states {
			if first := bytes.TrimSpace(state); len(first) > 0 && first[0] == '"' {
				wrapped := make([]byte, 0, len(state)+10)
				wrapped = append(wrapped, []byte(`{"State":`)...)
				wrapped = append(wrapped, state...)
				wrapped = append(wrapped, '}')
				states[name] = json.RawMessage(wrapped)
				rowChanged = true
			}
		}
		if rowChanged {
			encoded, err := json.Marshal(states)
			if err != nil {
				return data
			}
			row["TaskStates"] = encoded
			changed = true
		}
	}
	if !changed {
		return data
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return data
	}
	return encoded
}

// registerJob upserts a job spec. Nomad's job register is idempotent, so
// callers can call this on every start without a check-then-act race. The
// namespace rides the Job body itself (Nomad honors Job.Namespace on this
// route), matching every other route's explicit ?namespace= query param.
func (c *client) registerJob(ctx context.Context, job nomadJob) error {
	return c.do(ctx, defaultTimeout, http.MethodPost, []string{"v1", "jobs"}, map[string]any{"Job": job}, nil)
}

// getJob reads jobID via GET /v1/job/:id (namespace-aware via nsQuery) and
// reports ok=false, err=nil only on a confirmed absence (404) — mirroring
// errJobGone's meaning elsewhere in this client; any other failure is
// returned as an error since a lookup failure is not proof of absence. It
// decodes NodePool and Meta: NodePool is the one parentJobSpec field beyond
// namespace that varies at runtime (NRT-P2-05), and Meta carries
// jobspecHashMetaKey (fnrt-t4l.9) — together the two signals
// lifecycle.ensureParentRegistered needs to decide whether an
// already-registered parent still matches this build's jobspec, not only its
// node pool. A namespace-scoped GET against real Nomad already proves the
// namespace half of that identity on its own — Nomad 404s a job that exists
// only in a different namespace rather than returning it.
func (c *client) getJob(ctx context.Context, jobID string) (nodePool string, meta map[string]string, ok bool, err error) {
	var out struct {
		NodePool string
		Meta     map[string]string
	}
	if _, err := c.doIndexed(ctx, defaultTimeout, http.MethodGet, []string{"v1", "job", jobID}, c.nsQuery(), nil, &out); err != nil {
		if errors.Is(err, errJobGone) {
			return "", nil, false, nil
		}
		return "", nil, false, err
	}
	return out.NodePool, out.Meta, true, nil
}

type dispatchResult struct {
	DispatchedJobID string
}

// nsQuery returns the ?namespace= query value every non-default-namespace
// route needs (NRT-P2-05: confirmed empirically against a real cluster —
// Nomad's dispatch/list/deregister/fs/exec routes resolve against namespace
// "default" unless a query param names the target namespace; there is no
// header fallback. fakenomad's single global job map never enforced
// namespace scoping, so this gap was invisible to the offline L1/L2 suites).
func (c *client) nsQuery() url.Values {
	return url.Values{"namespace": []string{c.namespace}}
}

// dispatchChild dispatches a child of parentID through Nomad's native write
// path. Meta remains non-secret attribution (04 §2.1: gc_session + a
// per-dispatch nonce — never a capability); the same intent is also carried
// in the native idempotency token so a retry after a lost response cannot
// create a second child.
func (c *client) dispatchChild(ctx context.Context, parentID, sessionName, nonce string) (string, error) {
	return c.dispatchChildAt(ctx, parentID, sessionName, nonce, time.Now().UTC())
}

// dispatchChildAt is dispatchChild with an explicit creation time so the
// timestamp committed in the local intent record is exactly the one carried
// in Nomad Meta and available to sidecar recovery.
func (c *client) dispatchChildAt(ctx context.Context, parentID, sessionName, nonce string, createdAt time.Time) (string, error) {
	ctx, cancel := clientContext(ctx, defaultTimeout)
	defer cancel()
	meta := map[string]string{"gc_session": sessionName, "gc_nonce": nonce}
	if !createdAt.IsZero() {
		meta[createdAtMetaKey] = createdAt.UTC().Format(time.RFC3339Nano)
	}
	options := (&nomadapi.WriteOptions{
		Namespace:        c.namespace,
		IdempotencyToken: dispatchIdempotencyToken(sessionName, nonce),
	}).WithContext(ctx)
	out, _, err := c.nomad.Jobs().Dispatch(
		parentID,
		meta,
		nil,
		"",
		options,
	)
	if err != nil {
		return "", err
	}
	if out == nil || out.DispatchedJobID == "" {
		return "", fmt.Errorf("nomad dispatch: empty DispatchedJobID in response")
	}
	return out.DispatchedJobID, nil
}

func dispatchIdempotencyToken(sessionName, nonce string) string {
	return sessionName + ":" + nonce
}

// childJob is one entry of a children-of-parent jobs list (04 §2.1 rule
// 2/3): the subset opListRunning and dispatch's positive-attribution
// adoption (rule 6) need to decode identity and filter non-terminal
// children — never the job ID itself, which is not invertible back to a
// session name (e2a-job-id-charset-gap).
type childJob struct {
	ID        string
	Namespace string
	Meta      map[string]string
	Terminal  bool
}

// listChildJobs lists every job dispatched from parentID via the Nomad jobs
// endpoint with `meta=true` (e2a-amend-jobs-list-params), filtering
// client-side on ParentID — real Nomad's jobs-list has no parent-filter
// param of its own. This is the children-of-parent mechanism both
// list-running's ListRunning(prefix) (04 §2.1 rule 2/3) and dispatch's
// orphan-adoption lookup (rule 6) read: meta decode, never ID-string
// parsing.
func (c *client) listChildJobs(ctx context.Context, parentID string) ([]childJob, error) {
	q := c.nsQuery()
	q.Set("meta", "true")
	var out []struct {
		ID        string
		ParentID  string
		Namespace string
		Status    string
		Meta      map[string]string
	}
	if _, err := c.doIndexed(ctx, defaultTimeout, http.MethodGet, []string{"v1", "jobs"}, q, nil, &out); err != nil {
		return nil, fmt.Errorf("listing nomad jobs: %w", err)
	}
	var children []childJob
	for _, j := range out {
		if j.ParentID != parentID {
			continue
		}
		children = append(children, childJob{
			ID:        j.ID,
			Namespace: j.Namespace,
			Meta:      j.Meta,
			Terminal:  j.Status != "running",
		})
	}
	return children, nil
}

// allocRecord is the subset of a Nomad allocation the lifecycle ops need.
type allocRecord struct {
	ID            string
	ClientStatus  string
	DesiredStatus string
	// TaskStates is Nomad's own per-task lifecycle record for this allocation,
	// keyed by task name. Nomad fills it in the moment an alloc exists, so a
	// task still in "Task Setup" reads "pending"/"starting" here while
	// ClientStatus already says the alloc is live -- which is the only signal
	// that separates "the box is placed" from "the task we are about to exec
	// into is up" (ops.go awaitExecAllocReady, cr-u9vul4). nil means this read
	// carried no task states at all (a stub read), which is NOT an answer.
	TaskStates map[string]allocTaskState
}

// allocTaskState is the slice of Nomad's AllocTaskState the readiness wait
// needs: the lifecycle state, plus a one-line rendering of the task's newest
// own event, so a readiness timeout can report why the alloc never started
// instead of a bare deadline.
type allocTaskState struct {
	State     string
	LastEvent string

	// FailureEvent is the task event that explains why this task did not run,
	// which is NOT the same as the last one. Nomad appends terminal bookkeeping
	// after the real cause: on a missing image the event list ends
	//   ... "Failed to pull `img`: ... repository does not exist"
	//   ... "Policy allows no restarts"
	// so LastEvent reports the restart policy and says nothing about the image.
	// Measured live on T6 (cr-kkcvmd), 2026-09-11. Empty when no event in the
	// list carries a failure detail.
	FailureEvent string
}

// listAllocsForJob reads the allocations placed for jobID. If wait > 0 and
// sinceIndex > 0, it performs a blocking read (index/wait query params) that
// resolves as soon as the server's index advances past sinceIndex, or wait
// elapses — the "confirm terminal" mechanism 04 §4/§6 call for around stop
// and duplicate-start detection.
func (c *client) listAllocsForJob(ctx context.Context, jobID string, sinceIndex uint64, wait time.Duration) ([]allocRecord, uint64, error) {
	ctx, cancel := clientContext(ctx, blockingTimeout(wait))
	defer cancel()
	options := (&nomadapi.QueryOptions{
		Namespace: c.namespace,
		WaitIndex: sinceIndex,
		WaitTime:  wait,
	}).WithContext(ctx)
	allocs, meta, err := c.nomad.Jobs().Allocations(jobID, false, options)
	if err != nil {
		return nil, 0, err
	}
	var idx uint64
	if meta != nil {
		idx = meta.LastIndex
	}
	out := make([]allocRecord, 0, len(allocs))
	for _, alloc := range allocs {
		if alloc == nil {
			continue
		}
		out = append(out, allocRecord{
			ID:            alloc.ID,
			ClientStatus:  alloc.ClientStatus,
			DesiredStatus: alloc.DesiredStatus,
			TaskStates:    allocTaskStatesOf(alloc),
		})
	}
	return out, idx, nil
}

// allocTaskStatesOf lifts an allocation's per-task lifecycle record into the
// pack's own shape. Nomad's full AllocTaskState also carries timestamps,
// restart counts and the whole event history; the readiness wait needs the
// state string and, so a timeout can explain itself, the latest event --
// nothing larger. The list read the pack already makes answers for both
// Nomad shapes here (a list stub and a full allocation both carry
// TaskStates), so the wait never needs a second endpoint. A read with no
// usable task states yields nil so a caller can tell "the API did not answer"
// apart from "the task is not up".
func allocTaskStatesOf(alloc *nomadapi.AllocationListStub) map[string]allocTaskState {
	if len(alloc.TaskStates) == 0 {
		return nil
	}
	states := make(map[string]allocTaskState, len(alloc.TaskStates))
	for name, state := range alloc.TaskStates {
		if state == nil {
			continue
		}
		states[name] = allocTaskState{
			State:        state.State,
			LastEvent:    latestTaskEvent(state.Events),
			FailureEvent: failureTaskEvent(state.Events),
		}
	}
	if len(states) == 0 {
		return nil
	}
	return states
}

// latestTaskEvent renders the newest of a task's own event trail (Nomad's
// "Received" / "Task Setup" / "Started" / "Setup Failure" records) as the
// reason a readiness timeout reports, newest-first because Nomad appends.
// Empty when the task has no events yet.
func latestTaskEvent(events []*nomadapi.TaskEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e == nil {
			continue
		}
		detail := firstNonEmpty(e.DisplayMessage, e.Message, e.SetupError, e.DriverError, e.DownloadError, e.KillReason)
		if detail == "" {
			return e.Type
		}
		return e.Type + ": " + detail
	}
	return ""
}

// failureTaskEvent finds the event that explains a task's failure, scanning
// newest-first for one that carries a driver-level detail rather than taking the
// last event outright. Nomad appends terminal bookkeeping ("Policy allows no
// restarts", "Task Setup") AFTER the real cause, so the newest event is usually
// the least informative one in the list — which is exactly how a missing image
// came out as EOF with no image name anywhere (T6, cr-kkcvmd).
//
// Only events carrying a real error field qualify: DriverError, DownloadError,
// SetupError and KillReason. A DisplayMessage alone does not, because every
// routine lifecycle event has one and matching on those would make any ordinary
// "Task Setup" look like a cause.
func failureTaskEvent(events []*nomadapi.TaskEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e == nil {
			continue
		}
		detail := firstNonEmpty(e.DriverError, e.DownloadError, e.SetupError, e.KillReason)
		if detail == "" {
			continue
		}
		if strings.TrimSpace(e.Type) == "" {
			return detail
		}
		return e.Type + ": " + detail
	}
	return ""
}

// firstNonEmpty returns the first value that is not blank, or "".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// deregisterJob deregisters jobID and returns the response index (0 if the
// job was already gone). purge=false matches the stop-without-purge
// ordering invariant (04 §3 stop row, R1c-05); a 404 (already gone) is
// folded to success so stop stays idempotent.
func (c *client) deregisterJob(ctx context.Context, jobID string, purge bool) (uint64, error) {
	parts := []string{"v1", "job", jobID}
	q := c.nsQuery()
	if purge {
		q.Set("purge", "true")
	}
	var out struct {
		JobModifyIndex uint64
	}
	_, err := c.doIndexed(ctx, defaultTimeout, http.MethodDelete, parts, q, nil, &out)
	if errors.Is(err, errJobGone) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return out.JobModifyIndex, nil
}

// readAllocFile reads one file out of allocID's filesystem via Nomad's
// client fs "cat" endpoint (GET /v1/client/fs/cat/:allocID?path=...) — the
// stop-path egress read that must complete before a session's job is
// deregistered, since deregister is what makes the allocation's files
// unreachable. Returns errAllocFileGone on a 404 (alloc or path absent).
func (c *client) readAllocFile(ctx context.Context, allocID, path string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	q := c.nsQuery()
	q.Set("path", path)
	target := c.urlFor([]string{"v1", "client", "fs", "cat", allocID}, q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building nomad fs-cat request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	if c.token != "" {
		req.Header.Set("X-Nomad-Token", c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nomad fs-cat request: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseByte))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("reading nomad fs-cat response: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("closing nomad fs-cat response: %w", closeErr)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errAllocFileGone
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("nomad %s: status %d: %s", target.String(), resp.StatusCode, statusText(resp.StatusCode, data))
	}
	return data, nil
}

func blockingTimeout(wait time.Duration) time.Duration {
	if wait <= 0 {
		return defaultTimeout
	}
	return wait + defaultTimeout
}

func clientContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// execFrame decodes the three alloc-exec WebSocket frame shapes this client
// reads: a stdout data chunk, a stderr data chunk, and the terminal
// exited/exit_code frame (e2a-exec-protocol). stderr is collected only for
// callers that asked for it (execAllocObserved); the plain stream callers keep
// stdout-only semantics so nothing that matches on command output — peek's
// pane capture above all — starts seeing box diagnostics spliced into it.
type execFrame struct {
	Stdout *struct {
		Data string `json:"data"`
	} `json:"stdout,omitempty"`
	Stderr *struct {
		Data string `json:"data"`
	} `json:"stderr,omitempty"`
	Exited bool `json:"exited,omitempty"`
	Result struct {
		ExitCode int `json:"exit_code"`
	} `json:"result,omitempty"`
}

// execAlloc runs command inside task of allocID over the Nomad alloc-exec
// WebSocket (GET /v1/client/allocation/:alloc_id/exec, e2a-exec-protocol)
// and returns the remote command's exit code plus its collected stdout. No
// stdin is sent — every caller (launch/relaunch's tmux command, driving-verb
// probes) needs none — so the stdin channel is closed immediately, which is
// enough to make both fakenomad and real Nomad run the command and report
// its result.
func (c *client) execAlloc(ctx context.Context, allocID, task string, command []string) (int, []byte, error) {
	return c.execAllocStdin(ctx, allocID, task, command, nil)
}

// execStdinChunkSize bounds each stdin data frame so a large workspace/
// secrets tar (NRT-P1-06) is streamed rather than built as one oversized
// WebSocket frame.
const execStdinChunkSize = 32 * 1024

// execAllocStdin is execAlloc's stdin-carrying twin: it writes stdin (if
// any) as one or more base64 data frames before the stdin-close frame that
// makes both fakenomad and real Nomad actually run the command — the
// tar-over-exec-stdin channel NRT-P1-06's workspace/secrets staging rides.
func (c *client) execAllocStdin(ctx context.Context, allocID, task string, command []string, stdin []byte) (int, []byte, error) {
	exitCode, stdout, _, err := c.execAllocObserved(ctx, allocID, task, command, stdin)
	return exitCode, stdout, err
}

// execAllocObserved is execAllocStdin's twin for the callers that have to know
// WHY the box said no: it returns the command's stderr alongside its stdout.
// The launch handshake is the reason this exists (cr-86ytiq) — gc's exec
// transport reports only "tmux exited N", so a pane that dies before the
// bootstrap runs is otherwise unobservable from the controller side, and the
// box's own diagnostic was being dropped here at the frame codec.
func (c *client) execAllocObserved(ctx context.Context, allocID, task string, command []string, stdin []byte) (int, []byte, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := c.dialExecWS(ctx, allocID, task, command)
	if err != nil {
		return 0, nil, nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(execTimeout))

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()

	for len(stdin) > 0 {
		chunk := stdin
		if len(chunk) > execStdinChunkSize {
			chunk = chunk[:execStdinChunkSize]
		}
		dataFrame, err := json.Marshal(map[string]any{"stdin": map[string]any{"data": base64.StdEncoding.EncodeToString(chunk)}})
		if err != nil {
			return 0, nil, nil, fmt.Errorf("encoding exec stdin-data frame: %w", err)
		}
		if err := wsWriteMaskedFrame(conn, wsOpText, dataFrame); err != nil {
			return 0, nil, nil, fmt.Errorf("writing exec stdin-data frame: %w", err)
		}
		stdin = stdin[len(chunk):]
	}

	closeFrame, err := json.Marshal(map[string]any{"stdin": map[string]any{"close": true}})
	if err != nil {
		return 0, nil, nil, fmt.Errorf("encoding exec stdin-close frame: %w", err)
	}
	if err := wsWriteMaskedFrame(conn, wsOpText, closeFrame); err != nil {
		return 0, nil, nil, fmt.Errorf("writing exec stdin-close frame: %w", err)
	}

	var stdout, stderr bytes.Buffer
	for {
		opcode, payload, err := wsReadFrame(conn)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("reading exec frame: %w", err)
		}
		if opcode != wsOpText {
			continue
		}
		var msg execFrame
		if err := json.Unmarshal(payload, &msg); err != nil {
			return 0, nil, nil, fmt.Errorf("decoding exec frame: %w", err)
		}
		if msg.Stdout != nil && msg.Stdout.Data != "" {
			decoded, err := base64.StdEncoding.DecodeString(msg.Stdout.Data)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("decoding exec stdout: %w", err)
			}
			stdout.Write(decoded)
		}
		if msg.Stderr != nil && msg.Stderr.Data != "" {
			decoded, err := base64.StdEncoding.DecodeString(msg.Stderr.Data)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("decoding exec stderr: %w", err)
			}
			stderr.Write(decoded)
		}
		if msg.Exited {
			return msg.Result.ExitCode, stdout.Bytes(), stderr.Bytes(), nil
		}
	}
}

// wsConn is the subset of net.Conn the exec frame codec needs, satisfied by
// the raw connection returned from dialExecWS after its handshake.
type wsConn interface {
	io.ReadWriteCloser
	SetDeadline(t time.Time) error
}

// bufferedConn wraps a net.Conn whose handshake was read through a
// *bufio.Reader, so any bytes the reader buffered past the handshake
// headers (frame data arriving in the same TCP segment) are not lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// dialExecWS dials allocID's exec endpoint and performs the client side of
// the RFC 6455 handshake, returning a connection positioned to read/write
// exec frames.
func (c *client) dialExecWS(ctx context.Context, allocID, task string, command []string) (wsConn, error) {
	cmdJSON, err := json.Marshal(command)
	if err != nil {
		return nil, fmt.Errorf("encoding exec command: %w", err)
	}
	q := c.nsQuery()
	q.Set("command", string(cmdJSON))
	q.Set("task", task)
	q.Set("tty", "false")

	host := c.addr.Host
	var d net.Dialer
	var conn net.Conn
	if c.addr.Scheme == "https" {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: c.tls}).DialContext(ctx, "tcp", host)
	} else {
		conn, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, fmt.Errorf("dialing nomad exec websocket: %w", err)
	}

	keyBuf := make([]byte, 16)
	if _, err := rand.Read(keyBuf); err != nil {
		conn.Close()
		return nil, fmt.Errorf("generating websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBuf)

	base := strings.TrimRight(c.addr.Path, "/")
	path := base + "/v1/client/allocation/" + allocID + "/exec?" + q.Encode()

	var req bytes.Buffer
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&req, "Host: %s\r\n", host)
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", key)
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	if c.token != "" {
		fmt.Fprintf(&req, "X-Nomad-Token: %s\r\n", c.token)
	}
	req.WriteString("\r\n")

	if _, err := conn.Write(req.Bytes()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("writing websocket handshake: %w", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading websocket handshake status: %w", err)
	}
	if !strings.Contains(statusLine, "101") {
		conn.Close()
		return nil, fmt.Errorf("nomad exec websocket handshake failed: %s", strings.TrimSpace(statusLine))
	}

	var acceptKey string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("reading websocket handshake headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r\n"), ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Sec-WebSocket-Accept") {
			acceptKey = strings.TrimSpace(v)
		}
	}
	if acceptKey != wsAcceptKey(key) {
		conn.Close()
		return nil, fmt.Errorf("nomad exec websocket handshake: Sec-WebSocket-Accept mismatch")
	}

	return &bufferedConn{Conn: conn, r: br}, nil
}

func (c *client) do(ctx context.Context, timeout time.Duration, method string, parts []string, body, out any) error {
	_, err := c.doIndexed(ctx, timeout, method, parts, nil, body, out)
	return err
}

// doIndexed performs one Nomad API request and returns the X-Nomad-Index
// response header (0 if absent).
func (c *client) doIndexed(ctx context.Context, timeout time.Duration, method string, parts []string, query url.Values, body, out any) (uint64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshaling nomad request: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	target := c.urlFor(parts, query)
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return 0, fmt.Errorf("building nomad request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("X-Nomad-Token", c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("nomad request: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseByte))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return 0, fmt.Errorf("reading nomad response: %w", readErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("closing nomad response: %w", closeErr)
	}

	var idx uint64
	if raw := resp.Header.Get("X-Nomad-Index"); raw != "" {
		idx, _ = strconv.ParseUint(raw, 10, 64)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return idx, statusError(resp.StatusCode, target.String(), data)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return idx, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return idx, fmt.Errorf("decoding nomad response: %w", err)
	}
	return idx, nil
}

// urlFor joins parts onto the API base path. It assigns to u.Path (the
// decoded form) rather than pre-escaping each part: dispatched child job
// IDs legitimately contain a literal "/" (e2a-child-job-naming, mirrored in
// fakenomad's own routing comment), and url.URL.String() escapes u.Path
// correctly on its own — escaping parts individually first would encode
// that literal "/" to "%2F" and then double-escape it when the URL is
// serialized, corrupting the path the server routes on.
func (c *client) urlFor(parts []string, query url.Values) *url.URL {
	u := *c.addr
	base := strings.TrimRight(u.Path, "/")
	u.Path = base + "/" + strings.Join(parts, "/")
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	} else {
		u.RawQuery = ""
	}
	return &u
}

func statusError(status int, target string, data []byte) error {
	msg := statusText(status, data)
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s: %s", errJobGone, target, msg)
	case http.StatusForbidden:
		return fmt.Errorf("%w: %s: %s", errJobForbidden, target, msg)
	}
	return fmt.Errorf("nomad %s: status %d: %s", target, status, msg)
}

func statusText(status int, data []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &payload); err == nil && payload.Error != "" {
		return payload.Error
	}
	if text := strings.TrimSpace(string(data)); text != "" {
		return text
	}
	return http.StatusText(status)
}
