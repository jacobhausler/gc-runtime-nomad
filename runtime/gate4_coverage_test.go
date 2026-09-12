package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Gate 4 names eight causes a failure must be attributable to, and this asserts
// each one still has a message that names it. It exists because the audit is
// easy to get wrong in a way that looks right.
//
// A plain grep for one of these phrases reports MISSING when the phrase is
// split across concatenated Go literals — which is how most of them are
// written, because they are long. I ran exactly that grep, got a false MISSING
// for "a box that starts and never claims", and only caught it by checking the
// shipped binary, where the string is present. Source greps lie about
// concatenated strings; this normalises the concatenation first.

// goStringConcat matches the `" +\n\t\t"` seam between adjacent literals.
var goStringConcat = regexp.MustCompile(`"\s*\+\s*\n\s*"`)

// normalisedPackageSource returns every .go file in the package with Go string
// concatenation joined, so a phrase written across two literals reads as one.
func normalisedPackageSource(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package dir: %v", err)
	}
	var b strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		b.Write(goStringConcat.ReplaceAll(body, []byte("")))
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		t.Fatal("read no package source")
	}
	return b.String()
}

// TestGate4CausesAreEachNamed walks the eight causes deliberately, in the order
// the charge lists them.
func TestGate4CausesAreEachNamed(t *testing.T) {
	src := normalisedPackageSource(t)
	for _, c := range []struct{ cause, phrase string }{
		{"placement failure", "no client satisfied the job's constraints"},
		{"placement failure: empty node pool", "THE SCHEDULER SAW NO NODES AT ALL"},
		{"pull denied / image absent", "is not on this node"},
		{"pull impossible: no registry", "NAMES NO REGISTRY"},
		{"OOM", "OOM-killed"},
		{"constraint mismatch", "This job requires"},
		{"missing secrets", "secret key(s) in"},
		{"unreachable city (half-configured)", "NO remote city"},
		{"expired or revoked token", "a token WAS sent and refused"},
		{"no token at all", "no GC_NOMAD_TOKEN was sent"},
		{"a box that starts and never claims", "reports running because its state is UNKNOWN"},
		{"a box whose agent died: pane gone", "THE PANE'S COMMAND EXITED"},
	} {
		if !strings.Contains(src, c.phrase) {
			t.Errorf("gate 4 cause %q is no longer named: expected a message containing %q",
				c.cause, c.phrase)
		}
	}
}

// TestConcatNormalisationActuallyWorks guards the guard. If the normalisation
// broke, every phrase above would still be "found" or all would fail, and
// either way the coverage assertion would stop meaning anything.
func TestConcatNormalisationActuallyWorks(t *testing.T) {
	in := []byte("x := \"first half \" +\n\t\t\"second half\"\n")
	got := string(goStringConcat.ReplaceAll(in, []byte("")))
	if !strings.Contains(got, "first half second half") {
		t.Fatalf("normalisation did not join concatenated literals: %q", got)
	}
	// It must not join across a genuine boundary that is not a concatenation.
	in2 := []byte("a := \"one\"\nb := \"two\"\n")
	got2 := string(goStringConcat.ReplaceAll(in2, []byte("")))
	if strings.Contains(got2, "onetwo") {
		t.Errorf("normalisation joined two unrelated literals: %q", got2)
	}
}
