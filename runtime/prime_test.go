package main

import (
	"strings"
	"testing"
)

// TestPrimeScriptSubmitsLikeGcsNudgeSession is the RED/GREEN pair for
// cr-gdeav.6.14. Attempt 7 (pack bfe4378) typed the nudge as a literal paste
// immediately followed by a LONE Enter the instant the ready prefix matched.
// Reproduced live against alloc 12d0aa58 (ops/receipts/nrt-d3/
// 2026-09-07-g3j-nudge.md §11): typed that way at the same ~200ms mark
// (inside Codex's own startup terminal-query/discard phase), the message
// either sat drafted-but-unsubmitted or crashed Codex outright
// ("Error: timed out discarding buffered terminal input", exit 1). gc's own
// local codex nudge path (internal/runtime/tmux/tmux.go: NudgeSession /
// nudgeSubmitKeySequenceForFamily("codex")) never does this: it pastes, waits
// a fixed debounce, then submits with Escape THEN Enter as one unit — codex
// reads a send-keys burst as a bracketed paste, so a trailing lone Enter is a
// composer newline, not a submit. The identical live sequence (paste, 500ms
// debounce, Escape, Enter) worked cleanly twice in a row against the same
// box at the same ~200ms mark, with no extra readiness settle.
//
// This is a fixture-based structural test on the actual generated
// primeScript (the shell run.sh staged into the alloc): a live tmux/Codex
// round trip cannot run in a Go unit test, so it pins the properties the
// live reproduction showed matter, against the real script bytes this pack
// ships.
func TestPrimeScriptSubmitsLikeGcsNudgeSession(t *testing.T) {
	script := primeScript

	pasteIdx := strings.Index(script, `send-keys -t main -l --`)
	if pasteIdx < 0 {
		t.Fatal("primeScript never pastes the nudge text literally (send-keys -l)")
	}

	escapeIdx := strings.Index(script, `send-keys -t main Escape`)
	enterIdx := strings.Index(script, `send-keys -t main Enter`)
	if escapeIdx < 0 || enterIdx < 0 {
		t.Fatalf("primeScript must submit with Escape then Enter (codex's real submit sequence), got escape=%d enter=%d", escapeIdx, enterIdx)
	}
	if escapeIdx > enterIdx {
		t.Fatalf("primeScript sends Enter (%d) before Escape (%d); codex's submit sequence is Escape THEN Enter", enterIdx, escapeIdx)
	}
	if escapeIdx < pasteIdx {
		t.Fatalf("primeScript sends Escape (%d) before the paste (%d)", escapeIdx, pasteIdx)
	}

	// The regression: Enter landing immediately after the paste, with no
	// debounce and no Escape in between, is exactly what crashed Codex live.
	between := script[pasteIdx:enterIdx]
	if !strings.Contains(between, "sleep") {
		t.Fatalf("primeScript sends the submit sequence with no debounce between the paste and Enter: %q", between)
	}
	if !strings.Contains(between, "Escape") {
		t.Fatalf("primeScript's first Enter after the paste is not preceded by Escape (a lone Enter is what crashed Codex live): %q", between)
	}

	// Submission must be confirmed against codex's own busy indicator before
	// this script ever calls itself done, and it must be prepared to
	// re-submit (without re-pasting) a bounded number of times while the
	// pane stays idle - gc's submitEnterAndConfirm contract.
	if !strings.Contains(script, "esc to interrupt") {
		t.Fatal("primeScript never confirms delivery against codex's busy indicator (\"esc to interrupt\")")
	}
	if !strings.Contains(script, "pane_busy") {
		t.Fatal("primeScript has no pane_busy confirmation check at all")
	}
	if got := strings.Count(script, "send-keys -t main Escape"); got < 1 {
		t.Fatalf("primeScript's Escape send count = %d, want at least 1", got)
	}

	// Never re-submit into a pane that already went busy - a second Escape
	// there is an INTERRUPT to a codex turn already running (documented on
	// gc's own RE-SEND HAZARD, tmux.go).
	sendLoopIdx := strings.Index(script, "while [ \"$send\"")
	if sendLoopIdx < 0 || sendLoopIdx > escapeIdx {
		t.Fatal("primeScript's resend loop must wrap the submit sequence, guarding every re-send with a pane_busy re-check")
	}
}
