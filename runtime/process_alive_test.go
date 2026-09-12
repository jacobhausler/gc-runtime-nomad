package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// process-alive is the one RPP op where a WRONG "false" is catastrophic rather
// than merely unhelpful: gc's caller treats empty output and "true" alike as
// alive and only an explicit "false" as a dead agent, and it uses that answer
// to reap sessions. So an unmeasurable box must never be able to answer dead.
//
// That is why every non-observation path here returns alive:
//
//	transport error       nothing was observed
//	`ps` exited nonzero   no ps in the image — unmeasurable, not dead
//	no allocation         a different fault with its own message
//	no names asked about  nothing to disprove (the interface contract)

func TestProcessListMatchesOnBasename(t *testing.T) {
	ps := []byte("COMMAND\nsleep\ntmux: server\n/opt/codex/bin/codex\nsh\n")
	if !processListHasAny(ps, []string{"codex"}) {
		t.Error("a listing printing a full path must match a name given bare; otherwise a healthy " +
			"agent reads as dead and gets reaped")
	}
	if !processListHasAny(ps, []string{"sleep", "codex"}) {
		t.Error("any one of the wanted names matching is enough")
	}
	if processListHasAny(ps, []string{"claude"}) {
		t.Error("a name that is genuinely absent must not match")
	}
	// The header must never count as a process.
	if processListHasAny([]byte("COMMAND\n"), []string{"COMMAND"}) {
		t.Error("the ps header was counted as a running process")
	}
	// Blank and empty listings prove nothing is there.
	if processListHasAny([]byte("\n  \n"), []string{"codex"}) {
		t.Error("a blank listing must not match")
	}
}

// TestProcessAliveOpAnswersTrueWhenUnobserved is the honesty split, asserted at
// the op's wiring. A source assertion, because the behavioural path needs a
// live allocation — written to fail on the specific regression.
func TestProcessAliveOpAnswersTrueWhenUnobserved(t *testing.T) {
	src := readSourceFile(t, "main.go")
	i := strings.Index(src, `case "process-alive":`)
	if i < 0 {
		t.Fatal("the process-alive op is gone; conformance counted it as implemented")
	}
	body := src[i:]
	if end := strings.Index(body, "\n\tcase "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "if !observed {") {
		t.Error("the op no longer distinguishes an unobserved probe from an observed absence; an " +
			"image without `ps`, or a transport fault, would answer \"false\" and gc would reap a " +
			"live session")
	}
	if !strings.Contains(body, "io.ReadAll(stdin)") {
		t.Error("the op does not read process names from stdin, which is the caller's contract")
	}

	// And the decision function must be reachable from lifecycleOps, or the op
	// exits 2 and conformance skips it while the code sits there looking done.
	if !strings.Contains(src, `"process-alive":    true,`) {
		t.Error("process-alive is not in lifecycleOps, so the op exits 2 and is never dispatched")
	}
}

// TestNoNamesMeansAlive pins the interface contract's own default.
func TestNoNamesMeansAlive(t *testing.T) {
	l := &lifecycle{}
	alive, observed, err := l.namedProcessAlive(nil, "s", []string{"", "   "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !alive {
		t.Error("no process names asked about means nothing to disprove; must be alive")
	}
	if observed {
		t.Error("nothing was observed, so observed must be false — otherwise a later change could " +
			"start reporting a definitive answer from no evidence")
	}
}

// TestSkillDocumentsTheFailOpenProbe closes the loop on a defect I introduced
// by implementing process-alive well and documenting it not at all.
//
// The op answers `true` whenever it could not measure — no `ps` in the image, a
// transport fault, no allocation. That is the correct choice: gc reaps on an
// explicit `false`, so a probe that cannot see must never be able to kill a
// live seat. But it makes the op a SECOND, independent manufacturer of the
// hardest symptom this pack has — a seat that reports healthy and claims
// nothing — and the shipped runbook had no row for it and did not list it among
// the signals. A third outside SRE found that by reading the source.
//
// A fail-open path that is not on the diagnostic surface is a dark lane with
// extra steps.
func TestSkillDocumentsTheFailOpenProbe(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "skills", "gc-nomad-debug", "SKILL.md"))
	if err != nil {
		t.Fatalf("reading SKILL.md: %v", err)
	}
	doc := string(body)

	if !strings.Contains(doc, "process-alive") {
		t.Fatal("SKILL.md does not mention process-alive at all, though it is in lifecycleOps and " +
			"can report a dead seat as healthy")
	}
	for _, want := range []struct{ needle, why string }{
		{"FAILED OPEN", "the runbook does not say the probe fails open, which is the whole hazard"},
		{"REAPS", "it does not explain WHY it fails open, so a reader may think it a bug and 'fix' it"},
		{"ps", "it does not name the in-box dependency that makes the op unanswerable"},
	} {
		if !strings.Contains(doc, want.needle) {
			t.Errorf("SKILL.md's process-alive coverage lacks %q: %s", want.needle, want.why)
		}
	}

	// The live fail-open behaviour is asserted by
	// TestProcessAliveOpAnswersTrueWhenUnobserved (the op's wiring) and
	// TestNoNamesMeansAlive. Deliberately not re-asserted here with a
	// hand-built lifecycle: namedProcessAlive resolves an allocation through
	// l.client, so a zero-value lifecycle panics rather than exercising the
	// fail-open path. A test that has to construct an invalid object to reach
	// a behaviour is testing the constructor, not the behaviour.
}
