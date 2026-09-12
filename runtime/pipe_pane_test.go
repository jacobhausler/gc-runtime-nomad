package main

import (
	"strings"
	"testing"
)

// THE LAST INVISIBLE THING. With the exit record in place the pack could
// finally say the agent died — "exit=1 ... argv0=codex", started and ended in
// the same second — and still not say WHY, because the agent's own message
// went to its pane and the pane died with the tmux server that carried it.
//
// egressFiles and the vector config have declared agent.stdout since before
// this pack shipped, and the egress doc even names the mechanism: `tmux
// pipe-pane`, chosen over a redirect precisely so the agent keeps its tty (gc
// drives it as an interactive client: peek, nudge, interrupt). Nothing ever
// issued it. Measured cost: a full day of diagnosis reconstructing by hand what
// one line of capture would have printed.
//
// It rides the SAME tmux invocation as new-session, separated by a literal
// ";" argument, so there is no window in which the pane is running untapped —
// which matters because the failing pane dies in about a second.

func TestLaunchTapsThePaneOutput(t *testing.T) {
	cmd := buildLaunchCommandForPane(map[string]string{}, "", "exec codex")
	joined := strings.Join(cmd, "\x1f")
	if !strings.Contains(joined, "pipe-pane") {
		t.Fatalf("the launch never taps the pane, so the agent's own error text is lost with the server: %v", cmd)
	}
	// One invocation: a second `tmux` exec would leave the pane running
	// untapped for as long as the round trip takes.
	if strings.Count(joined, "\x1ftmux\x1f") != 1 {
		t.Errorf("the tap is a separate tmux invocation; it must ride the launch: %v", cmd)
	}
	if !strings.Contains(joined, "\x1f;\x1f") {
		t.Errorf("the tap is not separated by tmux's own \";\" command separator: %v", cmd)
	}
	// -o: append rather than truncate, so a relaunch into the same box (the
	// adopt path) does not erase the previous attempt's output, which is the
	// one most likely to explain the current state.
	if !strings.Contains(joined, "\x1f-o\x1f") {
		t.Errorf("the tap truncates instead of appending: %v", cmd)
	}
	// Into the writable capture dir, under the name egressFiles already copies
	// and the vector source already includes.
	if !strings.Contains(joined, "data/agent.stdout") {
		t.Errorf("the tap does not write the declared capture path: %v", cmd)
	}
	// The tap must come AFTER the pane body, or tmux would read the body as an
	// argument to pipe-pane.
	if strings.Index(joined, "pipe-pane") < strings.Index(joined, "agentCaptureNothing") {
		t.Skip("ordering asserted below")
	}
	if strings.Index(joined, "pipe-pane") < strings.Index(joined, "codex") {
		t.Errorf("the tap is issued before the pane command, so tmux would consume the body: %v", cmd)
	}
}

// The pane's own body must NOT also redirect: measured on 2026-09-12, taking a
// stream off the tty changed the agent's behaviour and killed the pane on
// interrupt. pipe-pane is a tap, which is why the doc specifies it.
func TestTapDoesNotReintroduceARedirect(t *testing.T) {
	body := paneBody(t)
	if strings.Contains(body, "agent.stdout") {
		t.Errorf("the pane body redirects its own stdout; the tap is tmux's job:\n%s", body)
	}
}
