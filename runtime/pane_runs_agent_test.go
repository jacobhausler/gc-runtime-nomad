package main

import (
	"strings"
	"testing"
)

// cr-8eagyh. A box whose pane holds a bare shell is not a launched agent, but it
// is a live process — which is why every pid-based check downstream reported it
// healthy while the lane claimed nothing for six minutes with a P0 routed to it.
func TestPaneRunsAgent(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
		why  string
	}{
		{"codex", true, "the agent itself is what a launched pane should be running"},
		{"node", true, "any non-shell foreground process counts — the pack must not hardcode one agent binary"},
		{"sh", false, "the measured failure: launch exited 0 and left the pane on its parent shell"},
		{"bash", false, "same shape, different image default"},
		{"dash", false, "debian's /bin/sh"},
		{"zsh", false, "same"},
		{" sh ", false, "whitespace must not smuggle a shell past the check"},
		{"", false, "an empty reading is not an agent — and the caller must treat it as 'no answer', not 'dead'"},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			if got := paneRunsAgent(tc.cmd); got != tc.want {
				t.Fatalf("paneRunsAgent(%q) = %v, want %v — %s", tc.cmd, got, tc.want, tc.why)
			}
		})
	}
}

// The refusal has to be loud enough to act on: the whole failure it replaces was
// silent, so an operator's only symptom was a lane that served nothing.
func TestBareShellRefusalNamesTheCause(t *testing.T) {
	// Mirrors the message built in verifyPaneRunsAgent.
	if !strings.Contains(bareShellRefusalHint(), "cr-8eagyh") {
		t.Fatal("the refusal must point at the bead carrying the evidence")
	}
	for _, want := range []string{"started nothing", "rendered launch command"} {
		if !strings.Contains(bareShellRefusalHint(), want) {
			t.Fatalf("refusal hint missing %q — it must say what went wrong and what the box needs", want)
		}
	}
}
