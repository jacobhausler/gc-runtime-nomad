package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Detached in-box startup priming (cr-gdeav.6.13).
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
// already survives the exec call that started it (buildLaunchCommand's own
// doc comment). No cross-network polling and no long-lived pack process are
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
	primeDirName     = "gc-prime"
	primeWindowName  = "gc-prime"
	primeScriptFile  = "run.sh"
	primeNudgeFile   = "nudge.txt"
	primePrefixFile  = "ready_prefix.txt"
	primeMarkerFile  = "prompted_at"
	primeStatusFile  = "status"
	primeReadyBudget = 180 // seconds; mirrors startupReadyBudgetDefault
	primeInterval    = 2   // seconds; mirrors startupReadyIntervalDefault
)

// primeScript is the POSIX shell the detached tmux window runs. It is a
// shell-side reimplementation of awaitReady/paneShowsReadyPrompt's core rule
// — trim the pane line, prefix-match against the configured value — because
// nothing in this pack's own process survives to run that Go code once the
// launch op has returned. It deliberately skips the NBSP-fold and box-border
// strip startup.go's Go matcher carries (paneShowsReadyPrompt): those cover
// TUI rendering edge cases the ready row observed in production does not
// need, and keeping the script this small keeps it auditable by inspection
// in the alloc, which matters more here than parity down to the last corner
// case.
const primeScriptTemplate = `#!/bin/sh
set -u
DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
PREFIX=""
if [ -f "$DIR/ready_prefix.txt" ]; then PREFIX="$(cat "$DIR/ready_prefix.txt")"; fi
elapsed=0
ready=0
while [ "$elapsed" -le %[1]d ]; do
  if [ -z "$PREFIX" ]; then
    ready=1
  else
    pane="$(tmux capture-pane -p -t main 2>/dev/null || true)"
    if printf '%%s\n' "$pane" | awk -v p="$PREFIX" '{
      line=$0
      sub(/^[ \t]+/, "", line)
      sub(/[ \t]+$/, "", line)
      if (length(p) <= length(line) && substr(line, 1, length(p)) == p) found=1
    } END { exit !found }'; then
      ready=1
    fi
  fi
  [ "$ready" -eq 1 ] && break
  sleep %[2]d
  elapsed=$((elapsed + %[2]d))
done
if [ "$ready" -eq 1 ]; then
  if tmux send-keys -t main -l -- "$(cat "$DIR/nudge.txt")" && tmux send-keys -t main Enter; then
    date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ > "$DIR/prompted_at"
    printf 'delivered\n' > "$DIR/status"
  else
    printf 'nudge_failed\n' > "$DIR/status"
  fi
else
  printf 'not_ready\n' > "$DIR/status"
fi
`

// primeScript is primeScriptTemplate with its budget/interval filled in — a
// var rather than a const because it is built with fmt.Sprintf, but fixed at
// init and never mutated afterward.
var primeScript = fmt.Sprintf(primeScriptTemplate, primeReadyBudget, primeInterval)

// buildPrimeFiles returns the tar payload staged into
// $NOMAD_SECRETS_DIR/gc-prime: the script, the resolved prompt text (never
// cfg.Env/Files, same rule promptSubset already applies), and the readiness
// prefix the script polls for.
func buildPrimeFiles(text, readyPromptPrefix string) []stageFile {
	return []stageFile{
		{Path: primeScriptFile, Content: []byte(primeScript), Mode: 0o755},
		{Path: primeNudgeFile, Content: []byte(text), Mode: 0o600},
		{Path: primePrefixFile, Content: []byte(readyPromptPrefix), Mode: 0o600},
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

	files := buildPrimeFiles(text, cfg.ReadyPromptPrefix)
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
	exitCode, out, err := l.client.execAlloc(ctx, allocID, execTaskName, spawnCmd)
	if err != nil {
		return fmt.Errorf("spawning prime window for %q: %w", sessionName, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("spawning prime window for %q: tmux new-window exited %d: %s", sessionName, exitCode, out)
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
	if marker == "" && status == "" {
		return // still waiting - nothing landed yet
	}
	if marker != "" {
		delivered, parseErr := time.Parse(time.RFC3339, marker)
		if parseErr != nil {
			delivered = time.Now().UTC()
		}
		b.PromptedAt = &delivered
		b.PromptStatus = promptStatusDelivered
	} else if status != "" {
		b.PromptStatus = status
	}
	if err := l.sidecar.save(*b); err != nil {
		l.logDecision("startup-prime session=%s marker sync could not save: %v", sessionName, err)
	}
}
