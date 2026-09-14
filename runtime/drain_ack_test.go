package main

import (
	"context"
	"regexp"
	"testing"
)

// TestOpIsRunningRetiresOneShotSessionAfterDrainAck is the red/green this
// mechanism exists for: a one_shot seat whose captured pane transcript
// already shows the claim-free remote drain ack succeeding must not hold
// its slot — the very poll that discovers the ack must retire the session,
// and the underlying alloc must reach a terminal state exactly as any other
// stop leaves it.
func TestOpIsRunningRetiresOneShotSessionAfterDrainAck(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-one-shot-drain"

	if err := l.opStartWithConfig(ctx, session, stageConfig{
		Command:   testPaneCommand,
		Lifecycle: lifecycleOneShot,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	running, err := l.opIsRunning(ctx, session)
	if err != nil || !running {
		t.Fatalf("is-running before drain-ack = (%v, %v), want (true, nil)", running, err)
	}

	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("load binding: (%v, %v, %v)", b, ok, err)
	}
	if b.Lifecycle != lifecycleOneShot {
		t.Fatalf("binding.Lifecycle = %q, want %q — confirmLaunched must carry gc's wire value forward", b.Lifecycle, lifecycleOneShot)
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil || len(allocs) != 1 {
		t.Fatalf("listAllocsForJob(%q) = (%v, %v), want one allocation", b.ChildJobID, allocs, err)
	}
	allocID := allocs[0].ID

	// Simulate the agent's own claim-free remote drain ack landing in the
	// pane's captured transcript — exactly what `tmux pipe-pane` would have
	// written for a real `gc hook --drain-ack --json` success, since that
	// is the agent's own shell output and agentCaptureBody already taps it.
	writeAck := `mkdir -p "$NOMAD_ALLOC_DIR/data" && printf '{"ok":true,"drain_ack":true}\n' >> "$NOMAD_ALLOC_DIR/data/agent.stdout"`
	if exitCode, out, err := l.client.execAlloc(ctx, allocID, execTaskName, []string{"/bin/sh", "-c", writeAck}); err != nil || exitCode != 0 {
		t.Fatalf("writing drain-ack fixture: exit=%d output=%q err=%v", exitCode, out, err)
	}

	running, err = l.opIsRunning(ctx, session)
	if err != nil || running {
		t.Fatalf("is-running after drain-ack = (%v, %v), want (false, nil) — a one_shot seat that already "+
			"acknowledged drain must not hold its slot", running, err)
	}

	if _, ok, err := l.sidecar.load(session); err != nil || ok {
		t.Fatalf("sidecar binding after retirement: ok=%v err=%v, want removed exactly like any other stop", ok, err)
	}

	allocs, _, err = l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil || len(allocs) != 1 {
		t.Fatalf("listAllocsForJob after retirement = (%v, %v), want one allocation", allocs, err)
	}
	if !isTerminalStatus(allocs[0].ClientStatus) {
		t.Fatalf("alloc ClientStatus after retirement = %q, want a terminal status — the alloc must reach complete", allocs[0].ClientStatus)
	}
}

// TestOpIsRunningIgnoresDrainAckMarkerForPoolLifecycle proves the mechanism
// is gated on lifecycle==one_shot and cannot misfire on an ordinary pool
// session that happens to have printed the same text (a worker discussing
// drain-ack, or quoting it, is not the same as a one_shot seat's own
// terminal step).
func TestOpIsRunningIgnoresDrainAckMarkerForPoolLifecycle(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-pool-drain-marker"

	if err := l.opStartWithConfig(ctx, session, stageConfig{Command: testPaneCommand}); err != nil {
		t.Fatalf("start: %v", err)
	}
	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("load binding: (%v, %v, %v)", b, ok, err)
	}
	if b.Lifecycle != "" {
		t.Fatalf("binding.Lifecycle = %q, want empty for a caller that sent no lifecycle", b.Lifecycle)
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil || len(allocs) != 1 {
		t.Fatalf("listAllocsForJob(%q) = (%v, %v), want one allocation", b.ChildJobID, allocs, err)
	}
	allocID := allocs[0].ID

	writeAck := `mkdir -p "$NOMAD_ALLOC_DIR/data" && printf '{"ok":true,"drain_ack":true}\n' >> "$NOMAD_ALLOC_DIR/data/agent.stdout"`
	if exitCode, out, err := l.client.execAlloc(ctx, allocID, execTaskName, []string{"/bin/sh", "-c", writeAck}); err != nil || exitCode != 0 {
		t.Fatalf("writing drain-ack fixture: exit=%d output=%q err=%v", exitCode, out, err)
	}

	running, err := l.opIsRunning(ctx, session)
	if err != nil || !running {
		t.Fatalf("is-running for a non-one_shot session with the marker present = (%v, %v), want (true, nil) — "+
			"only lifecycle=one_shot may be retired this way", running, err)
	}
}

// TestDrainAckMarkerEnvOverride proves the deployment override door works —
// the exact `--json` success text is the server's own wire contract, not
// this pack's, and an operator must be able to repoint the marker without a
// pack rebuild if it is ever reworded upstream.
func TestDrainAckMarkerEnvOverride(t *testing.T) {
	t.Setenv(drainAckMarkerEnv, "TOTALLY_CUSTOM_MARKER")
	re, err := drainAckMarker()
	if err != nil {
		t.Fatalf("drainAckMarker: %v", err)
	}
	if !re.MatchString("saw TOTALLY_CUSTOM_MARKER here") {
		t.Fatalf("override regex %q did not match its own literal", re.String())
	}
}

// TestDrainAckMarkerRejectsInvalidOverride proves a typo'd override fails
// loud (returned as an error the caller logs) rather than silently falling
// back to a default the operator no longer controls.
func TestDrainAckMarkerRejectsInvalidOverride(t *testing.T) {
	t.Setenv(drainAckMarkerEnv, "(unterminated")
	if _, err := drainAckMarker(); err == nil {
		t.Fatalf("drainAckMarker with an invalid override = nil error, want a compile error")
	}
}

// TestDrainAckGrepPatternStripsGoOnlyCaseFlag proves the shelled-out grep
// pattern is actual POSIX ERE, not Go regexp syntax `grep -E` cannot parse
// — the exact bug this pinned: `(?i)` embedded verbatim in the pattern text
// makes `grep -E` match nothing, ever, silently.
func TestDrainAckGrepPatternStripsGoOnlyCaseFlag(t *testing.T) {
	pattern, ci := drainAckGrepPattern(defaultDrainAckMarker)
	if !ci {
		t.Fatalf("drainAckGrepPattern(defaultDrainAckMarker) case-insensitive = false, want true")
	}
	if pattern == "" || pattern[0] == '(' {
		t.Fatalf("drainAckGrepPattern(defaultDrainAckMarker) pattern = %q, want the Go-only (?i) prefix stripped", pattern)
	}

	caseSensitive := regexp.MustCompile("EXACT")
	pattern, ci = drainAckGrepPattern(caseSensitive)
	if ci {
		t.Fatalf("drainAckGrepPattern(%q) case-insensitive = true, want false for a pattern with no (?i)", caseSensitive)
	}
	if pattern != "EXACT" {
		t.Fatalf("drainAckGrepPattern(%q) pattern = %q, want unchanged", caseSensitive, pattern)
	}
}

// TestDefaultDrainAckMarkerMatchesClaimFreeAckJSON pins the default regex
// against the shapes it must and must not match.
func TestDefaultDrainAckMarkerMatchesClaimFreeAckJSON(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"canonical", `{"ok":true,"drain_ack":true}`, true},
		{"spaced", `{"drain_ack": true}`, true},
		{"upper snake case", `{"DRAIN_ACK":true}`, true},
		{"camelCase key is a different field, not this one", `{"DrainAck":true}`, false},
		{"false is not an ack", `{"drain_ack":false}`, false},
		{"absent", `{"claimed":false}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := defaultDrainAckMarker.MatchString(c.text); got != c.want {
				t.Errorf("defaultDrainAckMarker.MatchString(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}
