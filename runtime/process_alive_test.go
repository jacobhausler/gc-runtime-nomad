package main

import (
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
