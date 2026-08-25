package main

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gc-runtime-nomad/fakenomad"
)

func TestRun(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
		want  int
	}{
		{"missing op", nil, "", exitError},
		{"protocol", []string{"protocol"}, "", exitOK},
		{"unknown op", []string{"get-last-activity", "s1"}, "", exitUnknown},
		{"lifecycle op missing session name", []string{"start"}, "", exitError},
		{"provision missing session name", []string{"provision"}, "", exitError},
		{"relaunch missing session name", []string{"relaunch"}, "", exitError},
		{"exec missing session name", []string{"exec"}, "", exitError},
		{"exec missing command on stdin", []string{"exec", "s1"}, "", exitError},
		{"nudge missing session name", []string{"nudge"}, "", exitError},
		{"peek missing session name", []string{"peek"}, "", exitError},
		{"interrupt missing session name", []string{"interrupt"}, "", exitError},
		{"send-keys missing session name", []string{"send-keys"}, "", exitError},
		{"clear-scrollback missing session name", []string{"clear-scrollback"}, "", exitError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got := run(tc.args, strings.NewReader(tc.stdin), w, w)
			w.Close()
			if got != tc.want {
				t.Fatalf("run(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// TestProtocolHandshakeDeclaresProvisionAndExec confirms the protocol
// handshake advertises proc.provision and proc.exec now that both are
// implemented — a provision-capable pack must also declare exec (the
// controller launches the agent over exec after provision).
func TestProtocolHandshakeDeclaresProvisionAndExec(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := run([]string{"protocol"}, strings.NewReader(""), w, w); got != exitOK {
		t.Fatalf("run(protocol) = %d, want %d", got, exitOK)
	}
	w.Close()
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		t.Fatalf("reading protocol output: %v", err)
	}
	for _, capName := range []string{"proc.provision", "proc.exec"} {
		if !strings.Contains(line, capName) {
			t.Fatalf("protocol handshake %q missing capability %q", line, capName)
		}
	}
}

// TestRunExecReadsCommandFromStdin is a regression test for the exec op's
// calling convention: docs/reference/exec-session-provider.md carries the
// command on stdin (invocation is just `exec <name>`), not extra argv. A
// prior revision read it from argv instead, which silently broke
// RPP-CONN-001's exit-code fidelity check because no real command ever
// reached the alloc.
func TestRunExecReadsCommandFromStdin(t *testing.T) {
	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)

	t.Setenv(envAddr, srv.URL())
	t.Setenv(envToken, "")
	t.Setenv(envNamespace, "")
	t.Setenv(envParentJob, "gc-sessions")
	t.Setenv(envSidecarDir, t.TempDir())

	const session = "sess-exec-cli"

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if got := run([]string{"start", session}, strings.NewReader(""), w, w); got != exitOK {
		w.Close()
		out, _ := io.ReadAll(r)
		t.Fatalf("start = %d, want %d (output: %s)", got, exitOK, out)
	}
	w.Close()
	r.Close()

	r, w, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	got := run([]string{"exec", session}, strings.NewReader("echo hi-from-stdin"), w, w)
	w.Close()
	out, _ := io.ReadAll(r)
	if got != exitOK {
		t.Fatalf("exec exit = %d, want %d (output: %s)", got, exitOK, out)
	}
	if !strings.Contains(string(out), "hi-from-stdin") {
		t.Fatalf("exec stdout = %q, want it to contain the echoed command output", out)
	}
}

// TestRunDrivingVerbsOverTmux exercises nudge/peek/send-keys/interrupt/
// clear-scrollback end to end against a real tmux session inside a real
// alloc (fakenomad's exec actually forks the command, NRT-P1-05): proof
// these driving verbs are genuine tmux commands sent over exec, not canned
// replies. Skips if tmux isn't on PATH rather than failing, keeping the
// hermetic offline suite green on a machine without it.
func TestRunDrivingVerbsOverTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)

	t.Setenv(envAddr, srv.URL())
	t.Setenv(envToken, "")
	t.Setenv(envNamespace, "")
	t.Setenv(envParentJob, "gc-sessions")
	t.Setenv(envSidecarDir, t.TempDir())

	const session = "sess-driving-verbs"

	runOp := func(args []string, stdin string) (string, int) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		got := run(args, strings.NewReader(stdin), w, w)
		w.Close()
		out, _ := io.ReadAll(r)
		r.Close()
		return string(out), got
	}

	if out, got := runOp([]string{"start", session}, ""); got != exitOK {
		t.Fatalf("start = %d, want %d (output: %s)", got, exitOK, out)
	}

	if out, got := runOp([]string{"send-keys", session, "echo GC_SEND_KEYS_OK", "Enter"}, ""); got != exitOK {
		t.Fatalf("send-keys = %d, want %d (output: %s)", got, exitOK, out)
	}
	waitForPeek(t, runOp, session, "GC_SEND_KEYS_OK")

	if out, got := runOp([]string{"nudge", session}, "echo GC_NUDGE_OK"); got != exitOK {
		t.Fatalf("nudge = %d, want %d (output: %s)", got, exitOK, out)
	}
	waitForPeek(t, runOp, session, "GC_NUDGE_OK")

	if out, got := runOp([]string{"interrupt", session}, ""); got != exitOK {
		t.Fatalf("interrupt = %d, want %d (output: %s)", got, exitOK, out)
	}

	if out, got := runOp([]string{"clear-scrollback", session}, ""); got != exitOK {
		t.Fatalf("clear-scrollback = %d, want %d (output: %s)", got, exitOK, out)
	}
}

// waitForPeek polls `peek <session>` until its output contains want or a
// short timeout elapses — send-keys/nudge queue keys into the pane
// asynchronously, so the shell needs a moment to execute and echo them.
func waitForPeek(t *testing.T, runOp func([]string, string) (string, int), session, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, got := runOp([]string{"peek", session}, "")
		if got != exitOK {
			t.Fatalf("peek = %d, want %d (output: %s)", got, exitOK, out)
		}
		last = out
		if strings.Contains(out, want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("peek never showed %q, last output: %s", want, last)
}

// TestRunLifecycleOpRequiresConfig confirms a lifecycle op fails loudly
// (not silently, not exit-2) when GC_NOMAD_ADDR/GC_NOMAD_SIDECAR_DIR are
// unset, so a misconfigured pack never masquerades as "op unimplemented".
func TestRunLifecycleOpRequiresConfig(t *testing.T) {
	t.Setenv(envAddr, "")
	t.Setenv(envSidecarDir, "")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := run([]string{"is-running", "s1"}, strings.NewReader(""), w, w)
	w.Close()
	if got != exitError {
		t.Fatalf("run(is-running) with no config = %d, want %d (exitError)", got, exitError)
	}
}
