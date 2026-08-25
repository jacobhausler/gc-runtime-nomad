package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gc-runtime-nomad/fakenomad"
)

// newTestLifecycle points a lifecycle at a fresh fakenomad server and a
// fresh sidecar directory under t.TempDir().
func newTestLifecycle(t *testing.T) (*lifecycle, *fakenomad.Server) {
	t.Helper()
	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)

	c, err := newClient(srv.URL(), "", "default")
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	sc, err := newSidecar(filepath.Join(t.TempDir(), "sidecar"))
	if err != nil {
		t.Fatalf("newSidecar: %v", err)
	}
	return &lifecycle{client: c, sidecar: sc, parentJobID: "gc-sessions"}, srv
}

// TestLifecycleRoundTrip exercises the exact shape `gc runtime check`'s
// required lifecycle round-trip drives: start observes running=true,
// duplicate start is rejected with an "already exists" stderr phrase (04 §6
// wire-contract constant), stop is idempotent, and is-running flips to
// false after stop.
func TestLifecycleRoundTrip(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-1"

	if running, err := l.opIsRunning(ctx, session); err != nil || running {
		t.Fatalf("is-running before start = (%v, %v), want (false, nil)", running, err)
	}

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}

	running, err := l.opIsRunning(ctx, session)
	if err != nil || !running {
		t.Fatalf("is-running after start = (%v, %v), want (true, nil)", running, err)
	}

	err = l.opStart(ctx, session)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate start error = %v, want an \"already exists\" error", err)
	}

	if err := l.opStop(ctx, session); err != nil {
		t.Fatalf("stop: %v", err)
	}

	running, err = l.opIsRunning(ctx, session)
	if err != nil || running {
		t.Fatalf("is-running after stop = (%v, %v), want (false, nil)", running, err)
	}

	// Stop is idempotent: a second stop on an already-stopped session
	// succeeds (E1a §6.1).
	if err := l.opStop(ctx, session); err != nil {
		t.Fatalf("second stop (idempotency) = %v, want nil", err)
	}

	// Stop on a session that was never started is also a success no-op.
	if err := l.opStop(ctx, "never-started"); err != nil {
		t.Fatalf("stop on never-started session = %v, want nil", err)
	}
}

// TestOpStartAfterStopRedispatches confirms a session can be restarted once
// its prior child job has reached a terminal state via stop.
func TestOpStartAfterStopRedispatches(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-restart"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := l.opStop(ctx, session); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("second start after stop: %v", err)
	}
	running, err := l.opIsRunning(ctx, session)
	if err != nil || !running {
		t.Fatalf("is-running after restart = (%v, %v), want (true, nil)", running, err)
	}
}

// TestOpListRunning drives multiple sessions through start/stop and checks
// list-running reflects exactly the still-running set, sorted.
func TestOpListRunning(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()

	for _, s := range []string{"charlie", "alpha", "bravo"} {
		if err := l.opStart(ctx, s); err != nil {
			t.Fatalf("start %q: %v", s, err)
		}
	}
	if err := l.opStop(ctx, "bravo"); err != nil {
		t.Fatalf("stop bravo: %v", err)
	}

	names, err := l.opListRunning(ctx, "")
	if err != nil {
		t.Fatalf("list-running: %v", err)
	}
	want := []string{"alpha", "charlie"}
	if len(names) != len(want) {
		t.Fatalf("list-running = %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("list-running = %v, want %v", names, want)
		}
	}
}

// TestOpListRunningEmpty confirms an empty sidecar directory (no sessions
// ever started) yields an empty, non-error list.
func TestOpListRunningEmpty(t *testing.T) {
	l, _ := newTestLifecycle(t)
	names, err := l.opListRunning(context.Background(), "")
	if err != nil {
		t.Fatalf("list-running: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("list-running = %v, want empty", names)
	}
}

// TestOpListRunningFiltersByPrefix confirms ListRunning(prefix) (04 §2.1
// rule 3) filters the DECODED session names to the given prefix rather than
// over-reporting every launched session regardless of what was asked for.
func TestOpListRunningFiltersByPrefix(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()

	for _, s := range []string{"city-a-sess1", "city-a-sess2", "city-b-sess1"} {
		if err := l.opStart(ctx, s); err != nil {
			t.Fatalf("start %q: %v", s, err)
		}
	}

	names, err := l.opListRunning(ctx, "city-a-")
	if err != nil {
		t.Fatalf("list-running: %v", err)
	}
	want := []string{"city-a-sess1", "city-a-sess2"}
	if len(names) != len(want) {
		t.Fatalf("list-running(prefix) = %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("list-running(prefix) = %v, want %v", names, want)
		}
	}

	if names, err := l.opListRunning(ctx, "no-such-prefix-"); err != nil || len(names) != 0 {
		t.Fatalf("list-running(no-such-prefix) = (%v, %v), want (empty, nil)", names, err)
	}
}

// TestOpListRunningUsesChildrenOfParentNotStaleSidecar confirms list-running
// enumerates the children-of-parent jobs list (04 §2.1 rule 2/3) rather than
// trusting the sidecar as the existence source: a binding whose child job
// was deregistered out-of-band (never going through opStop, so the sidecar
// binding itself is untouched) must NOT be reported as running — the
// children list, not the sidecar, is what says the child went terminal.
func TestOpListRunningUsesChildrenOfParentNotStaleSidecar(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-stale-binding"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("load binding: (%v, %v, %v)", b, ok, err)
	}

	// Deregister the child job directly against the fake, out-of-band from
	// opStop, leaving the sidecar binding (Launched=true) untouched — the
	// exact drift the children-of-parent list must not over-report through.
	if _, err := l.client.deregisterJob(ctx, b.ChildJobID, false); err != nil {
		t.Fatalf("deregisterJob: %v", err)
	}

	names, err := l.opListRunning(ctx, "")
	if err != nil {
		t.Fatalf("list-running: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("list-running after out-of-band deregister = %v, want empty (stale sidecar binding must not over-report)", names)
	}
}

// TestOpProvisionThenExecWithoutLaunch is the RPP-PROVISION-001 acceptance
// check: after provision (no launch), is-running is false, list-running
// omits the session, YET the box already answers exec — proving the
// launched marker, not mere alloc existence, is what is-running keys off
// (04 §6 decision table).
func TestOpProvisionThenExecWithoutLaunch(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-provisioned"

	if err := l.opProvision(ctx, session); err != nil {
		t.Fatalf("provision: %v", err)
	}

	if running, err := l.opIsRunning(ctx, session); err != nil || running {
		t.Fatalf("is-running after provision = (%v, %v), want (false, nil)", running, err)
	}
	names, err := l.opListRunning(ctx, "")
	if err != nil {
		t.Fatalf("list-running: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("list-running after provision = %v, want empty", names)
	}

	exitCode, out, err := l.opExec(ctx, session, []string{"echo", "hi"})
	if err != nil {
		t.Fatalf("exec on provisioned-not-launched session: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exec exit code = %d, want 0", exitCode)
	}
	if len(out) == 0 {
		t.Fatalf("exec stdout = empty, want fakenomad's scripted reply")
	}

	// provision is idempotent-rejecting exactly like start: a second
	// provision on the same live (non-terminal, unlaunched) box is a
	// duplicate.
	if err := l.opProvision(ctx, session); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate provision error = %v, want an \"already exists\" error", err)
	}
}

// TestOpStartLaunchesOverExec confirms opStart's launch half actually goes
// over the alloc-exec WebSocket (is-running only flips true once that
// succeeds — TestLifecycleRoundTrip covers the is-running assertion; this
// test additionally confirms exec still answers post-launch).
func TestOpStartLaunchesOverExec(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-launched"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	if running, err := l.opIsRunning(ctx, session); err != nil || !running {
		t.Fatalf("is-running after start = (%v, %v), want (true, nil)", running, err)
	}
	if _, _, err := l.opExec(ctx, session, []string{"true"}); err != nil {
		t.Fatalf("exec after start: %v", err)
	}
}

// TestOpRelaunchReusesTheSameAlloc drives the warm-relaunch path (04 §7):
// provision, relaunch (no prior opStart), and confirm the child job ID is
// unchanged — i.e. relaunch never re-dispatches — while is-running flips
// true exactly like a full start would.
func TestOpRelaunchReusesTheSameAlloc(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-relaunch"

	if err := l.opProvision(ctx, session); err != nil {
		t.Fatalf("provision: %v", err)
	}
	before, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("sidecar.load before relaunch = (%v, %v, %v)", before, ok, err)
	}

	if err := l.opRelaunch(ctx, session); err != nil {
		t.Fatalf("relaunch: %v", err)
	}

	after, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("sidecar.load after relaunch = (%v, %v, %v)", after, ok, err)
	}
	if after.ChildJobID != before.ChildJobID {
		t.Fatalf("relaunch child job ID = %q, want unchanged %q (no re-dispatch)", after.ChildJobID, before.ChildJobID)
	}
	if !after.Launched {
		t.Fatalf("relaunch did not set the launched marker")
	}
	if running, err := l.opIsRunning(ctx, session); err != nil || !running {
		t.Fatalf("is-running after relaunch = (%v, %v), want (true, nil)", running, err)
	}
}

// TestOpRelaunchWithoutABoxFails confirms relaunch on a session with no
// provisioned box (never started/provisioned) fails rather than silently
// dispatching one — relaunch never re-dispatches (04 §7).
func TestOpRelaunchWithoutABoxFails(t *testing.T) {
	l, _ := newTestLifecycle(t)
	if err := l.opRelaunch(context.Background(), "never-provisioned"); err == nil {
		t.Fatalf("relaunch on never-provisioned session: got nil error, want failure")
	}
}

// TestOpExecOnUnprovisionedSessionFails confirms exec on a session with no
// binding at all fails cleanly rather than panicking or dialing nothing.
func TestOpExecOnUnprovisionedSessionFails(t *testing.T) {
	l, _ := newTestLifecycle(t)
	if _, _, err := l.opExec(context.Background(), "never-provisioned", []string{"echo", "hi"}); err == nil {
		t.Fatalf("exec on never-provisioned session: got nil error, want failure")
	}
}

// TestOpStartFaultPropagates confirms a scripted dispatch fault surfaces as
// an error rather than a silently-successful start (fakenomad's L2 fault
// injection, driven through the ops layer).
func TestOpStartFaultPropagates(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-fault"

	// The parent job must exist before the dispatch call this start makes,
	// so fault-inject the dispatch endpoint specifically.
	if err := l.client.registerJob(ctx, parentJobSpec("default", l.nodePool, l.parentJobID)); err != nil {
		t.Fatalf("registerJob: %v", err)
	}
	srv.FailNext("POST", "/v1/job/"+l.parentJobID+"/dispatch", 500, `{"error":"injected"}`)

	if err := l.opStart(ctx, session); err == nil {
		t.Fatalf("start with faulted dispatch: got nil error, want failure")
	}

	if running, err := l.opIsRunning(ctx, session); err != nil || running {
		t.Fatalf("is-running after failed start = (%v, %v), want (false, nil)", running, err)
	}
}

// countFSCat counts client fs-cat requests in a fakenomad trace.
func countFSCat(trace []string) int {
	n := 0
	for _, req := range trace {
		if strings.HasPrefix(req, "GET /v1/client/fs/cat/") {
			n++
		}
	}
	return n
}

// TestOpStopEgressesBeforeDeregister drives the NRT-P1-07 acceptance: with
// egress configured, stop reads every allocation's transcript/evidence
// files via the fs API, lands them under the sink directory, and the fake's
// request trace shows the fs-cat read(s) completing strictly before the
// deregister call.
func TestOpStopEgressesBeforeDeregister(t *testing.T) {
	l, srv := newTestLifecycle(t)
	l.egressDir = t.TempDir()
	ctx := context.Background()
	const session = "sess-egress"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("load binding after start: (%v, %v, %v)", b, ok, err)
	}
	childJobID := b.ChildJobID

	if err := l.opStop(ctx, session); err != nil {
		t.Fatalf("stop: %v", err)
	}

	trace := srv.Trace()
	catIdx, deregIdx := -1, -1
	for i, req := range trace {
		if catIdx == -1 && strings.HasPrefix(req, "GET /v1/client/fs/cat/") {
			catIdx = i
		}
		if req == "DELETE /v1/job/"+childJobID {
			deregIdx = i
		}
	}
	if catIdx == -1 {
		t.Fatalf("trace has no fs-cat request: %v", trace)
	}
	if deregIdx == -1 {
		t.Fatalf("trace has no deregister request for %q: %v", childJobID, trace)
	}
	if catIdx >= deregIdx {
		t.Fatalf("fs-cat request (trace index %d) did not precede deregister (index %d): %v", catIdx, deregIdx, trace)
	}

	sinkDir := filepath.Join(l.egressDir, base64.RawURLEncoding.EncodeToString([]byte(session)))
	entries, err := os.ReadDir(sinkDir)
	if err != nil {
		t.Fatalf("reading egress sink dir: %v", err)
	}
	if len(entries) != len(egressFiles) {
		t.Fatalf("egress sink dir has %d entries, want %d: %v", len(entries), len(egressFiles), entries)
	}
}

// TestOpStopSkipsEgressWhenDisabled confirms a lifecycle with no egress
// directory configured (the zero value, matching every pre-NRT-P1-07
// deployment) never issues an fs-cat request on stop.
func TestOpStopSkipsEgressWhenDisabled(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-no-egress"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := l.opStop(ctx, session); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if n := countFSCat(srv.Trace()); n != 0 {
		t.Fatalf("fs-cat requests with egress disabled = %d, want 0: %v", n, srv.Trace())
	}
}

// TestOpStopRetryAfterDeregisterFaultSkipsReEgress confirms the sidecar
// egress receipt makes a stop retry idempotent on the egress side: once
// egress has been receipted, a stop that failed later (at deregister) does
// not re-copy files when retried.
func TestOpStopRetryAfterDeregisterFaultSkipsReEgress(t *testing.T) {
	l, srv := newTestLifecycle(t)
	l.egressDir = t.TempDir()
	ctx := context.Background()
	const session = "sess-egress-retry"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("load binding after start: (%v, %v, %v)", b, ok, err)
	}
	srv.FailNext("DELETE", "/v1/job/"+b.ChildJobID, 500, `{"error":"injected"}`)

	if err := l.opStop(ctx, session); err == nil {
		t.Fatalf("stop with faulted deregister: got nil error, want failure")
	}

	afterFail, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("load binding after failed stop: (%v, %v, %v)", afterFail, ok, err)
	}
	if !afterFail.EgressComplete {
		t.Fatalf("binding after failed stop: EgressComplete = false, want true (receipted before the deregister attempt)")
	}
	catCount := countFSCat(srv.Trace())

	if err := l.opStop(ctx, session); err != nil {
		t.Fatalf("retried stop: %v", err)
	}
	if got := countFSCat(srv.Trace()); got != catCount {
		t.Fatalf("fs-cat count after retry = %d, want unchanged %d (egress must not re-run)", got, catCount)
	}
}
