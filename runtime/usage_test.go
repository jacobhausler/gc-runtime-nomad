package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A published binary's first contact is someone typing its name, or its name
// with --help. Before this, the bare call printed "missing operation" and
// named none of them, and `--help` printed NOTHING and exited 0 — because
// --help fell through the lifecycleOps forward-compatibility gate, which is
// correct for an unknown RPP op and wrong for a human.
//
// Gate 1 shape: a reader who cannot get a list of operations out of the
// binary has to read the source to use it, which is the same defect as
// docs that do not match the tool.

// Temp files, not os.Pipe: a usage dump is bigger than a handshake line and a
// pipe with no reader deadlocks at the buffer, which would look like a hung
// test rather than a large output.
func runCapture(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	mk := func(name string) *os.File {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	outF, errF := mk("stdout"), mk("stderr")
	code := run(args, strings.NewReader(""), outF, errF)
	outF.Close()
	errF.Close()
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	return code, read("stdout"), read("stderr")
}

func TestHelpListsEveryOperation(t *testing.T) {
	for _, flag := range []string{"help", "--help", "-h"} {
		code, out, errOut := runCapture(t, flag)
		if code != exitOK {
			t.Fatalf("%s: exit %d, want %d (stderr %q)", flag, code, exitOK, errOut)
		}
		body := out + errOut
		if strings.TrimSpace(body) == "" {
			t.Fatalf("%s: printed nothing — a published binary must answer its own name", flag)
		}
		// Every operation the binary actually serves, named. A new op added to
		// lifecycleOps without a usage line goes red here rather than being
		// undiscoverable.
		for op := range lifecycleOps {
			if !strings.Contains(body, op) {
				t.Errorf("%s: operation %q is served but not named in usage", flag, op)
			}
		}
		for _, op := range []string{"check", "protocol"} {
			if !strings.Contains(body, op) {
				t.Errorf("%s: operation %q is served but not named in usage", flag, op)
			}
		}
	}
}

func TestBareInvocationNamesTheOperations(t *testing.T) {
	code, out, errOut := runCapture(t)
	if code != exitError {
		t.Fatalf("bare: exit %d, want %d", code, exitError)
	}
	body := out + errOut
	if !strings.Contains(body, "missing operation") {
		t.Errorf("bare: lost the cause line; got %q", body)
	}
	if !strings.Contains(body, "start") || !strings.Contains(body, "check") {
		t.Errorf("bare: says an operation is missing without naming any; got %q", body)
	}
}

// The forward-compatibility contract the help surface must not break: an
// UNKNOWN op is still exit 2 and silent, so a newer gc probing for an op this
// build does not implement keeps getting the "not implemented" answer rather
// than a usage dump it would have to parse.
func TestUnknownOpStaysSilentExitTwo(t *testing.T) {
	code, out, errOut := runCapture(t, "some-future-op")
	if code != exitUnknown {
		t.Fatalf("unknown op: exit %d, want %d", code, exitUnknown)
	}
	if strings.TrimSpace(out+errOut) != "" {
		t.Fatalf("unknown op: printed %q, want silence", out+errOut)
	}
}
