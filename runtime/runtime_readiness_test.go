package main

import "testing"

// TestReadinessFactsCanRideRuntimeEnv is gate 3's blocker, removed without
// adding a single field to gc's launch wire.
//
// A provider can run a DIFFERENT INTERACTION MODE depending on where its
// session runs. Measured (cr-8eagyh): one estate's codex provider runs
// `codex exec` locally — non-interactive, argv-driven, no prompt ever printed
// — while the same provider under this runtime runs the interactive TUI, which
// must be primed by typing and therefore needs a prompt to wait for. The
// provider's own ready_prompt_prefix cannot express both: set, the local exec
// lane waits forever for a prompt that never comes; unset, the box types into
// a TUI that is not ready.
//
// That is what forced a WRAPPER PROVIDER — a second provider block restating
// one provider's facts in box shape — which is precisely what "any lane
// onboards by naming its runtime" exists to remove. The session env is the
// pack's established seam for exactly this (see the loading/busy markers), so
// the readiness pair rides it too.
func TestReadinessFactsCanRideRuntimeEnv(t *testing.T) {
	t.Run("the wire value is used when nothing overrides it", func(t *testing.T) {
		cfg := stageConfig{ReadyPromptPrefix: "> ", ReadyDelayMs: 250}
		if got := cfg.readyPromptPrefix(); got != "> " {
			t.Errorf("prefix = %q, want the wire value", got)
		}
		if got := cfg.readyDelayMs(); got != 250 {
			t.Errorf("delay = %d, want the wire value", got)
		}
	})

	t.Run("the session env wins, so a runtime can differ from the provider", func(t *testing.T) {
		cfg := stageConfig{
			ReadyPromptPrefix: "",
			ReadyDelayMs:      0,
			Env: map[string]string{
				envPrimeReadyPrefix:  "› Ask Codex",
				envPrimeReadyDelayMs: "1500",
			},
		}
		if got := cfg.readyPromptPrefix(); got != "› Ask Codex" {
			t.Errorf("prefix = %q, want the runtime_env override", got)
		}
		if got := cfg.readyDelayMs(); got != 1500 {
			t.Errorf("delay = %d, want the runtime_env override", got)
		}
	})

	t.Run("a malformed delay falls back rather than becoming zero", func(t *testing.T) {
		// Zero is not a safe guess here: it means type the instant the TUI
		// claims readiness, which is the prompt-eaten case this pair exists to
		// prevent. A typo must not silently select it.
		for _, bad := range []string{"soon", "-1", "1.5", ""} {
			cfg := stageConfig{ReadyDelayMs: 900, Env: map[string]string{envPrimeReadyDelayMs: bad}}
			if got := cfg.readyDelayMs(); got != 900 {
				t.Errorf("delay for %q = %d, want the wire fallback 900", bad, got)
			}
		}
	})

	t.Run("an empty override does not blank a real wire value", func(t *testing.T) {
		cfg := stageConfig{ReadyPromptPrefix: "> ", Env: map[string]string{envPrimeReadyPrefix: "   "}}
		if got := cfg.readyPromptPrefix(); got != "> " {
			t.Errorf("prefix = %q, want the wire value to survive a blank override", got)
		}
	})
}

// TestPrimeFilesCarryTheRuntimeOverrides pins the WIRING, not the accessors.
//
// Reverting the call site left the accessor test above green, which is the
// exact silent-failure shape this codebase keeps producing: a correct
// mechanism that nothing calls. This asserts the staged files themselves.
func TestPrimeFilesCarryTheRuntimeOverrides(t *testing.T) {
	cfg := stageConfig{
		ReadyPromptPrefix: "WIRE-PREFIX",
		ReadyDelayMs:      111,
		Env: map[string]string{
			envPrimeReadyPrefix:   "ENV-PREFIX",
			envPrimeReadyDelayMs:  "222",
			envPrimeLoadingMarker: "ENV-LOADING",
			envPrimeBusyMarker:    "ENV-BUSY",
		},
	}
	got := map[string]string{}
	for _, f := range primeFilesFor("nudge", cfg) {
		got[f.Path] = string(f.Content)
	}
	for path, want := range map[string]string{
		primePrefixFile:  "ENV-PREFIX",
		primeDelayFile:   "222",
		primeLoadingFile: "ENV-LOADING",
		primeBusyFile:    "ENV-BUSY",
	} {
		if got[path] != want {
			t.Errorf("%s = %q, want %q — the override never reached the staged files", path, got[path], want)
		}
	}

	// And with no overrides the wire values still arrive, so an existing
	// deployment is unchanged.
	plain := stageConfig{ReadyPromptPrefix: "WIRE-PREFIX", ReadyDelayMs: 111}
	for _, f := range primeFilesFor("nudge", plain) {
		if f.Path == primePrefixFile && string(f.Content) != "WIRE-PREFIX" {
			t.Errorf("without an override the prefix = %q, want the wire value", f.Content)
		}
		if f.Path == primeDelayFile && string(f.Content) != "111" {
			t.Errorf("without an override the delay = %q, want the wire value", f.Content)
		}
	}
}
