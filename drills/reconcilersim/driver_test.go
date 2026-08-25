package reconcilersim_test

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gc-runtime-nomad/drills/reconcilersim"
	"github.com/gastownhall/gc-runtime-nomad/fakenomad"
)

// buildPackCLI builds the real gc-runtime-nomad binary from ../../runtime
// into a fresh temp dir and returns its path — the same "build the actual
// shipped binary" approach conformance.sh uses, so this offline test
// exercises the harness through the real RPP subprocess boundary
// (Driver.RunOp), not an in-process shortcut.
func buildPackCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "gc-runtime-nomad")
	runtimeDir, err := filepath.Abs("../../runtime")
	if err != nil {
		t.Fatalf("resolving runtime dir: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = runtimeDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building gc-runtime-nomad: %v\n%s", err, out)
	}
	return bin
}

// newTestDriver wires a Driver at a fresh fakenomad server plus the real
// built pack CLI binary, with its own sidecar dir — this package's
// hermetic offline twin of an L4 run (NRT-P2-06a: "Offline-testable
// against fakenomad").
func newTestDriver(t *testing.T) (*reconcilersim.Driver, *fakenomad.Server) {
	t.Helper()
	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)

	bin := buildPackCLI(t)
	sidecarDir := t.TempDir()

	cfg := reconcilersim.Config{
		Addr:       srv.URL(),
		Namespace:  "default",
		ParentJob:  "gc-sessions",
		RuntimeBin: bin,
	}
	d, err := reconcilersim.New(cfg)
	if err != nil {
		t.Fatalf("reconcilersim.New: %v", err)
	}
	d = d.WithEnv("GC_NOMAD_SIDECAR_DIR=" + sidecarDir)
	t.Cleanup(func() { _ = d.Close() })
	return d, srv
}

// startSession execs `start` for session through the real pack CLI and
// returns the dispatched child job ID, resolved by its gc_session Meta key
// (never derivable from the session name itself, e2a-job-id-charset-gap).
func startSession(t *testing.T, ctx context.Context, d *reconcilersim.Driver, session string) string {
	t.Helper()
	res, err := d.RunOp(ctx, "start", nil, session)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("start %s: err=%v exit=%d stderr=%s", session, err, res.ExitCode, res.Stderr)
	}
	children, err := d.Observer.ListChildJobs(ctx, "gc-sessions")
	if err != nil {
		t.Fatalf("ListChildJobs: %v", err)
	}
	for _, c := range children {
		if c.Meta["gc_session"] == session {
			return c.ID
		}
	}
	t.Fatalf("no child job found for session %s", session)
	return ""
}

// TestLifecycleRoundtripScenarioOffline runs the harness's own
// LifecycleRoundtripSteps scenario end to end against fakenomad through the
// real pack CLI subprocess — proving the Driver/Observer/RunScript wiring
// works hermetically before any of it is pointed at the real T1 lab.
func TestLifecycleRoundtripScenarioOffline(t *testing.T) {
	d, _ := newTestDriver(t)
	session := fmt.Sprintf("drill-test-%d", time.Now().UnixNano())

	report := reconcilersim.RunScript(context.Background(), d, reconcilersim.LifecycleRoundtripSteps(session, "gc-sessions"))

	for _, s := range report.Steps {
		t.Logf("step %q: passed=%v elapsed=%s failMsg=%q", s.Name, s.Passed, s.Elapsed, s.FailMsg)
	}
	if !report.AllPassed() {
		t.Fatalf("LifecycleRoundtripSteps did not all pass: %+v", report)
	}
}

// TestClassifyDeathBoxKillOffline drives ClassifyDeath (classify.go)
// through the real pack CLI + observer client against fakenomad's
// SetAllocStatus fault-injection primitive, which exists specifically so
// tests can simulate the 08 §1.1 box-kill row's ClientStatus transition
// (a fake box-kill; fakenomad has no real process to kill -9, so the
// in-box-agent-kill row's classification path is covered as a pure-logic
// case in classify_test.go instead — see TestClassifyDeath).
func TestClassifyDeathBoxKillOffline(t *testing.T) {
	d, srv := newTestDriver(t)
	ctx := context.Background()

	session := "sess-box-kill"
	jobID := startSession(t, ctx, d, session)

	before, err := d.Observer.LatestAlloc(ctx, jobID)
	if err != nil {
		t.Fatalf("LatestAlloc before: %v", err)
	}

	if ok := srv.SetAllocStatus(before.ID, "failed"); !ok {
		t.Fatalf("SetAllocStatus(%s, failed) = false, alloc not found", before.ID)
	}

	running, err := d.IsRunning(ctx, session)
	if err != nil {
		t.Fatalf("IsRunning after box kill: %v", err)
	}

	after, err := d.Observer.LatestAlloc(ctx, jobID)
	if err != nil {
		t.Fatalf("LatestAlloc after: %v", err)
	}

	got := reconcilersim.ClassifyDeath(before, after, running)
	if got != reconcilersim.DeathBox {
		t.Fatalf("ClassifyDeath = %v, want box-death (before=%+v after=%+v isRunning=%v)", got, before, after, running)
	}
}

// TestCountReplacementAllocsViaPlaceAlloc confirms the "zero replacement
// allocs" pass criterion (04 §4 fencing) trips correctly once fakenomad's
// PlaceAlloc simulates a second allocation appearing under the same job.
func TestCountReplacementAllocsViaPlaceAlloc(t *testing.T) {
	d, srv := newTestDriver(t)
	ctx := context.Background()

	session := "sess-replace"
	jobID := startSession(t, ctx, d, session)

	allocs, err := d.Observer.ListAllocsForJob(ctx, jobID)
	if err != nil {
		t.Fatalf("ListAllocsForJob: %v", err)
	}
	if n := reconcilersim.CountReplacementAllocs(allocs); n != 0 {
		t.Fatalf("CountReplacementAllocs before PlaceAlloc = %d, want 0", n)
	}

	srv.PlaceAlloc(jobID, "running")

	allocs, err = d.Observer.ListAllocsForJob(ctx, jobID)
	if err != nil {
		t.Fatalf("ListAllocsForJob after PlaceAlloc: %v", err)
	}
	if n := reconcilersim.CountReplacementAllocs(allocs); n != 1 {
		t.Fatalf("CountReplacementAllocs after PlaceAlloc = %d, want 1", n)
	}
}
