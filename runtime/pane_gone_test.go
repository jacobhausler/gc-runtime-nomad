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

// TestFSErrorBodyIsNotMistakenForAFile is a defect this code SHIPPED and then
// reported about itself in live output.
//
// Nomad's fs/cat can answer 200 with an error body rather than a 404, and
// readAllocFile correctly passes those bytes through as a successful read. The
// first version of diagnosePaneGone quoted them:
//
//	The box's own exit record says: "stat /opt/nomad/data/.../agent.exit: no
//	such file or directory"
//
// A confident answer assembled out of the instrument's own failure — the exact
// shape this pack exists to stop producing, produced by the code written to
// stop producing it.
func TestFSErrorBodyIsNotMistakenForAFile(t *testing.T) {
	for _, body := range []string{
		"stat /opt/nomad/data/alloc/547fe1e2/alloc/logs/agent.exit: no such file or directory",
		"open /alloc/logs/agent.stderr: permission denied",
		"unknown allocation ID \"547fe1e2\"",
		"Stat /x: No Such File Or Directory",
	} {
		if !isFSErrorBody(body) {
			t.Errorf("isFSErrorBody(%q) = false; this is the fs API complaining, not a file, and "+
				"quoting it produces a confident wrong answer", body)
		}
	}
	// Real records and real output must pass through.
	for _, body := range []string{
		"exit=1 started=1789221000 ended=1789221001 pane=%0 argv0=codex alloc=547fe1e2",
		"Traceback (most recent call last):",
		"codex: cannot open display",
	} {
		if isFSErrorBody(body) {
			t.Errorf("isFSErrorBody(%q) = true; this is real content and must not be discarded", body)
		}
	}
}

// TestExitRecordMustLookLikeOne is the positive half. A readable file at that
// path is not automatically the record, and "exit=" is the launcher's own
// documented field.
func TestExitRecordMustLookLikeOne(t *testing.T) {
	if !looksLikeAgentExitRecord("exit=0 started=a ended=b pane=%0 argv0=codex") {
		t.Error("a real exit record was rejected")
	}
	for _, body := range []string{
		"",
		"some other file that happened to be readable",
		"started=a ended=b",
	} {
		if looksLikeAgentExitRecord(body) {
			t.Errorf("looksLikeAgentExitRecord(%q) = true; without the exit field this is not the record", body)
		}
	}
}

// TestReadBoxFileRejectsTheFSErrorBody guards the WIRING, not the predicate.
//
// The first version of this guard asserted isFSErrorBody directly, which passes
// whether or not anything calls it — so removing the call site left the suite
// green while the defect was fully restored. The predicate is not the thing
// that was broken; using it is.
func TestReadBoxFileRejectsTheFSErrorBody(t *testing.T) {
	src := readSourceFile(t, "prime.go")
	fn := src[strings.Index(src, "func (l *lifecycle) readBoxFile("):]
	if end := strings.Index(fn, "\nfunc "); end > 0 {
		fn = fn[:end]
	}
	if fn == "" {
		t.Fatal("readBoxFile not found — this guard is pinned to a function that moved")
	}
	if !strings.Contains(fn, "isFSErrorBody(") {
		t.Error("readBoxFile no longer rejects an fs error body, so a 200 carrying " +
			"\"no such file or directory\" is quoted back as file content — which is how this " +
			"diagnosis reported a stat error as the box's own exit record")
	}
	diag := src[strings.Index(src, "func (l *lifecycle) diagnosePaneGone("):]
	if end := strings.Index(diag, "\nfunc "); end > 0 {
		diag = diag[:end]
	}
	if !strings.Contains(diag, "looksLikeAgentExitRecord(") {
		t.Error("diagnosePaneGone no longer checks that what it read IS an exit record; any " +
			"readable file at that path would be quoted as one")
	}
	if strings.Contains(diag, "l.client.readAllocFile(") {
		t.Error("diagnosePaneGone reads the alloc file directly again, bypassing the validation " +
			"in readBoxFile")
	}
}
