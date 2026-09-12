package main

import (
	"strings"
	"testing"
)

// cr-8eagyh. The pack hardcoded codex's TUI strings in the prime script while the
// README promised "nothing provider-specific lives in the runtime". A non-codex
// agent behind prime therefore got silently wrong ready/busy detection — the
// assumption was disclosed only in a code comment. These are provider facts and
// must be configurable, with the codex values as defaults so no existing
// deployment changes behaviour.
func TestPrimeMarkersAreConfigurable(t *testing.T) {
	t.Run("defaults preserve the previous codex behaviour", func(t *testing.T) {
		var cfg stageConfig
		if got := cfg.loadingMarker(); got != "loading" {
			t.Errorf("loadingMarker default = %q, want %q", got, "loading")
		}
		if got := cfg.busyMarker(); got != "esc to interrupt" {
			t.Errorf("busyMarker default = %q, want %q", got, "esc to interrupt")
		}
	})

	t.Run("a different agent family can state its own", func(t *testing.T) {
		cfg := stageConfig{Env: map[string]string{envPrimeLoadingMarker: "starting up", envPrimeBusyMarker: "working…"}}
		if cfg.loadingMarker() != "starting up" || cfg.busyMarker() != "working…" {
			t.Fatalf("configured markers not honoured: %q / %q", cfg.loadingMarker(), cfg.busyMarker())
		}
	})

	t.Run("markers are staged into the box, not baked into the script", func(t *testing.T) {
		files := buildPrimeFiles("hello", "› Ask Codex", "starting up", "working…", 1500)
		var sawLoading, sawBusy bool
		for _, f := range files {
			switch f.Path {
			case primeLoadingFile:
				sawLoading = true
				if string(f.Content) != "starting up" {
					t.Errorf("loading marker staged as %q", f.Content)
				}
			case primeBusyFile:
				sawBusy = true
				if string(f.Content) != "working…" {
					t.Errorf("busy marker staged as %q", f.Content)
				}
			}
		}
		if !sawLoading || !sawBusy {
			t.Fatalf("markers not staged (loading=%v busy=%v) — the script reads them from disk like ready_prefix.txt", sawLoading, sawBusy)
		}
	})

	t.Run("the script no longer names codex strings literally", func(t *testing.T) {
		// The whole point: these must come from the staged files, not the script.
		if strings.Contains(primeScript, "'esc to interrupt'") {
			t.Error("primeScript still hardcodes codex's busy indicator")
		}
		if strings.Contains(primeScript, "grep -q 'loading'") {
			t.Error("primeScript still hardcodes codex's loading marker")
		}
		for _, want := range []string{"loading_marker.txt", "busy_marker.txt"} {
			if !strings.Contains(primeScript, want) {
				t.Errorf("primeScript does not read %s", want)
			}
		}
	})
}
