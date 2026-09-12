package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestPackVersionMatchesTheTopReleaseNote pins pack.toml's declared version to
// the newest section of RELEASE-NOTES.md.
//
// These drifted: RELEASE-NOTES.md carried a `# v1.0.7` section documenting
// a BREAKING removal (GC_NOMAD_AGENT_LAUNCH_SCRIPT) while pack.toml still
// declared 1.0.6. pack.toml is what an installer reads, so the manifest was
// advertising a version whose documented contract the code no longer
// honoured — someone installing "1.0.6" got 1.0.7 behaviour, including a
// removed environment variable, with no way to tell from the manifest.
//
// Found 2026-09-12 by an outside reviewer reading only the repo's own docs.
func TestPackVersionMatchesTheTopReleaseNote(t *testing.T) {
	packTOML, err := os.ReadFile("../pack.toml")
	if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}
	declared := regexp.MustCompile(`(?m)^version\s*=\s*"([^"]+)"`).FindSubmatch(packTOML)
	if declared == nil {
		t.Fatal("pack.toml declares no version")
	}

	notes, err := os.ReadFile("../RELEASE-NOTES.md")
	if err != nil {
		t.Fatalf("reading RELEASE-NOTES.md: %v", err)
	}
	var newest string
	for _, line := range strings.Split(string(notes), "\n") {
		if rest, ok := strings.CutPrefix(line, "# v"); ok {
			newest = strings.TrimSpace(rest)
			break
		}
	}
	if newest == "" {
		t.Fatal("RELEASE-NOTES.md carries no `# v<version>` section")
	}

	if got := string(declared[1]); got != newest {
		t.Errorf("pack.toml version = %q, newest release note = %q.\n"+
			"pack.toml is what an installer reads; a lagging manifest advertises a\n"+
			"contract the code no longer honours. Bump pack.toml, or cut the note.", got, newest)
	}
}
