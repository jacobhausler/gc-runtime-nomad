package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

// TestPrimeStatusHonorsUnconfirmedSubmit pins syncPromptedMarker's other half
// of cr-gdeav.6.14: a detached window that pasted and submitted but never
// observed the busy indicator writes prompted_at (the keystrokes did reach
// tmux, gc's own "handed to tmux" best-effort contract) alongside
// status=submit_unconfirmed, and that status word - not a hardcoded
// "delivered" - is what must land on the binding.
func TestPrimeStatusHonorsUnconfirmedSubmit(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-unconfirmed-submit"

	fakeBin := t.TempDir()
	fakeTmux := filepath.Join(fakeBin, "tmux")
	if err := os.WriteFile(fakeTmux, []byte(`#!/bin/sh
case "$1" in
new-session|has-session) exit 0 ;;
list-panes) exit 1 ;;
list-windows) printf 'main\n' ;;
new-window) exit 0 ;;
*) exit 1 ;;
esac
`), 0o700); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := stageConfig{Nudge: "hi", ReadyPromptPrefix: "› Ask Codex"}
	if err := l.opProvisionWithConfig(ctx, session, cfg); err != nil {
		t.Fatalf("provision: %v", err)
	}
	launch := []string{"/bin/sh", "-c", `'tmux' 'new-session' '-d' '-s' 'main' 'sleep 60'`}
	if code, out, err := l.opExec(ctx, session, launch); err != nil || code != 0 {
		t.Fatalf("exec transport launch = (%d, %q, %v)", code, out, err)
	}

	// Simulate the detached window's other outcome: it pasted and submitted,
	// but never observed codex's busy indicator within its confirm budget.
	if code, out, err := l.opExec(ctx, session, []string{"/bin/sh", "-c",
		`date -u +%Y-%m-%dT%H:%M:%SZ > "$NOMAD_SECRETS_DIR/gc-prime/prompted_at" && printf '` + promptStatusSubmitUnconfirmed + `' > "$NOMAD_SECRETS_DIR/gc-prime/status"`}); err != nil || code != 0 {
		t.Fatalf("write marker: (%d, %q, %v)", code, out, err)
	}

	running, err := l.opIsRunning(ctx, session)
	if err != nil || !running {
		t.Fatalf("opIsRunning = (%v, %v), want (true, nil)", running, err)
	}

	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("sidecar.load = (%+v, %v, %v)", b, ok, err)
	}
	if b.PromptedAt == nil {
		t.Fatal("an unconfirmed-but-submitted attempt must still record prompted_at (keystrokes reached tmux)")
	}
	if b.PromptStatus != promptStatusSubmitUnconfirmed {
		t.Fatalf("prompt_status = %q, want %q (the status file's own word, not a hardcoded \"delivered\")", b.PromptStatus, promptStatusSubmitUnconfirmed)
	}
}

// extractPaneReadyFunc pulls the pane_ready() shell function verbatim out of
// the real, shipped primeScript, so a test of "the readiness predicate" is a
// test of the exact bytes staged into an alloc, not a hand-copied
// reimplementation that could silently drift from it.
func extractPaneReadyFunc(script string) (string, bool) {
	start := strings.Index(script, "pane_ready() {")
	if start < 0 {
		return "", false
	}
	rest := script[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return "", false
	}
	return rest[:end+len("\n}\n")], true
}

// runPaneReady runs the extracted pane_ready() against fixture, under the
// given ready-prefix, and reports whether it decided "ready" (exit 0).
func runPaneReady(t *testing.T, fn, prefix string, fixture []byte) bool {
	t.Helper()
	paneFile := filepath.Join(t.TempDir(), "pane.txt")
	// pane_ready reads real `tmux capture-pane -p` output: already-rendered
	// lines, not the raw TUI repaint stream a captured agent.stdout holds
	// (startup_ready_pane_test.go's own fixture note) - so the fixture is
	// reconstructed into rows first, exactly as the live pane would render.
	rendered := strings.Join(paneReadyLines(string(fixture)), "\n")
	if err := os.WriteFile(paneFile, []byte(rendered), 0o600); err != nil {
		t.Fatalf("write reconstructed pane fixture: %v", err)
	}
	script := "PREFIX=" + shellQuote(prefix) + "\n" + fn + "\npane_ready " + shellQuote(paneFile) + "\n"
	cmd := exec.Command("/bin/sh", "-c", script)
	return cmd.Run() == nil
}

// TestPrimeReadinessRejectsLoadingFrameAcceptsLoadedFrame is the RED/GREEN
// pair for cr-gdeav.6.16. Attempt 9 (pack db669c7) crashed again: agent.exit
// exit=1, stderr "Error: timed out discarding buffered terminal input",
// gc-prime/status=submit_unconfirmed, prompted_at 4s after start. Captured
// agent.stdout (fetched live from alloc 076519d8 before it was GC'd,
// testdata/codex-loading-not-ready.raw, trimmed to the first TUI frame) shows
// WHY: Codex paints "› Ask Codex to do anything" in the SAME first frame as
// "model:     loading" / "directory: loading" - the ready prefix is present
// from the very first frame, well before the agent has actually finished
// initializing, so a prefix-only match (every prior attempt's rule) fires
// immediately and types into Codex's startup terminal-query/discard window.
//
// pane_ready() must reject that frame (prefix present, but "loading" is
// still on screen) and accept testdata/codex-loaded-ready.raw's frame - the
// FINAL redraw sliced out of attempt 4's own capture
// (testdata/codex-startup-pane.raw, already in the tree and used by
// startup_ready_pane_test.go), once the model has resolved: "model:
// qwen3.8-27b-nvfp4 medium", "directory: /alloc/work", "permissions: YOLO
// mode", no "loading" anywhere. It has to be sliced to the final redraw
// rather than reusing the whole multi-frame capture verbatim: that capture
// is the TUI's entire repaint HISTORY, "loading" mentions from its own
// earlier frames included, whereas a real `tmux capture-pane -p` (what
// pane_ready actually reads in the box) only ever shows the CURRENT frame.
func TestPrimeReadinessRejectsLoadingFrameAcceptsLoadedFrame(t *testing.T) {
	fn, ok := extractPaneReadyFunc(primeScript)
	if !ok {
		t.Fatal("primeScript has no pane_ready() function to extract - cr-gdeav.6.16's readiness predicate must be its own named, testable unit")
	}

	loading, err := os.ReadFile("testdata/codex-loading-not-ready.raw")
	if err != nil {
		t.Fatalf("read attempt-9 loading fixture: %v", err)
	}
	loaded, err := os.ReadFile("testdata/codex-loaded-ready.raw")
	if err != nil {
		t.Fatalf("read attempt-4 loaded fixture: %v", err)
	}

	const prefix = "› Ask Codex"
	if runPaneReady(t, fn, prefix, loading) {
		t.Fatal("pane_ready accepted attempt 9's frame as ready, though model:/directory: still say \"loading\" - this is exactly what typed into Codex's startup discard window and crashed it")
	}
	if !runPaneReady(t, fn, prefix, loaded) {
		t.Fatal("pane_ready rejected attempt 4's fully-loaded frame - the rule must still accept a genuinely ready pane")
	}
}

// TestPrimeScriptHonorsReadyDelayAsPostReadySettle pins cr-gdeav.6.16's
// second half: ready_delay_ms.txt (staged from cfg.ReadyDelayMs, defaulting
// to primeDefaultReadyDelayMs when the wire sends none or zero) is read and
// applied as a settle AFTER the stability-gated readiness above, not as a
// pre-readiness fallback the way gc's own ReadyDelayMs is used locally
// (WaitForRuntimeReady) - a strict superset, documented in prime.go.
func TestPrimeScriptHonorsReadyDelayAsPostReadySettle(t *testing.T) {
	if !strings.Contains(primeScript, `if [ -f "$DIR/ready_delay_ms.txt" ]`) {
		t.Fatal("primeScript never reads a staged ready_delay_ms.txt")
	}
	settleIdx := strings.Index(primeScript, `sleep "$settle"`)
	readyIdx := strings.Index(primeScript, `ready=1`)
	pasteIdx := strings.Index(primeScript, `send-keys -t main -l --`)
	if settleIdx < 0 {
		t.Fatal(`primeScript never sleeps "$settle" (the ready_delay_ms settle) at all`)
	}
	if settleIdx < readyIdx {
		t.Fatalf("the ready_delay_ms settle (%d) runs before readiness is even decided (%d) - it must be a POST-ready settle", settleIdx, readyIdx)
	}
	if settleIdx > pasteIdx {
		t.Fatalf("the ready_delay_ms settle (%d) runs after the nudge is already pasted (%d) - it must settle BEFORE typing anything", settleIdx, pasteIdx)
	}
	if got := readyDelayMs(0); got != primeDefaultReadyDelayMs {
		t.Fatalf("readyDelayMs(0) = %d, want the documented default %d", got, primeDefaultReadyDelayMs)
	}
	if got := readyDelayMs(4000); got != 4000 {
		t.Fatalf("readyDelayMs(4000) = %d, want the wire value honored verbatim", got)
	}
}
