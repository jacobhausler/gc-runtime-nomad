package main

import (
	"strings"
	"testing"
)

// The capability gate answers "can the gc in this box claim". It cannot answer
// "which gc", and a stale-but-capable gc is the failure that actually happens:
// the pin rolls ~9 times a day here, the agent image is rebuilt separately, so
// the gc inside a box is routinely several rolls behind. Measured 2026-09-12 —
// live pin e78e884c1b, image carrying 3a3dda06a, three rolls back.
//
// It already cost a day: a box answered "--drain-ack requires --claim" while
// the host pin documented both modes. Capability present, behaviour different,
// nothing reporting it.

func TestGCCommitFromVersionJSON(t *testing.T) {
	real := `{"commit":"e78e884c1b","date":"2026-09-12T00:31:33-05:00","long":true,"ok":true,"schema_version":"1","version":"dev"}`
	if got := gcCommitFromVersionJSON(real); got != "e78e884c1b" {
		t.Errorf("got %q, want e78e884c1b", got)
	}
	// A DIFFERENT gc build's output is what this actually parses, so a shape
	// this pack was not compiled against must degrade to "unknown", never to a
	// wrong commit.
	for _, junk := range []string{
		"", "gc: command not found",
		`{"version":"dev"}`,
		`{"commit":"`,
		`{"commit":""}`,
	} {
		if got := gcCommitFromVersionJSON(junk); got != "" {
			t.Errorf("gcCommitFromVersionJSON(%q) = %q, want unknown", junk, got)
		}
	}
}

// TestShortAndFullShaAreNotDivergence is the false-positive guard that matters
// most. The host may report 40 chars where the image reports 9; reading that as
// staleness would send an operator to rebuild an image that was already right.
func TestShortAndFullShaAreNotDivergence(t *testing.T) {
	full := "e78e884c1b2c3d4e5f60718293a4b5c6d7e8f900"
	if gcCommitsDiverge(full, "e78e884c1b") {
		t.Error("a short sha of the same commit must not read as divergence")
	}
	if gcCommitsDiverge("e78e884c1b", full) {
		t.Error("divergence must be symmetric on prefix length")
	}
	if !gcCommitsDiverge("e78e884c1b", "3a3dda06a") {
		t.Error("two genuinely different commits must diverge — this is the live case")
	}
}

// TestUnknownIsNeverDivergence keeps the warning trustworthy. A warning that
// can cry wolf about the deepest dependency in the stack is worse than none.
func TestUnknownIsNeverDivergence(t *testing.T) {
	for _, c := range [][2]string{
		{"", "3a3dda06a"},
		{"e78e884c1b", ""},
		{"", ""},
	} {
		if gcCommitsDiverge(c[0], c[1]) {
			t.Errorf("gcCommitsDiverge(%q,%q) = true; could-not-tell must never read as differ", c[0], c[1])
		}
		if staleBoxGCWarning(c[0], c[1]) != "" {
			t.Errorf("staleBoxGCWarning(%q,%q) produced a warning from no evidence", c[0], c[1])
		}
	}
}

// TestStaleWarningNamesTheConsequence pins the message. "They differ" is not
// actionable; what it COSTS is.
func TestStaleWarningNamesTheConsequence(t *testing.T) {
	w := staleBoxGCWarning("e78e884c1b", "3a3dda06a")
	for _, want := range []struct{ needle, why string }{
		{"3a3dda06a", "the box's commit is not quoted, so the operator cannot tell which image"},
		{"e78e884c1b", "the host's commit is not quoted, so there is nothing to rebuild against"},
		{"invisible", "the consequence — gc changes not reaching the box — is not stated"},
		{"not a capability failure", "it does not distinguish itself from the capability gate, and a " +
			"reader who just saw that gate pass will assume this is the same thing"},
		{envAgentImage, "it names no remedy"},
	} {
		if !strings.Contains(w, want.needle) {
			t.Errorf("the stale-gc warning lacks %q: %s\n%s", want.needle, want.why, w)
		}
	}
}

// TestStalenessCheckIsWired guards the call site: the pure functions are
// worthless if nothing calls them, which is a mistake made twice already today.
func TestStalenessCheckIsWired(t *testing.T) {
	src := readSourceFile(t, "ops.go")
	probe := src[strings.Index(src, "func (l *lifecycle) warnIfBoxGCCannotClaim"):]
	if end := strings.Index(probe, "\nfunc (l *lifecycle) warnIfBoxGCIsStale"); end > 0 {
		probe = probe[:end]
	}
	if !strings.Contains(probe, "warnIfBoxGCIsStale") {
		t.Error("the launch probe checks capability but never identity, so a stale-but-capable gc " +
			"passes silently — which is the failure that actually happens here")
	}
}
