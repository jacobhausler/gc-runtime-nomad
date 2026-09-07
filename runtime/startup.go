package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Startup prompt delivery for a separable RPP session (cr-gdeav.6.10).
//
// gc routes a rendered startup prompt three ways (cmd/gc/prompt_delivery.go:35-57):
// argv suffix (`prompt_mode=arg`, the default), argv suffix + flag
// (`prompt_mode=flag`), or — for ACP and `prompt_mode=none` providers —
// prepended to `cfg.Nudge`. The RPP `start` payload it then serializes
// (internal/runtime/exec/json.go:26-67) carries `nudge` but has NO
// prompt_suffix/prompt_flag field at all (those exist only on runtime.Config,
// internal/runtime/runtime.go:660-671), and exec's Start never nudges
// post-launch the way every other separable provider does
// (internal/runtime/ssh/provider.go:171-172 and :264-265,
// internal/runtime/k8s/provider.go:310-311 and :409-410,
// internal/runtime/acp/acp.go:366-367). docs/reference/exec-session-provider.md:194-197
// puts that duty on the adapter ("handle this in `start` … or leave it to the
// separate `nudge` operation"). Nothing here did either, so an ordinary
// nomad-canary launched a live Codex that sat at its prompt forever
// (cr-gdeav.6.9 receipt §1).
//
// The native gc mechanism is therefore already sufficient — `prompt_mode =
// "none"` plus `ready_prompt_prefix` on the provider, both of which ride the
// start wire — and what was missing is the adapter half this file adds: wait
// for the agent to actually be at its prompt, then type the prompt EXACTLY
// ONCE. It adds no new flag, wrapper, or supervisor-side knob.
const (
	// startupReadyIntervalDefault is how often the priming step re-probes the
	// pane while it waits for the agent to come up. Two seconds: a cold Codex
	// TUI boot is tens of seconds, and each probe is one alloc-exec call, so a
	// tighter interval buys nothing and costs exec traffic.
	startupReadyIntervalDefault = 2 * time.Second

	// startupReadyBudgetDefault bounds the whole wait. It has to cover the
	// slowest honest agent boot (toolchain on NFS + auth install + TUI) while
	// staying far below the point where gc's own launch timeout would have
	// given up on `start` — `start` must return a launched session whether or
	// not the prompt made it, so this budget only ever delays the op, it can
	// never wedge it.
	startupReadyBudgetDefault = 180 * time.Second
)

// The prompt-status values recorded on the sidecar binding. They exist because
// gc's `GC_STARTUP_PROMPT_DELIVERED=1` env marker is stamped from
// promptDelivery's `Delivered` bool (cmd/gc/prompt_delivery.go:49), which for
// `prompt_mode=none` means "routed to nudge", not "typed into the pane" — so
// without these the pack would have no way to tell a primed session from a
// session whose nudge never landed.
const (
	promptStatusDelivered   = "delivered"
	promptStatusNotReady    = "not_ready"
	promptStatusNudgeFailed = "nudge_failed"
)

// startupIO bundles the side effects the priming step performs against a live
// session. They are injected rather than called inline for one reason: the
// once-after-readiness contract is the whole point of this file, and proving
// it against a real tmux pane means a real allocation, which cannot show the
// "never before readiness" half at all (a live pane is either ready or it
// isn't). With the probes injected the ordering is observable and the
// sidecar/binding logic underneath stays the real thing (startup_test.go).
type startupIO struct {
	alloc func(ctx context.Context, sessionName string) (string, error)
	// pane reports whether the tmux session AND its agent pid are still
	// there, and what the pane currently shows.
	pane  func(ctx context.Context, allocID string, pid int) (alive bool, captured string, err error)
	nudge func(ctx context.Context, sessionName, text string) error
	// wait blocks for d or until ctx is done, whichever comes first.
	wait     func(ctx context.Context, d time.Duration) error
	now      func() time.Time
	interval time.Duration
	budget   time.Duration
}

// startupIO wires the priming step to this lifecycle's real transport.
func (l *lifecycle) startupIO() startupIO {
	return startupIO{
		alloc:    l.currentAlloc,
		pane:     l.probeStartupPane,
		nudge:    l.opNudge,
		wait:     sleepCtx,
		now:      time.Now,
		interval: startupReadyIntervalDefault,
		budget:   startupReadyBudgetDefault,
	}
}

// sleepCtx is the pack's context-aware sleep (ops.go's other poll loops use a
// bare time.After on an interval that never has to be interrupted).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// probeStartupPane reads, in ONE alloc-exec call, whether the launched pane and
// the agent process inside it are still there and what the pane shows. It is
// probeAgentAlive plus a capture-pane: the has-session/kill -0 pair alone
// cannot tell "codex running, at its prompt" from "codex running, mid-boot or
// mid-turn", and typing into the latter is how a prompt gets eaten.
func (l *lifecycle) probeStartupPane(ctx context.Context, allocID string, pid int) (bool, string, error) {
	check := "tmux has-session -t " + tmuxSessionName
	if pid > 0 {
		check += fmt.Sprintf(" && kill -0 %d", pid)
	}
	check += " && tmux capture-pane -t " + tmuxSessionName + " -p"
	exitCode, out, err := l.client.execAlloc(ctx, allocID, execTaskName, []string{"/bin/sh", "-c", check})
	if err != nil {
		return false, "", err
	}
	return exitCode == 0, string(out), nil
}

// startupPrime delivers sessionName's first prompt into its pane. It is a
// no-op that touches nothing when the wire carries no prompt text, and
// idempotent thereafter: the sidecar's `prompted_at` receipt is written the
// moment the nudge succeeds, so a second `start` (or a relaunch, or a retried
// dispatch) can never type the prompt twice.
//
// Failure to prime is deliberately NOT a `start` failure. The box is launched,
// exec-able, and answers is-running whether or not anything was typed, and gc's
// own providers treat the post-launch nudge as best-effort
// (`_ = p.Nudge(...)` in ssh/k8s/acp). What failure does instead is stay
// visible: the binding records `prompt_status` and the decision line names the
// reason on stderr, which is the only channel gc captures from this binary.
func (l *lifecycle) startupPrime(ctx context.Context, sessionName string, cfg stageConfig) error {
	return startupPrime(ctx, l.sidecar, l.startupIO(), l.logDecision, sessionName, cfg)
}

func startupPrime(ctx context.Context, st *sidecar, in startupIO, logf func(format string, args ...any), sessionName string, cfg stageConfig) error {
	text, source := cfg.startupPrompt()
	if text == "" {
		return nil
	}
	b, ok, err := st.load(sessionName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("session %q binding vanished before the startup prompt could be delivered", sessionName)
	}
	if b.PromptedAt != nil {
		logf("startup-prompt session=%s skipped=already-primed at=%s", sessionName, b.PromptedAt.UTC().Format(time.RFC3339))
		return nil
	}

	ready, err := in.awaitReady(ctx, sessionName, b.AgentPID, cfg)
	if err != nil {
		return err
	}
	if !ready {
		b.PromptStatus = promptStatusNotReady
		if err := st.save(*b); err != nil {
			return err
		}
		// No prompt text in the log line — the pane content and the prompt
		// body stay out of every captured record (agent-launch.sh holds the
		// same rule for agent.exit).
		logf("startup-prompt session=%s result=not-ready within=%s prompt_prefix=%t",
			sessionName, in.effectiveBudget(), cfg.ReadyPromptPrefix != "")
		return nil
	}

	if err := in.nudge(ctx, sessionName, text); err != nil {
		b.PromptStatus = promptStatusNudgeFailed
		if saveErr := st.save(*b); saveErr != nil {
			logf("startup-prompt session=%s result=nudge-failed but the binding could not be marked: %v", sessionName, saveErr)
		} else {
			logf("startup-prompt session=%s result=nudge-failed: %v", sessionName, err)
		}
		return nil
	}

	deliveredAt := in.now().UTC()
	b.PromptedAt = &deliveredAt
	b.PromptStatus = promptStatusDelivered
	if err := st.save(*b); err != nil {
		return err
	}
	logf("startup-prompt session=%s result=delivered source=%s bytes=%d", sessionName, source, len(text))
	return nil
}

// effectiveBudget reports the wait bound in force, for the log line.
func (in startupIO) effectiveBudget() time.Duration {
	if in.budget > 0 {
		return in.budget
	}
	return startupReadyBudgetDefault
}

// awaitReady polls until the pane shows the agent at its prompt, the wait
// budget runs out, or ctx is done. Readiness is three things at once, because
// each one alone has been observed to lie: the tmux session exists (a session
// outlives its agent), the captured agent pid answers kill -0 (a TUI can exit
// and leave the pane shell), and the pane actually shows the configured prompt
// line (a live process is not an agent that will read what you type).
//
// An empty ready_prompt_prefix drops only the third test — the same fallback
// gc's own adapter takes (internal/runtime/tmux/tmux.go:3336-3350 falls back
// to the fixed delay when no prefix is configured), with cfg.ReadyDelayMs
// honored below as that delay.
func (in startupIO) awaitReady(ctx context.Context, sessionName string, pid int, cfg stageConfig) (bool, error) {
	interval, budget := in.interval, in.budget
	if interval <= 0 {
		interval = startupReadyIntervalDefault
	}
	if budget <= 0 {
		budget = startupReadyBudgetDefault
	}
	deadline := in.now().Add(budget)

	if cfg.ReadyDelayMs > 0 {
		delay := time.Duration(cfg.ReadyDelayMs) * time.Millisecond
		if delay > budget {
			delay = budget
		}
		if err := in.wait(ctx, delay); err != nil {
			return false, err
		}
	}

	for {
		allocID, err := in.alloc(ctx, sessionName)
		if err == nil {
			alive, captured, probeErr := in.pane(ctx, allocID, pid)
			switch {
			case probeErr != nil:
				// A transient exec fault is "not ready yet", not an error: the
				// alloc's exec endpoint flaps right after dispatch
				// (retryAllocExecReady exists for the same window).
			case alive && paneShowsReadyPrompt(captured, cfg.ReadyPromptPrefix):
				return true, nil
			}
		}
		if !in.now().Before(deadline) {
			return false, nil
		}
		if err := in.wait(ctx, interval); err != nil {
			return false, err
		}
	}
}

// paneShowsReadyPrompt reports whether a captured pane shows the agent's ready
// prompt. It is a pack-side reimplementation of gc's matchesPromptPrefix
// (internal/runtime/tmux/tmux.go:3297-3317), pinned to the same three
// behaviours rather than a copy: non-breaking spaces normalized to ordinary
// ones, the line trimmed, and a box-border-stripped variant for TUIs that
// render the input line inside a bordered box. The pack cannot import gc
// (zero-gascity-dependency contract), same as envArgvSafe's treatment above.
func paneShowsReadyPrompt(captured, readyPromptPrefix string) bool {
	if strings.TrimSpace(readyPromptPrefix) == "" {
		return true
	}
	prefix := normalizePromptSpace(readyPromptPrefix)
	bare := strings.TrimSpace(prefix)
	for _, line := range strings.Split(captured, "\n") {
		trimmed := normalizePromptSpace(line)
		for _, candidate := range []string{trimmed, stripPromptBoxBorder(trimmed)} {
			if strings.HasPrefix(candidate, prefix) || (bare != "" && candidate == bare) {
				return true
			}
		}
	}
	return false
}

// normalizePromptSpace trims a pane line and folds U+00A0 to an ordinary
// space, so a prompt configured with regular spaces still matches a TUI that
// pads with non-breaking ones.
func normalizePromptSpace(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\u00a0", " "))
}

// stripPromptBoxBorder drops a leading vertical box-drawing character so a
// prompt glyph rendered inside a bordered input box still matches.
func stripPromptBoxBorder(s string) string {
	s = strings.TrimLeft(s, " \t")
	runes := []rune(s)
	if len(runes) > 0 && (runes[0] == '\u2502' || runes[0] == '\u2503') {
		return strings.TrimLeft(string(runes[1:]), " \t")
	}
	return s
}
