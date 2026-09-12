package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The declared minimum exists in two places on purpose: in code, where `check`
// can report it, and in docs/PIN-DEPENDENCIES.md, where an adopter reads it.
// Two copies of a list is exactly how a page starts lying — COMPATIBILITY.md
// already did that once this week — so they are asserted to agree.

var tier1RowRe = regexp.MustCompile("^\\|\\s*`([^`]+)`")

// docTier1Families reads the family names out of the Tier 1 table.
func docTier1Families(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "docs", "PIN-DEPENDENCIES.md"))
	if err != nil {
		t.Fatalf("reading PIN-DEPENDENCIES.md: %v", err)
	}
	sec, ok := sectionBetween(string(body), "### Tier 1", "### Tier 2")
	if !ok {
		t.Fatal("no Tier 1 section — this guard is pinned to a heading that moved")
	}
	out := map[string]bool{}
	for _, line := range strings.Split(sec, "\n") {
		m := tier1RowRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		// A row may name an alias pair, e.g. `remote-worker` / `remote-worker-lifecycle`.
		for _, part := range strings.Split(m[1], "/") {
			if p := strings.TrimSpace(part); p != "" {
				out[p] = true
			}
		}
		for _, extra := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(strings.TrimSpace(line), -1) {
			out[strings.TrimSpace(extra[1])] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("parsed no families out of the Tier 1 table")
	}
	return out
}

// TestDeclaredMinimumMatchesTheDoc is the anti-drift assertion. Every family the
// CODE declares must appear in the doc's Tier 1 table, so `check` can never
// report a requirement the page does not explain.
func TestDeclaredMinimumMatchesTheDoc(t *testing.T) {
	doc := docTier1Families(t)
	for _, r := range tier1Requirements() {
		if !doc[r.Family] {
			t.Errorf("the code declares Tier 1 family %q but docs/PIN-DEPENDENCIES.md's Tier 1 table "+
				"does not list it — `check` would report a requirement the page cannot explain", r.Family)
		}
	}
}

// TestEveryRequirementCarriesASymptom keeps the declaration useful. A family
// name alone tells an adopter nothing; "without it, X happens" is what lets
// them recognise the failure when they hit it.
func TestEveryRequirementCarriesASymptom(t *testing.T) {
	for _, r := range tier1Requirements() {
		if strings.TrimSpace(r.Why) == "" {
			t.Errorf("requirement %q declares no symptom", r.Family)
		}
		if strings.TrimSpace(r.Upstream) == "" {
			t.Errorf("requirement %q names no upstream location, not even \"unfiled\"", r.Family)
		}
	}
}

// TestUnverifiableRequirementsAreListedNotCounted is the honesty property.
//
// Before the declaration existed, `check` tested the one family it could and
// said nothing about the rest, so a green check read as "compatible" when it
// meant "the one thing I can test passed". A COUNT would not fix that: "18
// families undetected" is a statistic an adopter cannot act on. The list is
// what they can check against their own gc.
func TestUnverifiableRequirementsAreListedNotCounted(t *testing.T) {
	var buf bytes.Buffer
	reportGCRequirements(&buf, gcWorkerSurfacePresent)
	got := buf.String()

	if !strings.Contains(got, "does NOT") {
		t.Error("the report does not warn that a green check fails to imply the unverifiable " +
			"families are present — which is the whole reason it exists")
	}
	if !strings.Contains(got, "CAPABILITY set") {
		t.Error("the report does not explain why there is no version to compare; an adopter will " +
			"otherwise go looking for a version number that is always the string \"dev\"")
	}
	for _, r := range tier1Requirements() {
		if r.Probeable {
			continue
		}
		if !strings.Contains(got, r.Family) {
			t.Errorf("unverifiable family %q is not named in the report; it must be listed, not "+
				"summarised into a count", r.Family)
		}
		if !strings.Contains(got, r.Upstream) {
			t.Errorf("family %q is listed without its upstream location", r.Family)
		}
	}
}
