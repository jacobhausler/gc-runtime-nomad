package main

import (
	"os"
	"strings"
	"testing"
)

// capturedStartupPane is the real thing: the Codex TUI repaint stream an
// ordinary nomad-canary launch wrote to agent.stdout in alloc fada3496
// (cr-gdeav.6.11 attempt 4, pack c989c48, provider prompt_mode="none",
// ready_prompt_prefix="Ask Codex"). It is the only artifact that shows why the
// startup nudge never fired: the ready row reads
//
//	› Ask Codex to do anything
//
// i.e. it LEADS with the U+203A prompt glyph, so a prefix configured without
// the glyph can never satisfy gc's matchesPromptPrefix rule (internal/runtime/
// tmux/tmux.go:3297-3315 - HasPrefix on the trimmed line), no matter how long
// the readiness wait is. Fetched from /alloc/logs/agent.stdout via
// `nomad alloc fs` before the allocation was GC'd.
func capturedStartupPane(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/codex-startup-pane.raw")
	if err != nil {
		t.Fatalf("read captured pane fixture: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("captured pane fixture is empty")
	}
	return string(raw)
}

// TestPaneShowsReadyPromptAgainstCapturedCodexPane pins the readiness question
// against captured bytes rather than a hand-written pane. Two separate defects
// have to be dead for the startup nudge to fire, and both are visible here:
// the configured prefix must carry the prompt glyph (gc's rule is a line
// PREFIX), and the captured stream must be reconstructed into rows before any
// line rule can be applied to it at all.
func TestPaneShowsReadyPromptAgainstCapturedCodexPane(t *testing.T) {
	captured := capturedStartupPane(t)
	if strings.Count(captured, "\n") > 12 {
		t.Fatalf("fixture expected a raw repaint stream, got %d newline-separated chunks",
			strings.Count(captured, "\n")+1)
	}
	cases := []struct {
		name   string
		pane   string
		prefix string
		want   bool
	}{
		{"raw stream, glyph-bearing prefix (the value that matches)", captured, "› Ask Codex", true},
		{"raw stream, glyph-less prefix (attempt 4's value)", captured, "Ask Codex", false},
		{"booting pane never matches", "model:     loading   /model to change", "› Ask Codex", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneShowsReadyPrompt(tc.pane, tc.prefix); got != tc.want {
				t.Fatalf("paneShowsReadyPrompt(prefix %q) = %t, want %t", tc.prefix, got, tc.want)
			}
		})
	}
}
