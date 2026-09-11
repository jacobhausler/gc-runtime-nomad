package main

import (
	"context"
	"strings"
	"testing"
)

// T5: list-running must stop naming a seat whose box is gone for good.
//
// Before this, opListRunning skipped a child only if the child JOB was
// terminal. Killing an allocation does not make its parent dispatch terminal --
// the job still exists and Nomad has simply lost the alloc -- so the verb
// answered "is there a non-terminal child job" while its caller asked "is this
// seat alive". Measured on a live lane 2026-09-11: alloc SIGKILLed, Nomad
// reporting the task dead twenty-five seconds later, and `gc-runtime-nomad
// list-running` still naming the seat five minutes on. gc's orphan-release
// probe consults this verb and fails closed on "still present", so the claim
// was never released and the bead stayed assigned to a dead seat.
func TestListRunningDropsASeatWhoseAllocsAreAllTerminal(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-listrunning-dead-alloc"

	if err := l.opStartWithConfig(ctx, session, stageConfig{Command: testPaneCommand}); err != nil {
		t.Fatalf("start: %v", err)
	}
	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok || b.ChildJobID == "" {
		t.Fatalf("sidecar.load = (%+v, %v, %v)", b, ok, err)
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil || len(allocs) == 0 {
		t.Fatalf("listAllocsForJob = (%v, %v), want at least one alloc", allocs, err)
	}
	// The fake marks its allocation terminal once the scripted pane command
	// exits, which is not the state a live seat is in. Put it back to running
	// so this test measures the transition it is about rather than the fake's
	// own bookkeeping.
	for _, a := range allocs {
		srv.SetAllocStatus(a.ID, "running")
	}
	names, err := l.opListRunning(ctx, "")
	if err != nil || !containsName(names, session) {
		t.Fatalf("list-running with a RUNNING alloc = (%v, %v), want it to name %q", names, err, session)
	}
	// The box dies and does NOT come back: every allocation terminal, while the
	// child job itself stays non-terminal exactly as Nomad leaves it.
	for _, a := range allocs {
		srv.SetAllocStatus(a.ID, "failed")
	}

	names, err = l.opListRunning(ctx, "")
	if err != nil {
		t.Fatalf("list-running after the box died = %v", err)
	}
	if containsName(names, session) {
		t.Fatalf("list-running still names %q after every allocation went terminal — a dead box reported as a running seat is what strands a claim forever", session)
	}
}

// The fail-closed half, and it is the one that matters more. A transient Nomad
// blip must never look like a fleet of dead seats: that would release live work
// everywhere at once, which is far worse than holding one claim too long.
func TestListRunningKeepsASeatWhenTheAllocReadFails(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-listrunning-unreadable"

	if err := l.opStartWithConfig(ctx, session, stageConfig{Command: testPaneCommand}); err != nil {
		t.Fatalf("start: %v", err)
	}
	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("sidecar.load = (%+v, %v, %v)", b, ok, err)
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil {
		t.Fatalf("listAllocsForJob: %v", err)
	}
	for _, a := range allocs {
		srv.SetAllocStatus(a.ID, "running")
	}
	srv.SetAllocListError(true)
	defer srv.SetAllocListError(false)

	names, err := l.opListRunning(ctx, "")
	if err != nil {
		t.Fatalf("list-running with an unreadable alloc list = %v, want the child kept rather than an error", err)
	}
	if !containsName(names, session) {
		t.Fatalf("list-running dropped %q because the allocation read FAILED — an unreadable runtime is not evidence of death, and treating it as such releases live work fleet-wide", session)
	}
}

// T6: provision must not report success for an image no node can run.
//
// Before this it returned 0 with empty stdout and empty stderr: the child was
// dispatched, nothing was ever placed, and the caller was told everything was
// fine. The failure only surfaced three layers later as "staging workspace:
// reading exec frame: EOF". A verb whose success means "I sent a request" is
// the wrong contract for one the caller uses to mean "the box exists".
func TestProvisionFailsAndNamesTheImageWhenNothingIsPlaced(t *testing.T) {
	l, srv := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-provision-unplaced"

	srv.SetPlaceAllocs(false)
	err := l.opProvision(ctx, session)
	if err == nil {
		t.Fatal("provision with no allocation placed = nil error — exit 0 and silent is the absence of a signal, not a weaker one")
	}
	if !strings.Contains(err.Error(), l.agentImage) {
		t.Fatalf("provision failure %q does not name the image %q — the operator needs the image, not a derived error three layers away", err, l.agentImage)
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
