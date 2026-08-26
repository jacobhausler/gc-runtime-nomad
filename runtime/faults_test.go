package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	if _, err := l.opListRunning(ctx, ""); err == nil {
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

	if err := l.client.registerJob(ctx, parentJobSpec("default", l.nodePool, l.parentJobID)); err != nil {
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

	if err := l.client.registerJob(ctx, parentJobSpec("default", l.nodePool, l.parentJobID)); err != nil {
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

// TestFaultLostAlloc covers the lost-alloc half of the unknown/lost-alloc
// fault row: an alloc gone terminal via "lost" (client node failure) must
// be excluded from both is-running and list-running, same as any other
// terminal status. TestFaultUnknownAlloc below covers the opposite-answer
// "unknown" half of the same row.
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
	names, err := l.opListRunning(ctx, "")
	if err != nil {
		t.Fatalf("list-running: %v", err)
	}
	for _, n := range names {
		if n == session {
			t.Fatalf("list-running still includes %q after its only alloc went lost", session)
		}
	}
}

// TestFaultTaskExitedBeforeLaunch covers the NRT-P2-06.1 regression: a
// session task that has already exited (terminal alloc) by the time an op
// needs to exec into the box — the /bin/true placeholder's failure mode on
// a real client, simulated directly here rather than racing a real
// process's exit — must fail clearly (errSessionNotFound: no non-terminal
// alloc) instead of silently launching into a dead alloc, and must not
// wedge a subsequent fresh start.
func TestFaultTaskExitedBeforeLaunch(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-task-exited-early"

	if err := l.opProvision(ctx, session); err != nil {
		t.Fatalf("provision: %v", err)
	}
	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok || b.ChildJobID == "" {
		t.Fatalf("sidecar.load(%q) after provision = (%+v, %v, %v)", session, b, ok, err)
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil || len(allocs) != 1 {
		t.Fatalf("listAllocsForJob = (%v, %v), want exactly one alloc", allocs, err)
	}
	srv.SetAllocStatus(allocs[0].ID, "complete")

	if err := l.opRelaunch(ctx, session); err == nil {
		t.Fatalf("relaunch onto a pre-launch-terminal alloc = nil error, want failure")
	} else if !errors.Is(err, errSessionNotFound) {
		t.Fatalf("relaunch onto a pre-launch-terminal alloc = %v, want an errSessionNotFound-wrapped error (nothing live to exec into)", err)
	}

	// A fresh start must not wedge behind the dead child: dispatch's
	// "already exists" guard only fires for a still-live child, so start
	// transparently redispatches and the session comes up normally.
	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start after the dead child = %v, want nil (fresh dispatch)", err)
	}
	if running, err := l.opIsRunning(ctx, session); err != nil || !running {
		t.Fatalf("is-running after fresh start = (%v, %v), want (true, nil)", running, err)
	}
}

// TestFaultUnknownAlloc covers the unknown-alloc half of the unknown/lost
// fault row: 04 §6 lists alloc-unknown (a disconnected client node — the
// client hasn't been heard from, but nothing has declared the alloc dead)
// and alloc-lost as SEPARATE rows with OPPOSITE required answers. "unknown"
// must never read as gone — the honesty split holds: is-running stays true
// (last-known-good) and list-running keeps including the session, exactly
// as if the API were merely unavailable, because isTerminalStatus does not
// treat "unknown" as terminal.
func TestFaultUnknownAlloc(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-unknown"

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
	srv.SetAllocStatus(allocs[0].ID, "unknown")

	running, err := l.opIsRunning(ctx, session)
	if err != nil || !running {
		t.Fatalf("is-running with an unknown-status alloc = (%v, %v), want (true, nil): unknown must never read as gone", running, err)
	}
	names, err := l.opListRunning(ctx, "")
	if err != nil {
		t.Fatalf("list-running: %v", err)
	}
	found := false
	for _, n := range names {
		if n == session {
			found = true
		}
	}
	if !found {
		t.Fatalf("list-running dropped %q after its alloc went unknown, want it retained (last-known-good)", session)
	}
}

// TestFaultReplacementAlloc covers the replacement-alloc fault row: Nomad
// reschedules a failed alloc under the same job. is-running must select the
// live replacement even though the original alloc is terminal — but since
// nothing has launched an agent into the fresh replacement yet, the in-box
// liveness probe correctly answers false there (agent not present) rather
// than reusing the OLD alloc's now-stale launched state; only once the
// replacement is explicitly relaunched does is-running flip true again,
// proving the alloc-selection logic is now checking the NEW alloc.
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
	if err != nil || running {
		t.Fatalf("is-running right after a replacement alloc = (%v, %v), want (false, nil): no agent has been launched into the new alloc yet", running, err)
	}

	if err := l.opRelaunch(ctx, session); err != nil {
		t.Fatalf("relaunch into the replacement alloc: %v", err)
	}
	running, err = l.opIsRunning(ctx, session)
	if err != nil || !running {
		t.Fatalf("is-running after relaunching into the replacement alloc = (%v, %v), want (true, nil)", running, err)
	}
}

// TestFaultInBoxAgentKill covers the in-box-agent-kill fault row (08 §3):
// the box (alloc) survives untouched, but the agent process/tmux session
// inside it dies on its own (e.g. an OOM kill) without ever touching
// Nomad's task-main process. is-running must flip to false (agent-dead)
// even though the alloc's ClientStatus stays "running" throughout — the
// exact honesty gap ClientStatus-only checks can never see — and must not
// read as a confirmed box death either: a relaunch into the SAME alloc (no
// re-dispatch) brings the session straight back.
func TestFaultInBoxAgentKill(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-inbox-kill"

	if err := l.opStart(ctx, session); err != nil {
		t.Fatalf("start: %v", err)
	}
	if running, err := l.opIsRunning(ctx, session); err != nil || !running {
		t.Fatalf("is-running after start = (%v, %v), want (true, nil)", running, err)
	}

	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("sidecar.load(%q) = (%+v, %v, %v)", session, b, ok, err)
	}
	allocID, err := l.currentAlloc(ctx, session)
	if err != nil {
		t.Fatalf("currentAlloc: %v", err)
	}

	// Kill the in-box tmux session (and with it the pid capturePanePID
	// recorded at launch) directly over exec, without touching the alloc
	// itself: jobspec.go's sessionSupervisorScript (the task-main process)
	// is untouched, so ClientStatus stays "running" — exactly the fault
	// this row models.
	exitCode, out, err := l.client.execAlloc(ctx, allocID, execTaskName, []string{"tmux", "kill-session", "-t", tmuxSessionName})
	if err != nil || exitCode != 0 {
		t.Fatalf("tmux kill-session = (%d, %v): %s", exitCode, err, out)
	}

	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil || len(allocs) != 1 || allocs[0].ClientStatus != "running" {
		t.Fatalf("alloc status after in-box agent kill = (%+v, %v), want exactly one alloc still %q", allocs, err, "running")
	}

	running, err := l.opIsRunning(ctx, session)
	if err != nil || running {
		t.Fatalf("is-running after in-box agent kill = (%v, %v), want (false, nil): box survives but the agent is gone", running, err)
	}

	if err := l.opRelaunch(ctx, session); err != nil {
		t.Fatalf("relaunch after in-box agent kill: %v", err)
	}
	running, err = l.opIsRunning(ctx, session)
	if err != nil || !running {
		t.Fatalf("is-running after relaunch = (%v, %v), want (true, nil)", running, err)
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

// TestFaultCrashPointSuite covers the crash-point-suite fault row's three
// pinned crash points (08 §3 line 96 / 04 §2.1 rule 6, R2b-02): crash
// before dispatch, crash after dispatch but before the binding commit, and
// crash after the binding commit but before the launched marker
// ("ConfirmStarted"). Each must converge to a single non-terminal child on
// retry, adopting via the dispatch-intent nonce rather than ever leaving a
// duplicate live child.
func TestFaultCrashPointSuite(t *testing.T) {
	t.Run("before dispatch: nothing cluster-side yet", func(t *testing.T) {
		l, _ := newTestLifecycle(t)
		ctx := context.Background()
		const session = "sess-crash-predispatch"

		nonce, err := newNonce()
		if err != nil {
			t.Fatalf("newNonce: %v", err)
		}
		// Simulates a crash between the pre-dispatch intent write (04 §2.1
		// rule 1) and the dispatch call itself: the intent is on record,
		// but no child job was ever created cluster-side.
		if err := l.sidecar.save(binding{SessionName: session, Namespace: "default", Nonce: nonce, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("sidecar.save(intent): %v", err)
		}

		if err := l.opStart(ctx, session); err != nil {
			t.Fatalf("start retry after a pre-dispatch crash = %v, want nil (nothing to adopt, dispatch fresh)", err)
		}

		names, err := l.opListRunning(ctx, "")
		if err != nil {
			t.Fatalf("list-running: %v", err)
		}
		count := 0
		for _, n := range names {
			if n == session {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("list-running reports %d entries for %q after retrying a pre-dispatch crash, want exactly 1 (single non-terminal child)", count, session)
		}
	})

	t.Run("after dispatch, before binding: orphan gets adopted", func(t *testing.T) {
		l, _ := newTestLifecycle(t)
		ctx := context.Background()
		const session = "sess-crash-orphan"

		if err := l.client.registerJob(ctx, parentJobSpec("default", l.nodePool, l.parentJobID)); err != nil {
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
		orphanID, err := l.client.dispatchChild(ctx, l.parentJobID, session, nonce)
		if err != nil {
			t.Fatalf("dispatchChild: %v", err)
		}

		running, err := l.opIsRunning(ctx, session)
		if err != nil || running {
			t.Fatalf("is-running with an orphaned intent binding = (%v, %v), want (false, nil)", running, err)
		}
		if err := l.opStart(ctx, session); err != nil {
			t.Fatalf("start after a crashed prior dispatch = %v, want nil (must not wedge behind the orphan)", err)
		}

		b, ok, err := l.sidecar.load(session)
		if err != nil || !ok {
			t.Fatalf("sidecar.load(%q) after adoption = (%v, %v, %v)", session, b, ok, err)
		}
		if b.ChildJobID != orphanID {
			t.Fatalf("sidecar binding after retry = %q, want the orphaned child %q adopted rather than a fresh dispatch (04 §2.1 rule 6)", b.ChildJobID, orphanID)
		}

		names, err := l.opListRunning(ctx, "")
		if err != nil {
			t.Fatalf("list-running: %v", err)
		}
		count := 0
		for _, n := range names {
			if n == session {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("list-running reports %d entries for %q after adopting the orphan, want exactly 1 (single non-terminal child)", count, session)
		}
	})

	t.Run("after binding, before ConfirmStarted: retry resumes to launch", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		ctx := context.Background()
		const session = "sess-crash-prelaunch"

		// dispatch() alone reproduces exactly the state a crash right
		// after it commits the binding (with ChildJobID set) but before
		// markLaunched runs would leave: a bound, live, not-yet-launched
		// child.
		if err := l.dispatch(ctx, session); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		b, ok, err := l.sidecar.load(session)
		if err != nil || !ok || b.ChildJobID == "" || b.Launched {
			t.Fatalf("sidecar.load(%q) after dispatch = (%+v, %v, %v), want a bound, not-yet-launched binding", session, b, ok, err)
		}
		childID := b.ChildJobID

		if err := l.opStart(ctx, session); err != nil {
			t.Fatalf("start retry after a crashed pre-launch attempt = %v, want nil (resume to launch)", err)
		}

		b2, ok, err := l.sidecar.load(session)
		if err != nil || !ok || !b2.Launched {
			t.Fatalf("sidecar.load(%q) after resume = (%+v, %v, %v), want Launched=true", session, b2, ok, err)
		}
		if b2.ChildJobID != childID {
			t.Fatalf("sidecar binding after resume = %q, want the same child %q reused rather than a second dispatch", b2.ChildJobID, childID)
		}

		dispatches := 0
		for _, req := range srv.Trace() {
			if strings.HasPrefix(req, "POST ") && strings.HasSuffix(req, "/dispatch") {
				dispatches++
			}
		}
		if dispatches != 1 {
			t.Fatalf("resume after a pre-launch crash issued %d dispatch calls, want exactly 1 (no duplicate dispatch); trace = %v", dispatches, srv.Trace())
		}
	})
}

// TestFaultStopSuite covers the stop-suite fault row's three pinned rows
// (08 §3 line 99 / R2b-04): egress error exhausts bounded retries and falls
// back to an evidence_lost marker rather than wedging stop; stop during an
// outage exits with a failure; and a crash between egress and deregister
// retries idempotently via the egress-complete marker (not re-copying
// files a prior attempt already egressed). Also covers the pre-dispatch
// intent-only binding shape, since Stop must stay idempotent there too
// (04 §6, E1a §6.1).
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

	t.Run("egress error exhausts bounded retries then proceeds with evidence_lost", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		l.egressDir = t.TempDir()
		ctx := context.Background()
		const session = "sess-egress-fail"

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
		srv.FailSticky("GET", "/v1/client/fs/cat/"+allocs[0].ID, 500, `{"error":"injected"}`)

		before := len(srv.Trace())
		if err := l.opStop(ctx, session); err != nil {
			t.Fatalf("stop with a persistently failing egress = %v, want nil (bounded retries then evidence_lost, R2b-04)", err)
		}
		after := srv.Trace()

		catAttempts := 0
		deregistered := false
		for _, req := range after[before:] {
			switch {
			case req == "GET /v1/client/fs/cat/"+allocs[0].ID:
				catAttempts++
			case req == "DELETE /v1/job/"+b.ChildJobID:
				deregistered = true
			}
		}
		if catAttempts != egressMaxAttempts {
			t.Fatalf("egress fs-cat attempts = %d, want exactly %d (bounded retries, R2b-04); trace = %v", catAttempts, egressMaxAttempts, after[before:])
		}
		if !deregistered {
			t.Fatalf("stop never deregistered the child job after the evidence_lost fallback; trace = %v", after[before:])
		}
		if _, ok, err := l.sidecar.load(session); err != nil || ok {
			t.Fatalf("sidecar binding for %q survived a successful stop", session)
		}
	})

	t.Run("stop during outage exits with a failure", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		ctx := context.Background()
		const session = "sess-stop-outage"

		if err := l.opStart(ctx, session); err != nil {
			t.Fatalf("start: %v", err)
		}
		srv.Close()

		if err := l.opStop(ctx, session); err == nil {
			t.Fatalf("stop during an API outage = nil error, want failure (04 §3 stop row)")
		}
		if _, ok, err := l.sidecar.load(session); err != nil || !ok {
			t.Fatalf("sidecar binding for %q removed despite an unconfirmed stop", session)
		}
	})

	t.Run("crash between egress and deregister retries idempotently via egress-complete marker", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		l.egressDir = t.TempDir()
		ctx := context.Background()
		const session = "sess-crash-between"

		if err := l.opStart(ctx, session); err != nil {
			t.Fatalf("start: %v", err)
		}
		b, ok, err := l.sidecar.load(session)
		if err != nil || !ok {
			t.Fatalf("sidecar.load(%q) = (%v, %v, %v)", session, b, ok, err)
		}

		// Deregister itself fails once, leaving EgressComplete already
		// receipted — the state a crash between egress and deregister
		// would leave behind.
		srv.FailNext("DELETE", "/v1/job/"+b.ChildJobID, 500, `{"error":"injected"}`)
		if err := l.opStop(ctx, session); err == nil {
			t.Fatalf("stop with a faulted deregister = nil error, want failure")
		}
		b2, ok, err := l.sidecar.load(session)
		if err != nil || !ok || !b2.EgressComplete {
			t.Fatalf("sidecar.load(%q) after the crashed deregister = (%+v, %v, %v), want EgressComplete=true retained", session, b2, ok, err)
		}

		before := len(srv.Trace())
		if err := l.opStop(ctx, session); err != nil {
			t.Fatalf("stop retry after the crash = %v, want nil", err)
		}
		after := srv.Trace()

		catAttempts := 0
		for _, req := range after[before:] {
			if strings.HasPrefix(req, "GET /v1/client/fs/cat/") {
				catAttempts++
			}
		}
		if catAttempts != 0 {
			t.Fatalf("stop retry re-read egress files %d times after EgressComplete was already true, want 0 (idempotent retry via the egress-complete marker)", catAttempts)
		}
		if _, ok, err := l.sidecar.load(session); err != nil || ok {
			t.Fatalf("sidecar binding for %q survived the successful retry", session)
		}
	})
}

// TestFaultConcurrentStartRace covers the concurrency-race fault row:
// concurrent same-name Start under the single-flight lock must produce
// exactly one child (R2b-03) — not merely avoid a panic or corrupt the
// on-disk sidecar binding. Run with -race to catch real data races on top.
func TestFaultConcurrentStartRace(t *testing.T) {
	l, srv := newTestLifecycle(t)
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

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent same-name start: %d of 2 calls succeeded, want exactly 1 under the single-flight lock (R2b-03); errs = %v", succeeded, errs)
	}

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

	dispatches := 0
	for _, req := range srv.Trace() {
		if strings.HasSuffix(req, "/dispatch") && strings.HasPrefix(req, "POST ") {
			dispatches++
		}
	}
	if dispatches != 1 {
		t.Fatalf("concurrent same-name start issued %d dispatch calls, want exactly 1 non-terminal child (R2b-03); trace = %v", dispatches, srv.Trace())
	}
}

// TestFaultConcurrentStartRaceCrossProcess covers the concurrency-race row
// under the shape it actually happens in: `gc` execs gc-runtime-nomad fresh
// per op (main.go's calling convention — "gc execs the binary directly, no
// shell wrapping"), so a real race is between two independent OS processes,
// never two goroutines sharing one Go value. This test races two separate
// *lifecycle values that share only the sidecar directory on disk (as two
// process invocations would), so an in-process-only lock (e.g. a plain
// sync.Mutex on the lifecycle) would pass TestFaultConcurrentStartRace
// above while doing nothing here — only a lock that lives on disk (flock)
// closes this race for real.
func TestFaultConcurrentStartRaceCrossProcess(t *testing.T) {
	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)
	sidecarDir := filepath.Join(t.TempDir(), "sidecar")
	const session = "sess-race-xproc"

	newProcessLifecycle := func() (*lifecycle, error) {
		c, err := newClient(srv.URL(), "", "default")
		if err != nil {
			return nil, err
		}
		sc, err := newSidecar(sidecarDir)
		if err != nil {
			return nil, err
		}
		return &lifecycle{client: c, sidecar: sc, parentJobID: "gc-sessions"}, nil
	}

	l1, err := newProcessLifecycle()
	if err != nil {
		t.Fatalf("newProcessLifecycle: %v", err)
	}
	l2, err := newProcessLifecycle()
	if err != nil {
		t.Fatalf("newProcessLifecycle: %v", err)
	}
	lifecycles := []*lifecycle{l1, l2}

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = lifecycles[i].opStart(ctx, session)
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent same-name start from two independent lifecycle values (simulating two OS processes): %d of 2 succeeded, want exactly 1 (R2b-03); errs = %v", succeeded, errs)
	}

	names, err := l1.opListRunning(ctx, "")
	if err != nil {
		t.Fatalf("list-running: %v", err)
	}
	count := 0
	for _, n := range names {
		if n == session {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("list-running reports %d entries for %q after a cross-process race, want exactly 1 (single non-terminal child)", count, session)
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

	names, err := l.opListRunning(ctx, "")
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
	if _, err := l.opListRunning(ctx, ""); err == nil {
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

	// list-running's honesty split is driven by the children-of-parent jobs
	// list (04 §2.1 rule 2/3), not the per-job allocations endpoint
	// is-running above just exercised — fault that endpoint too so
	// list-running's own unavailable-is-never-empty answer gets asserted.
	srv.FailSticky("GET", "/v1/jobs", 401, `{"error":"permission denied"}`)
	if _, err := l.opListRunning(ctx, ""); err == nil {
		t.Fatalf("list-running under permanent auth failure = nil error, want a propagated error")
	}
}
