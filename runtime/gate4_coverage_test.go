package main

import (
	"go/parser"
	"go/token"
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

// stripComments blanks every comment in a Go source file, keeping byte offsets
// so the concatenation normalisation below still lines up.
//
// WITHOUT THIS THE WHOLE ASSERTION BELOW IS VACUOUS, and it was. Every one of
// these phrases is ALSO quoted in a doc comment somewhere nearby — that is how
// this package documents its own error text — so a phrase deleted from the CODE
// is still found in the COMMENT that describes it, and the guard reports the
// cause as still named. Measured: breaking "The box's own exit record says" in
// prime.go:538 left the test passing, because prime.go:566 quotes the same
// sentence in a comment.
//
// A guard that cannot fail is not a guard, and this one could not fail for any
// of its twelve causes.
func stripComments(t *testing.T, name, body string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, body, parser.ParseComments)
	if err != nil {
		// An unparseable file is a real problem, but not this test's problem:
		// go vet and the build catch it. Fall back to the raw body rather than
		// failing the coverage audit for a syntax error elsewhere.
		return body
	}
	out := []byte(body)
	base := fset.File(f.Pos()).Base()
	for _, group := range f.Comments {
		for _, c := range group.List {
			start := int(c.Pos()) - base
			end := int(c.End()) - base
			if start < 0 || end > len(out) || start >= end {
				continue
			}
			for i := start; i < end; i++ {
				if out[i] != '\n' {
					out[i] = ' '
				}
			}
		}
	}
	return string(out)
}

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
		raw, err := os.ReadFile(name)
		body := []byte(stripComments(t, name, string(raw)))
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
		// NAMING A CAUSE IS NOT EXPLAINING IT, and the three below are what
		// separates the two. For days the lane failed with the pane-gone
		// message above — correct, and useless: it said the agent had died and
		// could not say why, because the record it told the reader to go find
		// was never written by anything and the agent's own words went to a
		// pane that died with its tmux server.
		//
		// So gate 4 is asserted on the EVIDENCE too, not only on the
		// vocabulary. Each of these is a channel that must keep existing:
		{"the box's own exit record is quoted", "The box's own exit record says"},
		{"the agent's own last words are quoted", "Its stderr ended"},
		{"orchestrator-only paths in argv are named", "PATHS THAT DO NOT EXIST IN IT"},
		{"a fetched gc that cannot load", "THE gc IN THE BOX CANNOT START"},
		{"a credential staged into the process environment", "credential-shaped key(s) into the agent's PROCESS ENVIRONMENT"},
		// An artifact that will not fetch was the last cause on this list to be
		// a generic "never started", and it is the one that took the whole
		// fleet down on 2026-09-12: a pin rolled, no gc was published for the
		// new commit, and every box 404'd a URL nobody had configured. The
		// cause has to say whose fix it is, because the two artifact URLs this
		// pack declares have opposite remedies — one is derived from the live
		// pin and one is set by hand.
		{"an artifact that will not fetch", "FETCH fault, not a start fault"},
		{"a derived artifact URL nobody configured", "NOT CONFIGURED ANYWHERE"},
		{"an artifact that arrived and failed verification", "bytes ARRIVED and failed verification"},
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
