package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiagnosticsSurviveADiscardedStderr pins the fix for the pack's own worst
// blind spot: gc's exec transport turns a nonzero op exit into "tmux exited N"
// and throws this process's stderr away, so everything the pack knows about a
// failure vanishes at the moment it is needed.
//
// Measured today: a lane went down twice and every failure read "spawning
// prime window: tmux new-window exited 1:" with EMPTY output. The pack had
// more to say and had already said it, to a channel nobody keeps.
func TestDiagnosticsSurviveADiscardedStderr(t *testing.T) {
	dir := t.TempDir()
	d := newDiagnosticsSink(dir)
	d.write("start", "FAILED: spawning prime window: exited 1")
	d.write("stop", "path=reap-then-provision-fresh")

	body, err := os.ReadFile(filepath.Join(dir, diagnosticsFile))
	if err != nil {
		t.Fatalf("no diagnostics file: %v", err)
	}
	got := string(body)
	for _, want := range []string{"start", "FAILED: spawning prime window", "stop", "reap-then-provision-fresh"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostics missing %q:\n%s", want, got)
		}
	}
	if lines := strings.Count(strings.TrimSpace(got), "\n") + 1; lines != 2 {
		t.Errorf("want 2 appended lines, got %d:\n%s", lines, got)
	}
}

// TestDiagnosticsNeverBreakTheOpTheyDescribe: a diagnostic that can fail the
// operation it is reporting on is worse than none. An unset sidecar dir and an
// unwritable one must both be silent no-ops.
func TestDiagnosticsNeverBreakTheOpTheyDescribe(t *testing.T) {
	newDiagnosticsSink("").write("start", "no dir configured")
	newDiagnosticsSink(filepath.Join(t.TempDir(), "does", "not", "exist")).write("start", "unwritable")
	newDiagnosticsSink("   ").write("start", "whitespace dir")
}

// TestDiagnosticsAreBounded: an unattended deployment must not fill a disk
// with its own commentary.
func TestDiagnosticsAreBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, diagnosticsFile)
	if err := os.WriteFile(path, make([]byte, diagnosticsCap+1), 0o600); err != nil {
		t.Fatal(err)
	}
	newDiagnosticsSink(dir).write("start", "after the cap")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > diagnosticsCap {
		t.Errorf("file is %d bytes, still over the %d cap", fi.Size(), diagnosticsCap)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "truncated at cap") {
		t.Error("truncation is silent; an operator reading a short log must know why it is short")
	}
	if !strings.Contains(string(body), "after the cap") {
		t.Error("the line that triggered truncation was lost")
	}
}
