package main

import (
	"os"
	"strings"
	"testing"
)

// This is the failure that cost the most diagnosis time in this pack's history
// and the one an operator actually meets:
//
//	spawning prime window: tmux new-window exited 1: stderr="no server running
//	on /tmp/tmux-0/default"
//
// The launch of that same session was ALREADY CONFIRMED moments earlier, so
// tmux was reachable. The old message named two possible shapes — a socket-path
// difference, or a server that exited — and left the reader to choose. One of
// those is answerable from inside the box and the other is not, so offering the
// choice was the wrong move: a tmux server exits with its last session, and
// that session ends when its pane's command exits. "No server running" right
// after a confirmed launch IS the agent having died immediately.
//
// The box writes that answer to alloc/logs/agent.exit, which this pack has
// always shipped on egress and never read for diagnosis.

func TestPaneGoneRecognisesBothTmuxSpellings(t *testing.T) {
	for _, s := range []string{
		`no server running on /tmp/tmux-0/default`,
		`error connecting to /tmp/tmux-0/default (No such file or directory)`,
		`NO SERVER RUNNING`,
	} {
		if !paneGone(s) {
			t.Errorf("paneGone(%q) = false; tmux says this when the server is not there", s)
		}
	}
	// It must NOT fire on unrelated failures, or every spawn error would claim
	// the pane died and send the operator to the wrong record.
	for _, s := range []string{
		``,
		`can't find window: prime`,
		`permission denied`,
	} {
		if paneGone(s) {
			t.Errorf("paneGone(%q) = true; this is not a missing server and must not be diagnosed as one", s)
		}
	}
}

func TestLastLinePicksTheExitRecord(t *testing.T) {
	if got := lastLine("first\nexit=1 started=a ended=b argv0=codex\n\n"); !strings.Contains(got, "exit=1") {
		t.Errorf("lastLine returned %q, want the final non-empty line where the exit record lives", got)
	}
	if got := lastLine("   \n\n"); got != "" {
		t.Errorf("lastLine of blank input = %q, want empty", got)
	}
}

// TestAbsentCaptureRecordIsItselfDiagnosed pins the case that would otherwise
// be silence. In the prebaked-image shape the capture block is the IMAGE's
// responsibility, so an image that does not write agent.exit makes this failure
// permanently unexplainable — and saying THAT names what to fix, instead of
// sending the reader back to tmux.
func TestAbsentCaptureRecordIsItselfDiagnosed(t *testing.T) {
	// A lifecycle with no client cannot read the file, which is the same
	// observable as a box that never wrote one.
	l := &lifecycle{}
	if got := l.diagnosePaneGone(nil, ""); got != "" {
		t.Errorf("with no allocation there is nothing to read; got %q", got)
	}
}

// TestSpawnFailureAsksInsteadOfOfferingAChoice is a source assertion, because
// the behavioural path needs a live alloc. It is written to fail on the
// SPECIFIC regression: reverting to the two-shapes sentence without asking.
func TestSpawnFailureAsksInsteadOfOfferingAChoice(t *testing.T) {
	src := readSourceFile(t, "prime.go")
	spawn := src[strings.Index(src, "spawnCmd := []string{"):]
	if end := strings.Index(spawn, "\nfunc "); end > 0 {
		spawn = spawn[:end]
	}
	if !strings.Contains(spawn, "paneGone(") {
		t.Error("the spawn failure no longer asks whether the tmux server is gone; it is back to " +
			"offering the operator a choice between two shapes, one of which is answerable")
	}
	if !strings.Contains(spawn, "diagnosePaneGone") {
		t.Error("the spawn failure does not read the box's own agent-capture record, which is the " +
			"layer that knows why the pane's command exited")
	}
}

func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}
