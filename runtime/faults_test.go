package main

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gc-runtime-nomad/fakenomad"
)

// This file is the L2 failure-injection suite (NRT-P1-09): one test per 08
// §3 fault row, each asserting the 04 §6 required answer for that row —
// principally the honesty split (is-running never flips to false on mere
// API unavailability; it answers last-known-good instead) and
// unavailable-is-never-empty (list-running surfaces ANY lookup error rather
// than a silently-empty list). Real-network partitions are out of scope
// (NRT-P2-06); every fault here is scripted against fakenomad or the local
// sidecar filesystem.

// TestFaultAPIOutage covers the API-outage fault row: the Nomad API becomes
// entirely unreachable (connection refused) after a session is running.
func TestFaultAPIOutage(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-outage"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	srv.Close()

	// Honesty split (04 §6): a known binding plus an unreachable API answers
	// true (last-known-good), never false.
	running, err := l.opIsRunning(ctx, session)
	if err != nil || !running {
		t.Fatalf("is-running during outage = (%v, %v), want (true, nil)", running, err)
	}

	// unavailable-is-never-empty: list-running surfaces the error instead of
	// silently reporting nothing running.
	if _, err := l.opListRunning(ctx); err == nil {
		t.Fatalf("list-running during outage = nil error, want a propagated error")
	}
}

// TestFault5xx covers the 5xx fault row: a scripted 500 on deregister must
// surface as a stop failure, not a silent success.
func TestFault5xx(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-5xx"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("sidecar.load(%q) = (%v, %v, %v)", session, b, ok, err)
	}

	srv.FailNext("DELETE", "/v1/job/"+b.ChildJobID, 500, `{"error":"injected"}`)

	if err := l.opStop(ctx, session); err == nil {
		t.Fatalf("stop with a faulted deregister = nil error, want failure")
	}

	// The binding must survive an unconfirmed stop: it wasn't proven gone.
	if _, ok, err := l.sidecar.load(session); err != nil || !ok {
		t.Fatalf("sidecar binding for %q removed despite a failed deregister", session)
	}
}

// TestFaultTLSHandshake covers the TLS-fail fault row: a client that
// doesn't trust the API's (self-signed) certificate gets a real TLS
// handshake failure, and start must fail rather than silently proceed.
func TestFaultTLSHandshake(t *testing.T) {
	srv := fakenomad.NewTLSServer()
	t.Cleanup(srv.Close)

	c, err := newClient(srv.URL(), "", "default")
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	sc, err := newSidecar(t.TempDir())
	if err != nil {
		t.Fatalf("newSidecar: %v", err)
	}
	l := &lifecycle{client: c, sidecar: sc, parentJobID: "gc-sessions"}
	ctx := context.Background()
	const session = "sess-tls"

	if err := l.opStart(ctx, session); err == nil {
		t.Fatalf("start against an untrusted TLS endpoint = nil error, want a handshake failure")
	}

	running, err := l.opIsRunning(ctx, session)
	if err != nil || running {
		t.Fatalf("is-running after a TLS-failed start = (%v, %v), want (false, nil)", running, err)
	}
}

// TestFaultTimeoutMidDispatch covers the timeout-mid-dispatch fault row: the
// dispatch call hangs past the caller's deadline. The pre-dispatch intent
// binding (written before the call that can hang, 04 §2.1 rule 1) must
// survive so a later attempt is not permanently wedged.
func TestFaultTimeoutMidDispatch(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-timeout"

	if err := l.client.registerJob(ctx, parentJobSpec("default", l.parentJobID)); err != nil {
		t.Fatalf("registerJob: %v", err)
	}
	srv.DelayNext("POST", "/v1/job/"+l.parentJobID+"/dispatch", 300*time.Millisecond)

	shortCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := l.opStart(shortCtx, session); err == nil {
		t.Fatalf("start with dispatch stuck mid-flight = nil error, want a timeout")
	}

	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok || b.ChildJobID != "" {
		t.Fatalf("sidecar binding after timeout-mid-dispatch = (%+v, %v, %v), want an intent-only binding", b, ok, err)
	}

	running, err := l.opIsRunning(ctx, session)
	if err != nil || running {
		t.Fatalf("is-running against an intent-only binding = (%v, %v), want (false, nil)", running, err)
	}
}

// TestFaultSlowServer covers the slow-server fault row: a dispatch call that
// is slow but well within the client's timeout must still succeed.
func TestFaultSlowServer(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-slow"

	if err := l.client.registerJob(ctx, parentJobSpec("default", l.parentJobID)); err != nil {
		t.Fatalf("registerJob: %v", err)
	}
	srv.DelayNext("POST", "/v1/job/"+l.parentJobID+"/dispatch", 300*time.Millisecond)

	start := time.Now()
	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start against a slow-but-answering server: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("start returned after %v, want it to have waited out the injected delay", elapsed)
	}

	running, err := l.opIsRunning(ctx, session)
	if err != nil || !running {
		t.Fatalf("is-running after a slow-but-successful start = (%v, %v), want (true, nil)", running, err)
	}
}

// TestFaultStaleIndexNeverTrusted covers the stale-index fault row: the ops
// layer always issues a fresh non-blocking read rather than trusting a
// cached answer, so a terminal transition is reflected immediately.
func TestFaultStaleIndexNeverTrusted(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-stale"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	if running, err := l.opIsRunning(ctx, session); err != nil || !running {
		t.Fatalf("is-running before transition = (%v, %v), want (true, nil)", running, err)
	}

	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("sidecar.load(%q) = (%v, %v, %v)", session, b, ok, err)
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil || len(allocs) != 1 {
		t.Fatalf("listAllocsForJob = (%v, %v), want exactly one alloc", allocs, err)
	}
	srv.SetAllocStatus(allocs[0].ID, "complete")

	running, err := l.opIsRunning(ctx, session)
	if err != nil || running {
		t.Fatalf("is-running after the alloc completed = (%v, %v), want (false, nil): a stale cached index must not mask the transition", running, err)
	}
}

// TestFaultLostAlloc covers the unknown/lost-alloc fault row: an alloc gone
// terminal via "lost" (client node failure) must be excluded from both
// is-running and list-running, same as any other terminal status.
func TestFaultLostAlloc(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-lost"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("sidecar.load(%q) = (%v, %v, %v)", session, b, ok, err)
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil || len(allocs) != 1 {
		t.Fatalf("listAllocsForJob = (%v, %v), want exactly one alloc", allocs, err)
	}
	srv.SetAllocStatus(allocs[0].ID, "lost")

	running, err := l.opIsRunning(ctx, session)
	if err != nil || running {
		t.Fatalf("is-running with a lost alloc = (%v, %v), want (false, nil)", running, err)
	}
	names, err := l.opListRunning(ctx)
	if err != nil {
		t.Fatalf("list-running: %v", err)
	}
	for _, n := range names {
		if n == session {
			t.Fatalf("list-running still includes %q after its only alloc went lost", session)
		}
	}
}

// TestFaultReplacementAlloc covers the replacement-alloc fault row: Nomad
// reschedules a failed alloc under the same job. is-running must count the
// live replacement even though the original alloc is terminal.
func TestFaultReplacementAlloc(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-replace"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("sidecar.load(%q) = (%v, %v, %v)", session, b, ok, err)
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil || len(allocs) != 1 {
		t.Fatalf("listAllocsForJob = (%v, %v), want exactly one alloc", allocs, err)
	}
	srv.SetAllocStatus(allocs[0].ID, "failed")
	srv.PlaceAlloc(b.ChildJobID, "running")

	running, err := l.opIsRunning(ctx, session)
	if err != nil || !running {
		t.Fatalf("is-running after a replacement alloc = (%v, %v), want (true, nil)", running, err)
	}
}

// TestFaultTokenAfterPurge covers the token-after-purge fault row: the
// child job is purged out from under a live binding (its dispatch
// attribution nonce is now moot). Stop against it must still be idempotent
// via the confirmed-absence fold (client.deregisterJob's errJobGone case).
func TestFaultTokenAfterPurge(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-purge"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("sidecar.load(%q) = (%v, %v, %v)", session, b, ok, err)
	}
	if _, err := l.client.deregisterJob(ctx, b.ChildJobID, true); err != nil {
		t.Fatalf("purge: %v", err)
	}

	if err := l.opStop(ctx, session); err != nil {
		t.Fatalf("stop after purge = %v, want nil (confirmed absence is idempotent)", err)
	}
	if _, ok, err := l.sidecar.load(session); err != nil || ok {
		t.Fatalf("sidecar binding for %q survived stop-after-purge", session)
	}
}

// TestFaultCrashPointOrphanedDispatch covers the crash-point-suite fault
// row: the process dies between a successful dispatch and the final
// sidecar commit that binds ChildJobID, leaving the sidecar showing only
// the pre-dispatch intent while a real child job exists cluster-side. A
// fresh start must not wedge behind that orphan.
func TestFaultCrashPointOrphanedDispatch(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-orphan"

	if err := l.client.registerJob(ctx, parentJobSpec("default", l.parentJobID)); err != nil {
		t.Fatalf("registerJob: %v", err)
	}
	nonce, err := newNonce()
	if err != nil {
		t.Fatalf("newNonce: %v", err)
	}
	intent := binding{SessionName: session, Namespace: "default", Nonce: nonce, CreatedAt: time.Now().UTC()}
	if err := l.sidecar.save(intent); err != nil {
		t.Fatalf("sidecar.save(intent): %v", err)
	}
	if _, err := l.client.dispatchChild(ctx, l.parentJobID, session, nonce); err != nil {
		t.Fatalf("dispatchChild: %v", err)
	}

	running, err := l.opIsRunning(ctx, session)
	if err != nil || running {
		t.Fatalf("is-running with an orphaned intent binding = (%v, %v), want (false, nil)", running, err)
	}
	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start after a crashed prior dispatch = %v, want nil (must not wedge behind the orphan)", err)
	}
}

// TestFaultStopSuite covers the stop-suite fault row across the shapes stop
// must handle without ever silently doing the wrong thing.
func TestFaultStopSuite(t *testing.T) {
	t.Run("intent-only binding from a pre-dispatch crash", func(t *testing.T) {
		l, _ := newTestLifecycle(t)
		ctx := context.Background()
		const session = "sess-intent"

		nonce, err := newNonce()
		if err != nil {
			t.Fatalf("newNonce: %v", err)
		}
		if err := l.sidecar.save(binding{SessionName: session, Nonce: nonce, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("sidecar.save: %v", err)
		}
		if err := l.opStop(ctx, session); err != nil {
			t.Fatalf("stop on an intent-only binding = %v, want nil", err)
		}
		if _, ok, err := l.sidecar.load(session); err != nil || ok {
			t.Fatalf("sidecar binding for %q survived stop", session)
		}
	})

	t.Run("deregister fault propagates instead of silently succeeding", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		ctx := context.Background()
		const session = "sess-stopfail"

		if err := l.opStart(ctx, session); err != nil {
			t.Fatalf("start: %v", err)
		}
		b, ok, err := l.sidecar.load(session)
		if err != nil || !ok {
			t.Fatalf("sidecar.load(%q) = (%v, %v, %v)", session, b, ok, err)
		}
		srv.FailNext("DELETE", "/v1/job/"+b.ChildJobID, 500, `{"error":"injected"}`)

		if err := l.opStop(ctx, session); err == nil {
			t.Fatalf("stop with a faulted deregister = nil error, want failure")
		}
		if _, ok, err := l.sidecar.load(session); err != nil || !ok {
			t.Fatalf("sidecar binding for %q removed despite a failed deregister", session)
		}
	})
}

// TestFaultConcurrentStartRace covers the concurrency-race fault row: two
// goroutines racing opStart for the same session must not panic or corrupt
// the on-disk sidecar binding, and must leave the session in a coherent
// running state. Which goroutine "wins" is not guaranteed (04 §6 notes the
// pre-dispatch duplicate check is best-effort, not a hard lock) — this test
// asserts safety, not exclusivity. Run with -race to catch real data races.
func TestFaultConcurrentStartRace(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-race"

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = l.opStart(ctx, session)
		}(i)
	}
	wg.Wait()

	b, ok, err := l.sidecar.load(session)
	if err != nil {
		t.Fatalf("sidecar binding corrupted by concurrent start: %v", err)
	}
	if !ok || b.ChildJobID == "" {
		t.Fatalf("sidecar binding after concurrent start = (%+v, %v), want a bound ChildJobID", b, ok)
	}

	running, err := l.opIsRunning(ctx, session)
	if err != nil || !running {
		t.Fatalf("is-running after concurrent start = (%v, %v), want (true, nil)", running, err)
	}
}

// TestFaultConsistencyAcrossSessions covers the consistency-case fault row:
// one session's terminal transition must be reflected in list-running
// without contaminating a still-running sibling's state.
func TestFaultConsistencyAcrossSessions(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()

	for _, s := range []string{"alpha", "bravo"} {
		if err := l.opStart(ctx, s); err != nil {
			t.Fatalf("start %q: %v", s, err)
		}
	}

	ba, ok, err := l.sidecar.load("alpha")
	if err != nil || !ok {
		t.Fatalf("sidecar.load(alpha) = (%v, %v, %v)", ba, ok, err)
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, ba.ChildJobID, 0, 0)
	if err != nil || len(allocs) != 1 {
		t.Fatalf("listAllocsForJob(alpha) = (%v, %v), want exactly one alloc", allocs, err)
	}
	srv.SetAllocStatus(allocs[0].ID, "complete")

	names, err := l.opListRunning(ctx)
	if err != nil {
		t.Fatalf("list-running: %v", err)
	}
	want := []string{"bravo"}
	if len(names) != len(want) || names[0] != want[0] {
		t.Fatalf("list-running = %v, want %v (alpha's completion must not leak into bravo's state)", names, want)
	}
}

// TestFaultSidecarLoss covers the sidecar-loss fault row: the sidecar
// directory disappears out from under a running session (disk loss). With
// no binding on record there is no last-known-good to fall back to, so
// is-running answers false — distinct from the API-unavailable honesty
// split, which only applies once a binding is known. list-running must
// still surface the loss as an error, never a silently-empty list.
func TestFaultSidecarLoss(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-sidecarloss"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := os.RemoveAll(l.sidecar.dir); err != nil {
		t.Fatalf("simulating sidecar loss: %v", err)
	}

	running, err := l.opIsRunning(ctx, session)
	if err != nil || running {
		t.Fatalf("is-running after sidecar loss = (%v, %v), want (false, nil)", running, err)
	}
	if _, err := l.opListRunning(ctx); err == nil {
		t.Fatalf("list-running after sidecar loss = nil error, want a propagated error")
	}
}

// TestFaultPermanentAuth covers the permanent-auth fault row: a sticky
// (non-transient) 401 must keep failing every attempt, not just the first,
// while the honesty split still holds for is-running.
func TestFaultPermanentAuth(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-auth"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("sidecar.load(%q) = (%v, %v, %v)", session, b, ok, err)
	}
	srv.FailSticky("GET", "/v1/job/"+b.ChildJobID+"/allocations", 401, `{"error":"permission denied"}`)

	for i := 0; i < 3; i++ {
		running, err := l.opIsRunning(ctx, session)
		if err != nil || !running {
			t.Fatalf("attempt %d: is-running under permanent auth failure = (%v, %v), want (true, nil)", i, running, err)
		}
	}
	if _, err := l.opListRunning(ctx); err == nil {
		t.Fatalf("list-running under permanent auth failure = nil error, want a propagated error")
	}
}
