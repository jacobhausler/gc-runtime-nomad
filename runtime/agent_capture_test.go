package main

import (
	"strings"
	"testing"
)

// WHY THIS EXISTS, in the pack's own words. runtime/ops.go documents an
// agent-capture trio — agent.exit, agent.stdout, agent.stderr — egressFiles
// copies all three before deregister, jobspec.go declares a vector source for
// agent.exit with an exact filename and a documented atomic write convention
// (agent.exit.$$ then rename), and prime.go reads it and formats a specific
// error when it is missing.
//
// Nothing wrote any of them. The doc attributes them to "the deployment
// bootstrap ops/nomad/agent-launch.sh", which was DELETED with the launch
// script seam — so in the only session shape that exists, the writer is gone
// and every reader is looking at a file that is never created.
//
// The cost, measured live on 2026-09-12, six times in one supervisor era:
//
//	tmux new-window exited 1: stderr="no server running on /tmp/tmux-0/default"
//	The pane's command has exited ... but this box wrote NO usable
//	agent-capture record at alloc/logs/agent.exit, so why it exited cannot be
//	recovered from here.
//
// A box that starts and never claims, with the reason permanently
// unrecoverable — and the pack blaming the image for a record the pack itself
// is now the only thing positioned to write. This is gate 4's last cause:
// every failure names its cause at the layer that knows.

func paneBody(t *testing.T) string {
	t.Helper()
	cmd := buildLaunchCommandForPane(map[string]string{}, "", "codex --model x")
	// The pane body is the argument after `-c` in the INNER /bin/sh the tmux
	// pane runs, which is the last element of the launch argv.
	body := cmd[len(cmd)-1]
	if !strings.Contains(body, "codex --model x") {
		t.Fatalf("did not find the pane body in the launch argv; got %q", body)
	}
	return body
}

func TestPaneWritesTheAgentExitRecord(t *testing.T) {
	body := paneBody(t)
	if !strings.Contains(body, "agent.exit") {
		t.Fatalf("the pane never writes agent.exit, so an agent that dies at once is permanently unexplainable:\n%s", body)
	}
	// The record prime.go actually accepts. A file with no exit= is rejected by
	// looksLikeAgentExitRecord, which is the same as writing nothing.
	if !strings.Contains(body, "exit=") {
		t.Errorf("the record carries no exit= field, so looksLikeAgentExitRecord rejects it:\n%s", body)
	}
	if !looksLikeAgentExitRecord(body) {
		t.Errorf("the pack's own reader would not recognise what the pane writes")
	}
	// The atomic convention jobspec.go documents and depends on: it declares
	// ONE exact filename with no glob precisely because the transient file is
	// written alongside and renamed in.
	if !strings.Contains(body, "agent.exit.$$") || !strings.Contains(body, "mv") {
		t.Errorf("the record is not written atomically (temp + rename), so a reader can see a partial line and the exact-filename vector source can match the transient file:\n%s", body)
	}
}

// THIS TEST ASSERTED THE OPPOSITE AND WAS WRONG. It required the pane to
// append the agent's stderr to alloc/logs/agent.stderr, which is what the
// egress doc describes — and implementing it broke the agent:
// TestRunDrivingVerbsOverTmux went red at clear-scrollback, one verb after
// `interrupt`, because the pane had died. Bisected to the redirect itself,
// with the trap ruled out separately.
//
// Taking a stream off the tty changes the behaviour of a program driven AS an
// interactive tty client, which is how gc drives the agent (peek, nudge,
// interrupt). The egress doc specifies `tmux pipe-pane` for both output
// channels for exactly this reason: a tap, not a redirect. That tap is not
// implemented; this asserts only that the fix is not a redirect, so nobody
// re-adds one from reading the doc.
func TestPaneDoesNotRedirectAgentStreams(t *testing.T) {
	body := paneBody(t)
	if strings.Contains(body, `2>>`) || strings.Contains(body, "codex --model x; } 2>") {
		t.Errorf("the pane redirects the agent's stderr off its tty; measured to change the agent's behaviour and kill the pane on interrupt:\n%s", body)
	}
	if strings.Contains(body, "| tee") || strings.Contains(body, "agent.stdout\"") {
		t.Errorf("the pane pipes the agent's stdout, which takes its tty away; the doc's mechanism is tmux pipe-pane:\n%s", body)
	}
}

// The capture must not eat the failure. If the wrapper exits 0 after a failed
// agent command, tmux and the pack both see a successful launch and the box
// becomes a shell that reports healthy — the exact defect shape this pack
// exists to stop producing, reintroduced by the diagnostics for it.
func TestCaptureDoesNotSwallowTheExitStatus(t *testing.T) {
	body := paneBody(t)
	if !strings.Contains(body, "exit $") && !strings.Contains(body, "exit \"$") {
		t.Fatalf("the pane body does not re-exit with the agent's status, so a failed launch reports success:\n%s", body)
	}
}

// gc's rendered pane command begins with `exec`, which is what made the first
// version of this wrapper a no-op: exec replaces the shell, so nothing after
// it runs and the exit record is never written. Found by an existing guard
// going red on the wrapped body, not by reading the code.
func TestLeadingExecIsDroppedSoTheWrapperSurvives(t *testing.T) {
	body := buildLaunchCommandForPane(map[string]string{}, "", "exec codex --model x")
	got := body[len(body)-1]
	if strings.Contains(got, "{ exec codex") {
		t.Fatalf("the pane still execs, so the exit record is never written:\n%s", got)
	}
	if !strings.Contains(got, "{ codex --model x; }") {
		t.Errorf("the rendered command did not survive the exec strip:\n%s", got)
	}
	// argv0 must be the AGENT, not the shell keyword.
	if !strings.Contains(got, "argv0=%s") || !strings.Contains(got, "'codex'") {
		t.Errorf("argv0 is not the agent command's own name:\n%s", got)
	}
}

// Only the exact word. A command whose name merely starts with those letters
// must be untouched, or the wrapper would silently corrupt it.
func TestStripLeadingExecTakesOnlyTheWord(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  string
		strip bool
	}{
		{"exec codex", "codex", true},
		{"exec\tcodex", "codex", true},
		{"execute-agent --go", "execute-agent --go", false},
		{"exec", "exec", false},
		{"codex exec run", "codex exec run", false},
	} {
		got, ok := stripLeadingExec(tc.in)
		if got != tc.want || ok != tc.strip {
			t.Errorf("stripLeadingExec(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.strip)
		}
	}
}
