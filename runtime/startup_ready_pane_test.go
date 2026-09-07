package main

import (
	"context"
	"fmt"
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
	rendered := strings.Join(paneReadyLines(captured), "\n")
	cases := []struct {
		name   string
		pane   string
		prefix string
		want   bool
	}{
		{"raw stream, glyph-bearing prefix (the value that matches)", captured, "› Ask Codex", true},
		{"raw stream, glyph-less prefix (attempt 4's value)", captured, "Ask Codex", false},
		{"rendered pane, glyph-bearing prefix", rendered, "\u203a Ask Codex", true},
		{"rendered pane, glyph-less prefix", rendered, "Ask Codex", false},
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

// TestPaneReadyLinesReconstructsRowsFromRawStream is the byte half of the fix.
// A TUI lays its screen out with cursor-positioning sequences, not newlines, so
// the captured stream holds a whole screen in 9 chunks. The rows only become
// matchable once the control layer is stripped and a column-1 CUP is honored as
// the row break the terminal itself would have made.
func TestPaneReadyLinesReconstructsRowsFromRawStream(t *testing.T) {
	captured := capturedStartupPane(t)
	rows := paneReadyLines(captured)
	if len(rows) <= strings.Count(captured, "\n") {
		t.Fatalf("raw stream produced %d rows, no more than its %d newlines - the CUP row breaks were not honored",
			len(rows), strings.Count(captured, "\n")+1)
	}
	var ready string
	for _, row := range rows {
		if strings.Contains(row, "Ask Codex") {
			ready = row
			break
		}
	}
	if ready == "" {
		t.Fatalf("no reconstructed row mentions the ready prompt; rows=%q", rows)
	}
	if got := strings.TrimSpace(ready); got != "› Ask Codex to do anything" {
		t.Fatalf("ready row = %q, want the pane's real prompt line \"› Ask Codex to do anything\"", got)
	}
}

// TestStartupPrimeRecordsStartWireValueBlind is the observability half: which
// start-wire fields arrived has to be readable from the alloc, with names and
// presence only. The prompt body never reaches a log line.
func TestStartupPrimeRecordsStartWireValueBlind(t *testing.T) {
	rec := &primingRecorder{paneAnswers: []bool{true}}
	sc := startupTestSidecar(t)
	const name = "sess-wire-log"
	readyBinding(t, sc, name, func(b *binding) { b.PromptedAt = nil })
	var logged []string
	err := startupPrime(context.Background(), sc, rec.io(), func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}, name, stageConfig{
		Command: "codex", Nudge: "the prompt body", ProcessNames: []string{"codex"},
		ReadyPromptPrefix: "\u203a Ask Codex",
	})
	if err != nil {
		t.Fatalf("startupPrime: %v", err)
	}
	var wire string
	for _, line := range logged {
		if strings.Contains(line, " wire ") {
			wire = line
		}
	}
	if wire == "" {
		t.Fatalf("no value-blind start-wire line was logged; got %q", logged)
	}
	for _, want := range []string{
		"command=true", "nudge=true", "ready_prompt_prefix=true",
		"ready_delay_ms=0", "process_names=true", "prompt_source=nudge", "prompt_bytes=15",
	} {
		if !strings.Contains(wire, want) {
			t.Fatalf("start-wire line %q missing %q", wire, want)
		}
	}
	if strings.Contains(wire, "the prompt body") {
		t.Fatalf("the start-wire line leaked the prompt body: %q", wire)
	}
}
