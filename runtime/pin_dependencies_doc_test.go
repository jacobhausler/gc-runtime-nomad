package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docs/PIN-DEPENDENCIES.md states a blocking-set size in prose AND lists the
// Tier 1 rows in a table. Those two can disagree, and when they do the prose
// is what an adopter reads and the table is what is true.
//
// This is not a hypothetical failure mode. COMPATIBILITY.md carried exactly
// it: a retracted claim in one section and the same claim asserted as measured
// fact thirty lines later, for long enough that an outside reviewer found it
// rather than the authors.
//
// A guard that checked upstream PR state would be a network call in a unit
// test and would rot on its own; the doc provides a `gh` re-check loop for
// that. This checks only that the page agrees with ITSELF.
func TestPinDependenciesDocAgreesWithItself(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "docs", "PIN-DEPENDENCIES.md"))
	if err != nil {
		t.Fatalf("reading PIN-DEPENDENCIES.md: %v", err)
	}
	doc := string(body)

	tier1, ok := sectionBetween(doc, "### Tier 1", "### Tier 2")
	if !ok {
		t.Fatal("no Tier 1 section — this guard is pinned to a heading that moved")
	}

	var unfiled int
	for _, line := range strings.Split(tier1, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "| `") {
			continue // not a family row
		}
		if strings.Contains(trimmed, "**unfiled**") {
			unfiled++
		}
	}

	// The prose claim, spelled as a word because that is how the page reads.
	claims := map[string]int{
		"plus one unfiled follow-up":    1,
		"plus two unfiled follow-ups":   2,
		"plus three unfiled follow-ups": 3,
		"plus four unfiled follow-ups":  4,
	}
	found := false
	for phrase, n := range claims {
		if !strings.Contains(doc, phrase) {
			continue
		}
		found = true
		if n != unfiled {
			t.Errorf("the summary says %q but the Tier 1 table lists %d unfiled row(s). "+
				"The prose is what an adopter reads; the table is what is true.", phrase, unfiled)
		}
	}
	if !found {
		t.Errorf("the summary no longer states how many follow-ups are unfiled; the Tier 1 "+
			"table lists %d. An adopter reads the summary.", unfiled)
	}
}

// sectionBetween returns the text between two headings.
func sectionBetween(doc, start, end string) (string, bool) {
	i := strings.Index(doc, start)
	if i < 0 {
		return "", false
	}
	rest := doc[i:]
	if j := strings.Index(rest, end); j > 0 {
		return rest[:j], true
	}
	return rest, true
}

// TestReadmeNamesTheDiagnosticsFile closes a discoverability gap an outside
// SRE review found: diagnostics.log was named only in `check`'s stderr and in
// the debug skill. The README env table described GC_NOMAD_SIDECAR_DIR as
// bindings and markers and never mentioned that unsetting it destroys the only
// durable record this pack keeps.
//
// "The operator cannot find the log" and "there is no log" are the same
// outcome at 3am.
func TestReadmeNamesTheDiagnosticsFile(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	doc := string(body)
	if !strings.Contains(doc, diagnosticsFile) {
		t.Errorf("README.md never names %q, so an operator has no documented way to find "+
			"this pack's durable record of what went wrong", diagnosticsFile)
	}
	// It has to be on the row for the variable that CONTROLS it, not merely
	// somewhere in the page — that row is what someone configuring the pack
	// reads, and unsetting the variable is what silently removes the log.
	for _, line := range strings.Split(doc, "\n") {
		if strings.Contains(line, envSidecarDir) && strings.HasPrefix(strings.TrimSpace(line), "|") {
			if !strings.Contains(line, diagnosticsFile) {
				t.Errorf("the %s row does not mention %s:\n  %s", envSidecarDir, diagnosticsFile, line)
			}
			return
		}
	}
	t.Errorf("no env-table row for %s in README.md", envSidecarDir)
}
