package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// This file is the pack-native fix for a one_shot seat that has already
// told the controller it is done — the claim-free remote ack,
// `gc hook --drain-ack` (equivalently `gc runtime drain-ack --json` while it
// still holds a claim) — sits at its interactive prompt forever afterward.
// The box, the alloc and the tmux pane all stay healthy, so nothing outside
// this pack ever notices; the pool then serves exactly max_active_sessions
// atoms and goes dark, holding every slot on a session that finished.
//
// THE MECHANISM AND WHY IT LIVES HERE, NOT IN THE IMAGE OR THE CHARTER.
// GC_NOMAD_AGENT_LAUNCH_SCRIPT — the one deployment-owned seam that used to
// sit in front of the pane's command — was removed outright in 1.0.7
// (image_env.go, codex_home.go, RELEASE-NOTES.md): "the image's tmux pane
// runs gc's rendered launch command and nothing else". This pack's own
// agentCaptureBody (ops.go) is therefore the ENTIRE in-box launcher today —
// it already taps the pane's output via `tmux pipe-pane` into
// alloc/data/agent.stdout, so that capture is the one channel this pack can
// read without owning the image, the charter, or gc's own source. Nothing
// upstream needs to change for this to work: the ack's own `--json` success
// text is already what lands in that file, because it is what the agent's
// shell prints when it runs the command.
//
// lifecycleOneShot is gc's own stageConfig.Lifecycle string for a one_shot
// pool template, carried into the binding at launch confirmation
// (confirmLaunched, ops.go) purely so this file can gate on it — the
// mechanism below must never fire for a pool session that is SUPPOSED to
// stay interactive between atoms.
const lifecycleOneShot = "one_shot"

// saveLifecyclePending persists cfg.Lifecycle onto sessionName's binding at
// stage time — BEFORE any launch — independent of whether a startup prompt
// is owed. It exists because confirmLaunched's own lifecycleValue
// parameter (ops.go) is not enough on its own: gc's canonical way to run a
// pool's SECOND and later atom in the SAME box is opExec's separable
// transport-launch handshake (markTransportLaunched), which runs with NO
// stageConfig of its own — gc's transport argv carries only the tmux
// handshake — and rebuilds one only from binding.PromptPending, which
// savePromptPending populates from cfg.promptSubset(). That subset is
// deliberately narrow (Command/Nudge/ProcessNames/ReadyPromptPrefix/
// ReadyDelayMs — the prompt-delivery fields the transport path exists to
// re-supply) and does not carry Lifecycle, so a session relaunched through
// that path alone never had a Lifecycle to hand confirmLaunched — measured
// live on a one_shot worker-local pool: `gc hook --drain-ack` succeeded,
// but the binding never carried a "lifecycle" key at all, so
// reapDrainedOneShot's own gate on it could never fire.
//
// gc DOES send the value — TestStageConfigCarriesEveryGcLaunchWireField
// pins a captured `"lifecycle": "one_shot"` wire body — so the fix is
// carrying gc's own field forward, not inventing one: every call that
// stages a session (stageTracked, hit by both provision and start, ahead
// of every launch shape) records it here, once, so it is already on the
// binding by the time ANY later launch path — direct or transport — runs.
// confirmLaunched's own "only overwrite on a non-empty value" rule then
// preserves it even when a later transport-launch call has nothing to
// send.
func (l *lifecycle) saveLifecyclePending(sessionName string, cfg stageConfig) error {
	lifecycleValue := strings.TrimSpace(cfg.Lifecycle)
	if lifecycleValue == "" {
		return nil
	}
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("session %q binding vanished before its lifecycle could be recorded", sessionName)
	}
	if b.Lifecycle == lifecycleValue {
		return nil
	}
	b.Lifecycle = lifecycleValue
	return l.sidecar.save(*b)
}

// drainAckMarkerEnv lets a deployment override the regex opIsRunning greps
// the captured pane transcript for. The default matches the claim-free
// remote ack's own `--json` success shape (a `"drain_ack":true` field —
// internal/api/handler_worker.go's DrainAck response, POST
// /v0/city/{city}/worker/drain-ack), tolerant of the whitespace `encoding/
// json` may or may not insert. This is env-configurable rather than a
// second gc-side contract this pack would have to track: the exact text is
// the SERVER's, not this pack's, and a rewording upstream should not need a
// pack rebuild to keep working — set this once on the deployment and every
// session picks it up on its next is-running poll.
const drainAckMarkerEnv = "GC_NOMAD_DRAIN_ACK_MARKER"

// defaultDrainAckMarker matches `"drain_ack": true` (any amount of
// whitespace around the colon, either JSON boolean casing gc happens to
// emit) wherever it appears in the captured transcript — case-insensitive,
// because the key convention (`drain_ack` vs `DrainAck`) is the server's
// call, not this pack's, and a marker keyed to one casing that gc changes is
// a marker that silently stops firing.
var defaultDrainAckMarker = regexp.MustCompile(`(?i)"drain_ack"\s*:\s*true`)

// drainAckMarker resolves the active marker regex: the deployment override
// when GC_NOMAD_DRAIN_ACK_MARKER is set and compiles, else the default. An
// invalid override is not a fatal error — a typo in an env var must not
// crash every is-running poll on this lane — but it must not fall through to
// the default silently either, so the caller logs it once (see
// reapDrainedOneShot).
func drainAckMarker() (*regexp.Regexp, error) {
	raw := strings.TrimSpace(os.Getenv(drainAckMarkerEnv))
	if raw == "" {
		return defaultDrainAckMarker, nil
	}
	re, err := regexp.Compile(raw)
	if err != nil {
		return nil, fmt.Errorf("%s=%q does not compile: %w", drainAckMarkerEnv, raw, err)
	}
	return re, nil
}

// paneShowsDrainAck execs a single grep of the alloc's own captured pane
// transcript for the active drain-ack marker. It never reads pane state
// live (no `tmux capture-pane`): agent.stdout is the append-only, already-
// tapped copy of everything the pane ever printed, which is both simpler
// (one exec, not two) and safe against a transient empty capture racing a
// live read. A one_shot session's transcript holds exactly one turn, so
// "the marker appears anywhere in it" is exactly "the ack already
// succeeded" — no busy/idle disambiguation is needed on top, because
// nothing legitimate runs in a one_shot seat after its own terminal
// drain-ack step.
func (l *lifecycle) paneShowsDrainAck(ctx context.Context, allocID string, marker *regexp.Regexp) (bool, error) {
	pattern, caseInsensitive := drainAckGrepPattern(marker)
	flags := "-Eq"
	if caseInsensitive {
		flags = "-Eqi"
	}
	check := fmt.Sprintf(
		`grep %s %s "${NOMAD_ALLOC_DIR:-/tmp}/data/%s" 2>/dev/null`,
		flags, shellQuote(pattern), agentStdoutCaptureName,
	)
	exitCode, _, err := l.client.execAlloc(ctx, allocID, execTaskName, []string{"/bin/sh", "-c", check})
	if err != nil {
		return false, err
	}
	return exitCode == 0, nil
}

// drainAckGrepPattern turns a Go regexp into the POSIX extended-regex form
// `grep -E` actually understands. Go's Regexp.String() round-trips an
// `(?i)` case-insensitivity flag as literal `(?i)` text, which `grep -E`
// does not parse as a flag at all — it is inert leading text in an ERE, so
// embedding it verbatim silently changed the match to "starts with the
// four characters (?i)" and never matched anything, always exiting
// nonzero. Stripping the prefix and translating it into grep's own `-i`
// flag keeps the two matchers equivalent instead of merely similar-looking.
func drainAckGrepPattern(marker *regexp.Regexp) (pattern string, caseInsensitive bool) {
	s := marker.String()
	if rest, ok := strings.CutPrefix(s, "(?i)"); ok {
		return rest, true
	}
	return s, false
}

// reapDrainedOneShot is opIsRunning's one side effect beyond its own
// bookkeeping (it already runs syncPromptedMarker the same way): for a
// one_shot session whose captured transcript already shows a successful
// claim-free drain ack, it retires the session through the pack's ordinary
// opStop path — the same egress, log-shipper flush and deregister every
// other stop uses — rather than inventing a second shutdown route. It
// reports true only when it actually stopped the session, so the caller
// can answer is-running "false" for the same poll that discovered it,
// instead of reporting alive=true once more and catching it only next
// tick.
//
// WHAT THIS DELIBERATELY DOES NOT DO: it is not a timer, and it never
// fires on idleness alone — the owner's own standing correction on this
// bead is "do not add a reaper that kills idle boxes on a timer (a timer
// cannot tell a finished seat from a thinking one)". This reaps on the
// ack's own success text, observed in the session's own transcript, and
// on nothing else: a pool session (lifecycle != one_shot) or a one_shot
// session that has not yet printed the marker is untouched.
func (l *lifecycle) reapDrainedOneShot(ctx context.Context, sessionName string, b *binding, allocID string) bool {
	if b == nil || b.Lifecycle != lifecycleOneShot || b.ExitReason != "" {
		return false
	}
	marker, err := drainAckMarker()
	if err != nil {
		l.logDecision("session %q: drain-ack marker disabled this poll: %v", sessionName, err)
		return false
	}
	drained, err := l.paneShowsDrainAck(ctx, allocID, marker)
	if err != nil || !drained {
		return false
	}
	b.ExitReason = exitReasonDrainAck
	if err := l.sidecar.save(*b); err != nil {
		// Non-fatal: the stop below is what actually matters, and it still
		// removes the binding either way. Losing the receipt is worth
		// logging, not worth aborting the retirement over.
		l.logDecision("session %q: could not record exit_reason=%s before stopping: %v", sessionName, exitReasonDrainAck, err)
	}
	l.logDecision("session %q: one_shot agent's drain-ack is already acknowledged and it is still parked at its prompt — retiring the incarnation (exit_reason=%s)", sessionName, exitReasonDrainAck)
	if err := l.opStop(ctx, sessionName); err != nil {
		l.logDecision("session %q: retiring after drain-ack failed, leaving the slot held: %v", sessionName, err)
		return false
	}
	return true
}
