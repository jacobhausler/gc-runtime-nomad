package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Detached in-box startup priming.
//
// Attempt 6 (pack 6d13b8f) looped: supervisor.log showed op=start cycling
// provider_error(~5s) -> stale_async_start(~30.5-30.7s), with a FRESH dispatch
// every cycle. Cause: markTransportLaunched/markLaunched had started calling
// the blocking startupPrime (startup.go) INSIDE the launch op, so a cold
// Codex boot pushed the op well past gc's ~30s async-start budget; gc marked
// the start stale and retried, the pack's own adopt-or-reap gate then saw a
// binding it did not recognize as this attempt's and reaped it, and the next
// attempt dispatched a brand new box and repeated the cycle forever.
//
// The fix moves the wait entirely INSIDE the allocation, on its own clock,
// so the launch op goes back to returning the instant launch is confirmed
// (its pre-priming behavior). This file stages a tiny polling script plus
// the resolved nudge text into $NOMAD_SECRETS_DIR/gc-prime and starts it in
// a second tmux window against the SAME session: `tmux new-window` is a
// client verb that starts the window and returns immediately, parented to
// the tmux server inside the task cgroup — exactly how the launch itself
// already survives the exec call that started it (the launch builder's own doc
// comment). No cross-network polling and no long-lived pack process are
// needed: gc execs this binary fresh per op, so there is nothing here that
// could outlive an op anyway; the window is the only thing that can.
//
// The window polls tmux capture-pane -p for the trimmed ready prefix (the
// same rule paneShowsReadyPrompt pins, applied in shell rather than Go since
// nothing survives in this process to run Go code), types the nudge once,
// and drops prompted_at/status marker files. syncPromptedMarker reads those
// back into the sidecar binding on the next opportunity gc already gives it
// (opIsRunning) — no extra polling loop of its own, and it costs nothing
// once a session is either primed or was never owed a prompt.
const (
	primeDirName    = "gc-prime"
	primeWindowName = "gc-prime"
	primeScriptFile = "run.sh"
	primeNudgeFile  = "nudge.txt"
	primePrefixFile = "ready_prefix.txt"
	primeDelayFile  = "ready_delay_ms.txt"
	// primeLoadingFile and primeBusyFile carry the two TUI markers the prime
	// script polls for. They are staged like ready_prefix.txt rather than baked
	// into the script because they are PROVIDER facts, not runtime facts: a
	// different agent family paints a different "still loading" line and a
	// different busy indicator.
	primeLoadingFile  = "loading_marker.txt"
	primeBusyFile     = "busy_marker.txt"
	primeMarkerFile   = "prompted_at"
	primeStatusFile   = "status"
	primePaneTailFile = "pane_tail"
	primeReadyBudget  = 180 // seconds; mirrors startupReadyBudgetDefault
	primeInterval     = 2   // seconds; mirrors startupReadyIntervalDefault

	// primeDefaultReadyDelayMs is the settle applied AFTER readiness when the
	// wire carries no ready_delay_ms. This is a different
	// role than gc's own ReadyDelayMs, which is a PRE-readiness fallback
	// delay used only when no ready_prompt_prefix is configured at all
	// (WaitForRuntimeReady, tmux.go:3388-3400) - a strict superset of that
	// semantics, documented rather than silently diverging: here it is
	// always honored, after the readiness rule below is satisfied, whether
	// or not a prefix is configured, because attempt 9 showed readiness
	// itself is not the same instant as safe-to-type (below).
	primeDefaultReadyDelayMs = 1500

	// Submit tuning below is gc's own (internal/runtime/tmux/tmux.go,
	// nudgeSubmitDebounce / submitEnterAndConfirm's consts), copied verbatim
	// rather than guessed at. Live evidence is why this
	// matters: attempt 7 (pack bfe4378) typed the nudge as literal text
	// followed by a LONE Enter the instant the ready prefix matched — which
	// on this box is ~200ms after launch, well inside Codex's own startup
	// terminal-query/discard phase (OSC 10/11, CSI 6n, kitty keyboard query,
	// captured at the very head of agent.stdout) — and Codex exited 1,
	// "timed out discarding buffered terminal input". Reproduced live against
	// alloc 12d0aa58: the same immediate lone-Enter sequence either
	// stranded the text drafted-but-unsubmitted or crashed the agent; gc's
	// own sequence — literal paste, THEN a 500ms debounce, THEN Escape
	// then Enter as ONE submit unit, confirmed by polling for codex's busy
	// indicator ("esc to interrupt") — worked cleanly twice in a row, typed
	// at the same ~200ms mark with no extra readiness settle beyond the
	// debounce gc already applies. Codex's TUI reads a send-keys burst as a
	// bracketed paste, so a lone trailing Enter is swallowed as a composer
	// newline, not a submit (nudgeSubmitKeySequenceForFamily("codex") =
	// {"Escape","Enter"}, tmux.go). The debounce is gc's ordinary paste
	// settle time, not a bespoke wait for this pack's own race.
	primeSubmitDebounceMs    = 500
	primeSubmitMaxSends      = 3
	primeSubmitBackoffMs     = 200
	primeConfirmPollsPerSend = 4
	primeConfirmPollMs       = 150
)

// primeScript is the POSIX shell the detached tmux window runs. It is a
// shell-side reimplementation of two pieces of gc's own Go logic, because
// nothing in this pack's own process survives to run Go code once the launch
// op has returned:
//
//   - awaitReady/paneShowsReadyPrompt's core readiness rule (trim the pane
//     line, prefix-match against the configured value) — deliberately
//     without the NBSP-fold and box-border strip the Go matcher carries;
//     those cover TUI rendering edge cases the ready row observed in
//     production does not need, and keeping the script this small keeps it
//     auditable by inspection in the alloc. The script adds two more
//     conditions gc's own rule has no need for, because gc never observed
//     this failure mode locally: Codex paints its ready row
//     ("› Ask Codex to do anything") in the SAME first frame as
//     "model: loading" / "directory: loading" — attempt 9's captured
//     agent.stdout (testdata/codex-loading-not-ready.raw) shows both at
//     once — so a prefix-only match fires while Codex is still inside its
//     startup terminal-query/discard window, and typing there is what
//     killed it (the same crash, again). Readiness is now: the ready
//     prefix row is present, AND the pane contains no "loading" marker
//     (model:/directory: have resolved), AND the capture is IDENTICAL to
//     the previous poll (one full interval of no repaint at all - the
//     screen has actually settled, not merely between two different
//     "loading" frames). Only then is ready_delay_ms honored as a POST-ready
//     settle before anything is typed (primeDefaultReadyDelayMs above if the
//     wire sent none) - a strict superset of gc's own ReadyDelayMs role.
//   - tmux.NudgeSession's submit sequence for the codex family: literal
//     paste, a fixed debounce, then Escape+Enter as the submit unit,
//     confirmed by polling the pane for codex's busy indicator and re-sent
//     (without re-pasting) while the pane stays idle. This pack drives only
//     Codex-shaped TUIs (agent-launch.sh's AGENT_CMD), so it hardcodes the
//     codex family's own sequence rather than reimplementing gc's per-family
//     dispatch (providerEnv / targetLooksLikeProvider) for one family.
const primeScriptTemplate = `#!/bin/sh
set -u
DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
PREFIX=""
if [ -f "$DIR/ready_prefix.txt" ]; then PREFIX="$(cat "$DIR/ready_prefix.txt")"; fi
# The TUI markers. The codex-family defaults live at the USE SITE as
# ${LOADING:-loading} / ${BUSY:-esc to interrupt}, not here, because pane_ready()
# is extracted and run as a standalone unit by its own guard
# (TestPrimeReadinessRejectsLoadingFrameAcceptsLoadedFrame) — a default that
# lived only in this preamble would vanish under extraction and leave grep
# matching the empty string, i.e. rejecting every frame. Defaulting at the use
# site keeps each predicate self-contained.
#
# An absent or empty marker file therefore falls back to the default rather than
# disabling the check: skipping the loading check is fail-OPEN on readiness, and
# typing into a startup discard window is what crashed the agent before this
# readiness rule existed. An absent
# or empty marker file must not disable the check: skipping the loading check is
# fail-OPEN on readiness, and typing into a startup discard window is what
# crashed the agent before this readiness rule existed. A family that paints
# something else overrides these by staging its own value.
LOADING=""
if [ -s "$DIR/loading_marker.txt" ]; then LOADING="$(cat "$DIR/loading_marker.txt")"; fi
BUSY=""
if [ -s "$DIR/busy_marker.txt" ]; then BUSY="$(cat "$DIR/busy_marker.txt")"; fi
DELAY_MS=%[8]d
if [ -f "$DIR/ready_delay_ms.txt" ]; then
  staged="$(cat "$DIR/ready_delay_ms.txt")"
  case "$staged" in ''|*[!0-9]*) : ;; *) DELAY_MS="$staged" ;; esac
fi

# pane_ready FILE: exit 0 when FILE (a tmux capture-pane -p rendering)
# shows the configured ready prefix AND carries no "loading" marker
# (Codex paints its ready row in the SAME frame as
# "model: loading"/"directory: loading", so the prefix alone is not
# enough). PREFIX empty (no ready_prompt_prefix configured) skips the
# prefix half only, matching the Go matcher's own fallback. Named and kept
# this small on purpose: it is the exact unit TestPrimeReadinessRejects...
# extracts and runs against real captured fixtures.
pane_ready() {
  f="$1"
  if [ -n "$PREFIX" ] && ! awk -v p="$PREFIX" '{
      line=$0
      sub(/^[ \t]+/, "", line)
      sub(/[ \t]+$/, "", line)
      if (length(p) <= length(line) && substr(line, 1, length(p)) == p) found=1
    } END { exit !found }' "$f"; then
    return 1
  fi
  if grep -q "${LOADING:-loading}" "$f"; then
    return 1
  fi
  return 0
}

PANEFILE="$DIR/.pane_now"
PREVFILE="$DIR/.pane_prev"
: > "$PREVFILE"
elapsed=0
ready=0
while [ "$elapsed" -le %[1]d ]; do
  tmux capture-pane -p -t main >"$PANEFILE" 2>/dev/null || : >"$PANEFILE"
  if pane_ready "$PANEFILE" && cmp -s "$PANEFILE" "$PREVFILE"; then
    ready=1
    break
  fi
  cp "$PANEFILE" "$PREVFILE"
  sleep %[2]d
  elapsed=$((elapsed + %[2]d))
done

pane_busy() {
  tmux capture-pane -p -t main 2>/dev/null | grep -q "${BUSY:-esc to interrupt}"
}

if [ "$ready" -eq 1 ]; then
  settle="$(awk -v ms="$DELAY_MS" 'BEGIN{printf "%%.3f", ms/1000}')"
  sleep "$settle"
  # Delivery rides tmux's own paste path, never send-keys with the text as an
  # argument: tmux 3.7b rejects an argument over ~16 KiB ("command too long",
  # rc=1) and the worker-local charter is 20,857 bytes, so the literal-argument
  # form died right here and the seat was handed NOTHING while the launch still
  # looked healthy. load-buffer takes a FILE path, so no
  # argument-size limit applies; paste-buffer -p keeps the prompt's line feeds
  # and -d deletes the buffer as it pastes, so a 20 KB prompt does not linger
  # in the box's tmux state. The explicit delete-buffer covers a paste that
  # died mid-way, which is the only path where -d does not run.
  if ! tmux load-buffer -b gc-prime-nudge "$DIR/nudge.txt"; then
    printf 'nudge_failed\n' > "$DIR/status"
    exit 0
  fi
  BEFORE="$DIR/.pane_pre_paste"
  tmux capture-pane -p -t main >"$BEFORE" 2>/dev/null || : >"$BEFORE"
  if ! tmux paste-buffer -p -d -t main -b gc-prime-nudge; then
    tmux delete-buffer -b gc-prime-nudge 2>/dev/null
    printf 'nudge_failed\n' > "$DIR/status"
    exit 0
  fi
  # gc's own paste-to-submit debounce (tmux.NudgeSession -> nudgeSubmitDebounce).
  sleep %[3]s
  # Fail CLOSED on delivery. A paste that leaves the pane byte-identical put
  # nothing in front of the agent: status is nudge_failed and prompted_at is
  # NOT written, because a prime that records success while the pane still
  # reads "Ask Codex to do anything" is exactly what made a dark lane look
  # healthy. Only a definite no-change counts as failure - a pane
  # that repainted for any reason at all passes - so this can never turn a real
  # delivery into a false negative.
  AFTER="$DIR/.pane_post_paste"
  tmux capture-pane -p -t main >"$AFTER" 2>/dev/null || : >"$AFTER"
  if cmp -s "$BEFORE" "$AFTER"; then
    printf 'nudge_failed\n' > "$DIR/status"
    exit 0
  fi
  confirmed=0
  send=0
  while [ "$send" -lt %[4]d ]; do
    if [ "$send" -gt 0 ]; then
      # Never re-send into a pane that already went busy - that would be a
      # second submit of an already-started turn (submitEnterAndConfirm).
      if pane_busy; then confirmed=1; break; fi
      sleep %[5]s
    fi
    # codex's submit sequence is Escape THEN Enter, never a lone Enter: a
    # send-keys burst lands as a bracketed paste, and codex reads a trailing
    # lone Enter as a composer newline rather than submit.
    tmux send-keys -t main Escape
    tmux send-keys -t main Enter
    poll=0
    while [ "$poll" -lt %[6]d ]; do
      if pane_busy; then confirmed=1; break; fi
      sleep %[7]s
      poll=$((poll + 1))
    done
    [ "$confirmed" -eq 1 ] && break
    send=$((send + 1))
  done
  date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ > "$DIR/prompted_at"
  if [ "$confirmed" -eq 1 ]; then
    printf 'delivered\n' > "$DIR/status"
  else
    printf 'submit_unconfirmed\n' > "$DIR/status"
  fi
else
  # The readiness budget is a fail-closed startup check, not a silent wait.
  # Keep the last bounded pane view beside the status so the sidecar can tell
  # an interactive trust dialog from a slow or dead agent after this window
  # exits. A tmux capture failure still leaves an empty evidence file and the
  # not_ready status below, never a false healthy result.
  tmux capture-pane -p -t main -S -120 2>/dev/null > "$DIR/pane_tail" || :
  printf 'not_ready\n' > "$DIR/status"
fi
`

// seconds formats a millisecond count as the fractional-seconds argument
// POSIX sleep accepts (coreutils sleep, present on the task image; not
// busybox ash, which this pack does not target).
func seconds(ms int) string {
	return fmt.Sprintf("%g", float64(ms)/1000)
}

// primeScript is primeScriptTemplate with its tuning filled in — a var
// rather than a const because it is built with fmt.Sprintf, but fixed at
// init and never mutated afterward.
var primeScript = fmt.Sprintf(primeScriptTemplate,
	primeReadyBudget, primeInterval,
	seconds(primeSubmitDebounceMs), primeSubmitMaxSends, seconds(primeSubmitBackoffMs),
	primeConfirmPollsPerSend, seconds(primeConfirmPollMs), primeDefaultReadyDelayMs)

// buildPrimeFiles returns the tar payload staged into
// $NOMAD_SECRETS_DIR/gc-prime: the script, the resolved prompt text (never
// cfg.Env/Files, same rule promptSubset already applies), and the readiness
// prefix the script polls for.
// readyDelayMs computes the settle to write to ready_delay_ms.txt: the wire
// value, when positive, else primeDefaultReadyDelayMs. run.sh reads the file
// but falls back to the same default if the file is absent or unparsable, so
// this is belt-and-suspenders rather than the only place the default lives.
func readyDelayMs(wireValue int) int {
	if wireValue > 0 {
		return wireValue
	}
	return primeDefaultReadyDelayMs
}

// primeFilesFor is the one place a stageConfig becomes prime files. It exists
// so the WIRING is testable: the accessors below can be perfectly correct and
// simply not called, and a test of the accessors alone passes either way —
// verified by reverting this call and watching that test stay green.
func primeFilesFor(text string, cfg stageConfig) []stageFile {
	return buildPrimeFiles(text, cfg.readyPromptPrefix(), cfg.loadingMarker(), cfg.busyMarker(), cfg.readyDelayMs())
}

func buildPrimeFiles(text, readyPromptPrefix, loadingMarker, busyMarker string, readyDelayMsValue int) []stageFile {
	return []stageFile{
		{Path: primeScriptFile, Content: []byte(primeScript), Mode: 0o755},
		{Path: primeNudgeFile, Content: []byte(text), Mode: 0o600},
		{Path: primePrefixFile, Content: []byte(readyPromptPrefix), Mode: 0o600},
		{Path: primeLoadingFile, Content: []byte(loadingMarker), Mode: 0o600},
		{Path: primeBusyFile, Content: []byte(busyMarker), Mode: 0o600},
		{Path: primeDelayFile, Content: []byte(fmt.Sprintf("%d", readyDelayMs(readyDelayMsValue))), Mode: 0o600},
	}
}

// startupPrimeDetached replaces the blocking startupPrime call at both
// launch call sites. It returns as soon as the prime window is staged and
// spawned (or was already spawned, or nothing is owed) — never once it
// waits for the agent's ready prompt, which is now the detached window's
// job on its own clock.
func (l *lifecycle) startupPrimeDetached(ctx context.Context, sessionName string, cfg stageConfig) error {
	text, _ := cfg.startupPrompt()
	if text == "" {
		return nil
	}
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("session %q binding vanished before the startup prompt could be primed", sessionName)
	}
	if b.PromptedAt != nil {
		return nil // already delivered - never re-stage or re-spawn
	}
	exists, err := l.primeWindowExists(ctx, sessionName)
	if err != nil {
		// Non-fatal and deliberately not retried here: the launch op must
		// still return promptly. A retried start (or opIsRunning's own
		// marker sync finding nothing yet) gets the next chance.
		l.logDecision("startup-prime session=%s window-check error: %v", sessionName, err)
		return nil
	}
	if exists {
		return nil // already running, or finished - syncPromptedMarker will catch that
	}

	files := primeFilesFor(text, cfg)
	data, err := buildTar(files)
	if err != nil {
		return fmt.Errorf("building prime script for %q: %w", sessionName, err)
	}
	stageCmd := []string{
		"/bin/sh", "-c",
		fmt.Sprintf(`mkdir -p "$NOMAD_SECRETS_DIR/%s" && tar -x -f - -C "$NOMAD_SECRETS_DIR/%s"`, primeDirName, primeDirName),
		"stage-prime",
	}
	if err := l.execStage(ctx, sessionName, stageCmd, data); err != nil {
		return fmt.Errorf("staging prime script for %q: %w", sessionName, err)
	}

	allocID, err := l.currentAlloc(ctx, sessionName)
	if err != nil {
		return err
	}
	spawnCmd := []string{
		"/bin/sh", "-c",
		fmt.Sprintf(`tmux new-window -d -t %s -n %s '/bin/sh "$NOMAD_SECRETS_DIR/%s/%s"'`,
			tmuxSessionName, primeWindowName, primeDirName, primeScriptFile),
	}
	// execAllocObserved, not execAlloc: tmux writes the reason it could not
	// open a window to STDERR, and execAlloc returns only stdout. This spawn
	// was the last call site still discarding it, which made it the one
	// failure an operator actually meets that reads
	//     spawning prime window: tmux new-window exited 1:
	// with nothing after the colon. The launch path and the exec op already
	// report both streams; this now matches them.
	exitCode, out, boxErr, err := l.client.execAllocObserved(ctx, allocID, execTaskName, spawnCmd, nil)
	if err != nil {
		return fmt.Errorf("spawning prime window for %q: %w", sessionName, err)
	}
	if exitCode != 0 {
		// The contradiction is worth stating outright: confirmLaunched has
		// already verified this tmux session exists, so a spawn that cannot
		// reach it is not "the box is not ready yet" — it means the session
		// went away, or this exec is looking at a different tmux socket than
		// the launch used.
		// The old message named the two shapes and left the operator to
		// pick. When tmux itself says the server is gone, the answer is
		// knowable from inside the box, so ask instead of offering a choice.
		cause := ""
		if paneGone(string(boxErr)) {
			cause = l.diagnosePaneGone(ctx, allocID)
		}
		if cause == "" {
			cause = " (the launch of this session was already confirmed, so tmux was reachable moments " +
				"ago — a socket path difference between the launch env and this exec, or a server that " +
				"exited, are the two shapes that produce this)"
		}
		return fmt.Errorf("spawning prime window for %q: tmux new-window exited %d: stdout=%s stderr=%s%s",
			sessionName, exitCode, execExcerpt(out), execExcerpt(boxErr), cause)
	}
	l.logDecision("startup-prime session=%s window=spawned", sessionName)
	return nil
}

// primeWindowExists reports whether sessionName's alloc already carries the
// detached priming window — spawn-once guard against a retried start
// re-spawning a second window against a session that is already being
// primed (or was, and is only waiting on syncPromptedMarker to notice).
func (l *lifecycle) primeWindowExists(ctx context.Context, sessionName string) (bool, error) {
	allocID, err := l.currentAlloc(ctx, sessionName)
	if err != nil {
		return false, err
	}
	check := fmt.Sprintf(`tmux list-windows -t %s -F '#{window_name}' 2>/dev/null || true`, tmuxSessionName)
	exitCode, out, err := l.client.execAlloc(ctx, allocID, execTaskName, []string{"/bin/sh", "-c", check})
	if err != nil {
		return false, err
	}
	if exitCode != 0 {
		return false, nil
	}
	for _, name := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(name) == primeWindowName {
			return true, nil
		}
	}
	return false, nil
}

// syncPromptedMarker is the other half of the detached hand-off: it reads
// the marker files the prime window drops and copies the result onto the
// sidecar binding, so prompted_at/prompt_status still land there exactly as
// they did when startupPrime wrote them synchronously. Called from
// opIsRunning (gc already polls that op regularly, so this rides an
// existing call rather than adding a poll loop of its own) and is a no-op —
// no exec call at all — once a session is either primed or was never owed a
// prompt (PromptPending nil), so it costs nothing on the common path.
func (l *lifecycle) syncPromptedMarker(ctx context.Context, sessionName string) {
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil || !ok || b.PromptedAt != nil || b.PromptPending == nil {
		return
	}
	allocID, err := l.currentAlloc(ctx, sessionName)
	if err != nil {
		return
	}
	cmd := fmt.Sprintf(`cat "$NOMAD_SECRETS_DIR/%s/%s" 2>/dev/null; printf '\036'; cat "$NOMAD_SECRETS_DIR/%s/%s" 2>/dev/null`,
		primeDirName, primeMarkerFile, primeDirName, primeStatusFile)
	exitCode, out, err := l.client.execAlloc(ctx, allocID, execTaskName, []string{"/bin/sh", "-c", cmd})
	if err != nil || exitCode != 0 {
		return
	}
	parts := strings.SplitN(string(out), "\x1e", 2)
	marker := strings.TrimSpace(parts[0])
	status := ""
	if len(parts) > 1 {
		status = strings.TrimSpace(parts[1])
	}
	tail := ""
	tailCmd := fmt.Sprintf("cat \"$NOMAD_SECRETS_DIR/%s/%s\" 2>/dev/null", primeDirName, primePaneTailFile)
	tailCode, tailOut, tailErr := l.client.execAlloc(ctx, allocID, execTaskName, []string{"/bin/sh", "-c", tailCmd})
	if tailErr == nil && tailCode == 0 {
		tail = strings.TrimSpace(string(tailOut))
	}
	if marker == "" && status == "" {
		return // still waiting - nothing landed yet
	}
	if marker != "" {
		delivered, parseErr := time.Parse(time.RFC3339, marker)
		if parseErr != nil {
			delivered = time.Now().UTC()
		}
		b.PromptedAt = &delivered
	}
	if status != "" {
		// The status file's own word wins (delivered / submit_unconfirmed /
		// nudge_failed / not_ready, from run.sh) rather than being collapsed
		// to "delivered" whenever a marker is present: run.sh writes
		// prompted_at even on an unconfirmed submit (matching
		// gc's own "handed to tmux" best-effort contract), so the marker
		// alone cannot tell confirmed from best-effort.
		b.PromptStatus = status
	} else if marker != "" {
		b.PromptStatus = promptStatusDelivered
	}
	// Replace rather than append: a later successful prime must clear stale
	// timeout evidence left by an earlier bounded attempt.
	b.PromptPaneTail = tail
	if err := l.sidecar.save(*b); err != nil {
		l.logDecision("startup-prime session=%s marker sync could not save: %v", sessionName, err)
	}
}

// paneGoneMarkers are what tmux says when the server it is asked to reach is
// not there. Both spellings occur: the socket file is absent, or the socket is
// present and nothing is listening.
var paneGoneMarkers = []string{"no server running", "error connecting to"}

// paneGone reports whether tmux's own stderr says the server is not there.
func paneGone(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, m := range paneGoneMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// diagnosePaneGone answers the question the prime-window failure raises and
// could not previously answer: the launch of this session was CONFIRMED, so
// tmux was reachable moments ago — where did the server go?
//
// A tmux server exits when its last session ends, and that session ends when
// its pane's command exits. So "no server running" a moment after a confirmed
// launch is not a tmux fault, it is the AGENT COMMAND having died immediately,
// and the layer that knows is the box's own agent-capture record. The pack
// already ships alloc/logs/agent.exit on egress — an "exit= started= ended=
// pane= argv0=" line carrying the command name and timestamps but never any env
// value, which is what makes it safe to quote — and until now never read it for
// diagnosis. The operator got a socket path and no cause.
//
// An absent record is itself an answer and is reported as one: in the prebaked
// image shape the capture block is the IMAGE's job, and an image that does not
// write it leaves this failure permanently unexplainable. Saying so is better
// than silence, because it names what to fix in the image rather than sending
// someone back to tmux.
func (l *lifecycle) diagnosePaneGone(ctx context.Context, allocID string) string {
	if allocID == "" {
		return ""
	}
	if rec, ok := l.readBoxFile(ctx, allocID, "alloc/data/agent.exit"); ok && looksLikeAgentExitRecord(rec) {
		out := " THE PANE'S COMMAND EXITED — a tmux server exits with its last session, and that " +
			"session ends when its pane's command does, so the server being gone IS the agent having " +
			"died. The box's own exit record says: " + execExcerpt([]byte(lastLine(rec)))
		if errRec, ok := l.readBoxFile(ctx, allocID, "alloc/data/agent.stderr"); ok {
			if tail := strings.TrimSpace(lastLine(errRec)); tail != "" {
				out += " Its stderr ended: " + execExcerpt([]byte(tail))
			}
		}
		// The one cause an exit code cannot express and the box alone can
		// answer. Appended AFTER the record because it explains it.
		if report, ok := l.readBoxFile(ctx, allocID, "alloc/data/"+hostPathReportName); ok {
			out += l.hostPathFinding(report)
		}
		return out
	}
	return " The pane's command has exited (a tmux server exits with its last session), but this box " +
		"wrote NO usable agent-capture record at alloc/data/agent.exit, so why it exited cannot be " +
		"recovered from here. In the prebaked-image shape that record is the IMAGE's responsibility: " +
		"an image that does not write it makes this exact failure permanently unexplainable. Fix that " +
		"in the image rather than looking further at tmux."
}

// readBoxFile reads a file out of the allocation and reports whether what came
// back is actually FILE CONTENT.
//
// Nomad's fs/cat can answer 200 with an ERROR BODY — "stat <path>: no such file
// or directory" — rather than a 404, and readAllocFile correctly passes that
// through as a successful read of those bytes. Quoting it is worse than saying
// nothing: the first version of this diagnosis reported
//
//	The box's own exit record says: "stat /opt/nomad/data/.../agent.exit: no
//	such file or directory"
//
// which is a confident answer built out of the instrument's own failure. That
// is the exact shape this pack exists to stop producing, produced by the code
// written to stop producing it — found by reading its own live output.
func (l *lifecycle) readBoxFile(ctx context.Context, allocID, path string) (string, bool) {
	raw, err := l.client.readAllocFile(ctx, allocID, path)
	if err != nil {
		return "", false
	}
	body := strings.TrimSpace(string(raw))
	if body == "" || isFSErrorBody(body) {
		return "", false
	}
	return body, true
}

// isFSErrorBody recognises the fs API answering with a complaint instead of a
// file. Matched on the operation words rather than the path, because the path
// is whatever was asked for and only the verb identifies the shape.
func isFSErrorBody(body string) bool {
	low := strings.ToLower(body)
	if strings.Contains(low, "no such file or directory") ||
		strings.Contains(low, "permission denied") ||
		strings.Contains(low, "unknown allocation") {
		return true
	}
	// A one-line body that starts with a syscall name is the fs API's error
	// shape, never an agent record.
	return strings.HasPrefix(low, "stat ") || strings.HasPrefix(low, "open ")
}

// looksLikeAgentExitRecord requires the record to BE one. The launcher writes
// "exit= started= ended= pane= argv0= alloc=", so the exit field is the cheapest
// positive signal that this is the record and not something else that happened
// to be readable at that path.
func looksLikeAgentExitRecord(body string) bool {
	return strings.Contains(body, "exit=")
}

// lastLine returns the final non-empty line, which is where an exit record and
// a dying command's last words both live.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// hostPathFinding turns the box's missing-path report into the sentence that
// sends the reader to the right file. The remedy is never in the box: the path
// came from the provider's own arguments, which are rendered on the
// orchestrator, where it does exist. That asymmetry is the whole defect, and
// it cost this estate days of a lane whose agent exited 1 in the same second
// it started, with the real cause being an orchestrator-only path in a `-c` flag.
func (l *lifecycle) hostPathFinding(report string) string {
	var paths []string
	for _, line := range strings.Split(report, "\n") {
		line = strings.TrimSpace(line)
		if p := strings.TrimPrefix(line, "missing-path:"); p != line {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
	}
	if len(paths) == 0 {
		return ""
	}
	return " AND THE BOX WAS GIVEN PATHS THAT DO NOT EXIST IN IT: " + strings.Join(paths, ", ") +
		" — these are absolute paths in the AGENT'S OWN ARGUMENTS, which are rendered on the " +
		"orchestrator, where they do exist. Nothing in the image can fix this; the provider " +
		"configuration has to name a path that is valid inside the allocation (stage the file, bake " +
		"it into the image, or drop the argument in the runtime's shape)."
}
