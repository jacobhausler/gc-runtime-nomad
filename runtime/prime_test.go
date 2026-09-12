package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPrimeScriptSubmitsLikeGcsNudgeSession is the RED/GREEN pair for a
// measured crash. Attempt 7 (pack bfe4378) typed the nudge as a literal paste
// immediately followed by a LONE Enter the instant the ready prefix matched.
// Reproduced live against alloc 12d0aa58: typed that way at the same ~200ms mark
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

	pasteIdx := strings.Index(script, `tmux paste-buffer -p -d -t main -b gc-prime-nudge`)
	if pasteIdx < 0 {
		t.Fatal("primeScript never pastes the staged nudge with tmux's own paste path (load-buffer + paste-buffer)")
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

// TestPrimeStatusHonorsUnconfirmedSubmit pins syncPromptedMarker's other
// half: a detached window that pasted and submitted but never
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
// pair for a third crash. Attempt 9 (pack db669c7) crashed again:
// agent.exit exit=1, stderr "Error: timed out discarding buffered terminal
// input", gc-prime/status=submit_unconfirmed, prompted_at 4s after start.
// Captured agent.stdout (fetched live from alloc 076519d8 before it was
// GC'd, testdata/codex-loading-not-ready.raw, trimmed to the first TUI
// frame) shows WHY: Codex paints "› Ask Codex to do anything" in the SAME
// first frame as "model: loading" / "directory: loading" - the ready prefix
// is present from the very first frame, well before the agent has actually
// finished initializing, so a prefix-only match (every prior attempt's
// rule) fires immediately and types into Codex's startup
// terminal-query/discard window.
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
		t.Fatal("primeScript has no pane_ready() function to extract - the readiness predicate must be its own named, testable unit")
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

// TestPrimeScriptHonorsReadyDelayAsPostReadySettle pins that readiness
// rule's second half: ready_delay_ms.txt (staged from cfg.ReadyDelayMs, defaulting
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
	pasteIdx := strings.Index(primeScript, `tmux paste-buffer -p -d -t main -b gc-prime-nudge`)
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

// primePasteCommand is the delivery command primeScript must use: tmux's own
// paste path, reading the staged file and deleting the buffer as it pastes.
const primePasteCommand = "tmux paste-buffer -p -d -t main -b gc-prime-nudge"

// TestPrimeScriptCarriesNudgeOffTheArgumentList is the RED/GREEN pair for a
// measured delivery failure. The worker-local charter renders a 20,857-byte startup prompt, and
// tmux 3.7b refuses any single argument above ~16 KiB: `send-keys -l -- <text>`
// died with "command too long" (rc=1) at prime.go:181, so the seat was left at
// "Ask Codex to do anything", owned no issues, and every observer - including
// gc's own session record - still saw a launched, primed session. Measured on
// tmux 3.7b (the version in gc-agent:b55e229e1): a 16,000-byte argument lands,
// 16,384 and 20,857 are refused, and load-buffer + paste-buffer delivers all
// 20,858 bytes with the buffer gone afterwards.
//
// This is a structural test on the real generated primeScript, the same shape
// TestPrimeScriptSubmitsLikeGcsNudgeSession uses: what it pins is that the text
// never crosses the tmux argument boundary, and that a delivery which cannot be
// verified fails CLOSED (status nudge_failed, no prompted_at marker).
func TestPrimeScriptCarriesNudgeOffTheArgumentList(t *testing.T) {
	script := primeScript

	if strings.Contains(script, `send-keys -t main -l`) {
		t.Fatal("primeScript still carries the nudge as a send-keys ARGUMENT - tmux 3.7b refuses an argument over ~16 KiB, so a charter-sized prompt silently delivers nothing")
	}
	if !strings.Contains(script, `tmux load-buffer -b gc-prime-nudge "$DIR/nudge.txt"`) {
		t.Fatal(`primeScript must hand the nudge to tmux as a FILE (load-buffer -b gc-prime-nudge "$DIR/nudge.txt")`)
	}
	pasteIdx := strings.Index(script, primePasteCommand)
	if pasteIdx < 0 {
		t.Fatalf("primeScript must paste with %s: -p preserves the prompt's line feeds, -d deletes the buffer as it pastes", primePasteCommand)
	}
	loadIdx := strings.Index(script, `load-buffer -b gc-prime-nudge`)
	if loadIdx > pasteIdx {
		t.Fatalf("load-buffer (%d) must run before paste-buffer (%d)", loadIdx, pasteIdx)
	}
	// The buffer must not linger: -d consumes it on the success path, and an
	// explicit delete-buffer has to cover a paste that died mid-way.
	if !strings.Contains(script, `tmux delete-buffer -b gc-prime-nudge`) {
		t.Fatal("primeScript has no explicit delete-buffer fallback for a failed paste")
	}
	deleteIdx := strings.Index(script, `tmux delete-buffer -b gc-prime-nudge`)
	if deleteIdx < pasteIdx {
		t.Fatalf("the delete-buffer fallback (%d) must come after the paste (%d)", deleteIdx, pasteIdx)
	}

	// Fail closed: the delivery is only accepted when the pane actually
	// changed, and every nudge_failed path must exit BEFORE any prompted_at
	// marker exists. A dark lane that records success is the defect this pins
	// out - it is what made the gate "a seat that starts and prompts is not a
	// migrated lane" necessary.
	if !strings.Contains(script, `cmp -s "$BEFORE" "$AFTER"`) {
		t.Fatal(`primeScript accepts the delivery without comparing the pane before and after the paste (cmp -s "$BEFORE" "$AFTER")`)
	}
	// Match the STATEMENT, not the bare word. The script explains itself in a
	// comment that necessarily contains both "nudge_failed" and "prompted_at",
	// and a bare-word scan reads that prose as a fourth failure branch whose
	// next `exit 0` lies past the real prompted_at write - a false positive on
	// the fix's own documentation. Same shape as the machinery guard's
	// name-vs-invocation family: a NAME is not a STATEMENT.
	for i, idx := range allIndexes(script, `printf 'nudge_failed\n' > "$DIR/status"`) {
		end := strings.Index(script[idx:], `exit 0`)
		if end < 0 {
			t.Fatalf("nudge_failed branch %d never exits", i)
		}
		if strings.Contains(script[idx:idx+end], "prompted_at") {
			t.Fatalf("nudge_failed branch %d writes prompted_at - a failed prime must not claim it primed the session", i)
		}
	}
}

func allIndexes(haystack, needle string) []int {
	var out []int
	for off := 0; ; {
		i := strings.Index(haystack[off:], needle)
		if i < 0 {
			return out
		}
		out = append(out, off+i)
		off += i + len(needle)
	}
}

// TestTmuxArgumentCapMakesFilePasteTheOnlyNudgeDelivery encodes the bisect that
// diagnosed it, against a real tmux rather than as a comment: the
// literal-argument form lands a 16,000-byte nudge and is REFUSED at charter
// size, while the load-buffer/paste-buffer pair the pack now ships delivers the
// whole 20,858-byte payload and leaves no buffer behind.
//
// tmux runs on a private socket owned by the test - never the default server,
// which is where live city panes live - and the reader pane is `cat > file`,
// the standing-in for an agent that consumes stdin. The prompt is newline
// terminated here on purpose: an unterminated tail line stays in the tty's
// canonical buffer unread, which is a property of the reader, not of delivery.
func TestTmuxArgumentCapMakesFilePasteTheOnlyNudgeDelivery(t *testing.T) {
	requireTmuxForDriverTest(t)

	dir := t.TempDir()
	// One reader per arm; see the comment at new-session below.
	capOut := filepath.Join(dir, "landed-cap.txt")
	paneOut := filepath.Join(dir, "landed-paste.txt")

	// The socket does NOT live in t.TempDir(). A unix socket path is capped at
	// ~104 bytes on macOS (108 on Linux) and t.TempDir() embeds the test's own
	// name, which for this test is long enough to blow the cap by itself:
	//     error connecting to /var/folders/.../TestTmuxArgumentCapMakesFilePasteTheOn...
	// so on macOS this failed at new-session before measuring anything. This is
	// purely a path-length fix; it changes nothing about what the test asserts.
	sockDir, err := os.MkdirTemp("", "gcp")
	if err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "p.sock")

	// The measured worker-local prompt size (ops/receipts/nrt-d3/
	// 2026-09-11-d3-cutover-landed-gate-unmet.md), newline terminated.
	nudge := "HEADMARKER\n" + strings.Repeat("LINE\n", 3280) + "TAILMARKER-liv6ac\n"
	nudgeFile := filepath.Join(dir, "nudge.txt")
	if err := os.WriteFile(nudgeFile, []byte(nudge), 0o600); err != nil {
		t.Fatalf("write nudge: %v", err)
	}
	if len(nudge) < 16384 {
		t.Fatalf("the fixture must sit above tmux's argument cap, got %d bytes", len(nudge))
	}

	tmux := func(args ...string) (string, error) {
		cmd := exec.Command("tmux", append([]string{"-S", sock}, args...)...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { _, _ = tmux("kill-server") })

	// EACH ARM GETS ITS OWN PANE, and that is load-bearing rather than tidy.
	//
	// The argument-cap probe below types 16,000 bytes with NO trailing newline.
	// That overflows the tty line discipline's canonical buffer (MAX_CANON),
	// which then DISCARDS everything that follows until the line is consumed -
	// and `cat` never consumes it, because it has no newline to read. Measured
	// 2026-09-12 on macOS: after that probe the reader pane wrote 0 bytes, and
	// the paste that followed into the SAME pane also landed 0 bytes, while the
	// identical paste into a FRESH pane landed all 16,429 with its tail intact.
	//
	// So a shared reader made this test assert that paste-buffer delivers
	// nothing - a false red against working delivery. Truncating the shared
	// file between arms does not fix it either: `cat` holds the file open and
	// its offset survives truncation, leaving a sparse hole rather than a clean
	// reader.
	if out, err := tmux("new-session", "-d", "-s", "gc-nudge-cap", "cat > "+capOut); err != nil {
		t.Fatalf("new-session on the private socket: %v (%s)", err, out)
	}

	// The boundary, measured rather than remembered. tmux tightened its
	// per-argument ceiling between 3.5a and 3.7b, so the refusal is recorded as
	// an observation of THIS tmux rather than asserted: the property that must
	// hold on every version is the one below it - the file path delivers the
	// whole payload. On gc-agent:b55e229e1 (tmux 3.7b, the box this pack runs
	// on) the argument form is refused and that observation is the defect.
	if out, err := tmux("send-keys", "-t", "gc-nudge-cap", "-l", "--", strings.Repeat("x", 16000)); err != nil {
		t.Fatalf("a 16000-byte send-keys argument should land on any tmux: %v (%s)", err, out)
	}
	if out, err := tmux("send-keys", "-t", "gc-nudge-cap", "-l", "--", nudge); err != nil {
		t.Logf("this tmux refuses a %d-byte send-keys argument (%s) - the measured shape", len(nudge), strings.TrimSpace(out))
		if probe, rerr := os.ReadFile(capOut); rerr == nil && strings.Contains(string(probe), "TAILMARKER") {
			t.Fatal("the refused send-keys still delivered its tail")
		}
	} else {
		t.Logf("this tmux accepts a %d-byte send-keys argument - the ceiling landed after this version; the file path below is still the required form", len(nudge))
	}

	// The delivery arm, on a reader the probe above has not touched.
	if out, err := tmux("new-session", "-d", "-s", "gc-nudge-probe", "cat > "+paneOut); err != nil {
		t.Fatalf("new-session for the delivery arm: %v (%s)", err, out)
	}

	if out, err := tmux("load-buffer", "-b", "gc-prime-nudge", nudgeFile); err != nil {
		t.Fatalf("load-buffer on the same payload: %v (%s)", err, out)
	}
	if out, err := tmux("paste-buffer", "-p", "-d", "-t", "gc-nudge-probe", "-b", "gc-prime-nudge"); err != nil {
		t.Fatalf("paste-buffer on the same payload: %v (%s)", err, out)
	}

	var landed []byte
	deadline := time.Now().Add(15 * time.Second)
	for {
		b, err := os.ReadFile(paneOut)
		if err == nil && len(b) == len(nudge) {
			landed = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pane never received the whole payload: got %d of %d bytes (read err %v)", len(b), len(nudge), err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(string(landed), "TAILMARKER-liv6ac") {
		t.Fatal("the pasted payload lost its tail - the prompt arrived truncated")
	}
	if strings.Count(string(landed), "\n") != strings.Count(nudge, "\n") {
		t.Fatal("paste-buffer collapsed the prompt's line feeds (missing -p?)")
	}
	if out, _ := tmux("list-buffers"); strings.Contains(out, "gc-prime-nudge") {
		t.Fatalf("paste-buffer -d left a 20 KB prompt in the box's tmux buffer state: %s", out)
	}
}
