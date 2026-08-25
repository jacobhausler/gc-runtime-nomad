package main

import (
	"context"
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

	names, err := l.opListRunning(ctx)
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
	names, err := l.opListRunning(context.Background())
	if err != nil {
		t.Fatalf("list-running: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("list-running = %v, want empty", names)
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
	if err := l.client.registerJob(ctx, parentJobSpec("default", l.parentJobID)); err != nil {
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
