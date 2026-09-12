package main

import (
	"os"
	"strings"
	"testing"
)

// The pack ships its own diagnostic skill (gc convention: <packDir>/skills/,
// discovered by DiscoverPackAttachmentRoots — no declaration needed). A skill
// is documentation that gets FOLLOWED rather than read, so a falsified line in
// it is worse than a missing one: it sends an operator down a path the code
// does not support. One line in this pack's estate-side sibling was falsified
// by a change in this very repo and went unnoticed until it was graded by
// hand.
//
// These guards pin the claims that are checkable against the code.
const skillDoc = "../skills/gc-nomad-debug/SKILL.md"

func readSkillDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(skillDoc)
	if err != nil {
		t.Fatalf("reading %s: %v", skillDoc, err)
	}
	return string(b)
}

// TestSkillNamesEveryFatalPlacementRule: the skill tells a reader which
// constraints decide placement. A rule added without updating it leaves an
// operator debugging a cluster that can never place.
func TestSkillNamesEveryFatalPlacementRule(t *testing.T) {
	doc := readSkillDoc(t)
	job := parentJobSpec("gc", "", "gc-sessions", logShipperConfig{}, "img", placementConfig{})
	for _, c := range job.Constraints {
		attr := strings.TrimSuffix(strings.TrimPrefix(c.LTarget, "${attr."), "}")
		if !strings.Contains(doc, attr) {
			t.Errorf("%s never mentions the fatal placement rule %s %s %s", skillDoc, c.LTarget, c.Operand, c.RTarget)
		}
	}
}

// TestSkillNamesTheBindingFields: layer 6 is the only layer holding these, and
// the skill says so by name. If the binding grows or loses a field the skill
// stops describing the instrument it points at.
func TestSkillNamesTheBindingFields(t *testing.T) {
	doc := readSkillDoc(t)
	for _, field := range []string{"ChildJobID", "Launched", "AgentPID", "StagePending"} {
		if !strings.Contains(doc, field) {
			t.Errorf("%s does not name the binding field %q that layer 6 reads", skillDoc, field)
		}
	}
}

// TestSkillNamesTheProcessProbe pins the ONE binary this skill names, and
// deliberately not the whole image contract.
//
// The skill's own rule is to point at the tables rather than copy them,
// because every hand-copied subset of them has drifted — COMPATIBILITY.md owns
// the binary list and carries its own guard. `ps` is the exception worth
// naming here because it is diagnostically load-bearing: it is the instrument
// that tells a working seat from an empty box, and an image without it makes
// that diagnosis impossible for the life of the deployment.
func TestSkillNamesTheProcessProbe(t *testing.T) {
	doc := readSkillDoc(t)
	// Backticked tokens, not bare substrings: an earlier version of this guard
	// looked for "ps" anywhere, which "steps" and "helps" satisfy, so it stayed
	// green against a doc with the requirement deliberately removed.
	for _, want := range []string{"`ps -eo comm`", "`ps`"} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s does not name %s, the probe that separates a working seat from an empty box", skillDoc, want)
		}
	}
}

// TestSkillPointsAtTheRealEntryPoint: `check` is the first instrument the skill
// tells a reader to run, so the op must exist and be spelled the way the skill
// spells it.
func TestSkillPointsAtTheRealEntryPoint(t *testing.T) {
	doc := readSkillDoc(t)
	if !strings.Contains(doc, "gc-runtime-nomad check") {
		t.Fatalf("%s does not point at `gc-runtime-nomad check` as the entry point", skillDoc)
	}
	// The op must not be in lifecycleOps: it deliberately needs no cluster
	// config, which is why it is safe as a first instrument.
	if lifecycleOps["check"] {
		t.Error("`check` is now a lifecycle op; the skill presents it as a configuration-free first step")
	}
}

// TestSkillHasFrontmatter keeps the file loadable as a skill at all: gc
// discovers <packDir>/skills/<name>/SKILL.md by convention, and the name and
// description are what a reader matches against before opening it.
func TestSkillHasFrontmatter(t *testing.T) {
	doc := readSkillDoc(t)
	if !strings.HasPrefix(doc, "---\n") {
		t.Fatalf("%s does not open with YAML frontmatter", skillDoc)
	}
	for _, key := range []string{"name: gc-nomad-debug", "description:"} {
		if !strings.Contains(doc, key) {
			t.Errorf("%s frontmatter is missing %q", skillDoc, key)
		}
	}
}

// TestSkillLeadsWithTheDarkLaneSymptom pins the ordering fix.
//
// An operator handed this skill and a staged fault reached the right answer in
// one command — but via a law in the preamble, not via the symptom table. The
// table's row for "a box that looks healthy and does nothing" sent them to
// identity-superset theory instead, and had no row at all for the symptom the
// pack's own docs name twice. A table that routes the commonest symptom to the
// wrong hypothesis is worse than a table with a gap, because it is confident.
func TestSkillLeadsWithTheDarkLaneSymptom(t *testing.T) {
	doc := readSkillDoc(t)
	drain := strings.Index(doc, "Seats drain `no_work`")
	if drain < 0 {
		t.Fatal("the symptom table has no row for seats draining no_work")
	}
	// It must come before the second-line hypothesis it replaces.
	if second := strings.Index(doc, "Second-line hypothesis only"); second < 0 {
		t.Error("the identity-superset row is not marked as a second-line hypothesis")
	} else if drain > second {
		t.Error("the second-line hypothesis is listed before the first-line symptom")
	}
}

// TestSkillSaysCheckCanFail: the skill described `check` as warning-only, so a
// reader who skimmed that line had been told its most decisive output does not
// exist. `check` exits nonzero on a capability gate.
func TestSkillSaysCheckCanFail(t *testing.T) {
	doc := readSkillDoc(t)
	for _, want := range []string{"not advisory", "FAIL"} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s does not say `check` can %q — that is its most decisive output", skillDoc, want)
		}
	}
}

// TestSkillSuppliesASafeEnvironmentProbe: the skill demanded an environment
// probe in one section and forbade echo/env/set/printenv in another, without
// ever giving the safe spelling. An operator had to invent it.
func TestSkillSuppliesASafeEnvironmentProbe(t *testing.T) {
	doc := readSkillDoc(t)
	if !strings.Contains(doc, "+x}") {
		t.Errorf("%s demands an environment probe but supplies no presence-only spelling", skillDoc)
	}
}

// TestSkillWarnsThatConformanceDisablesTheGate is a TWO-WAY guard, and it
// exists because the skill's own recommended first check was hiding the fault
// the skill is most often used to find.
//
// conformance.sh exports GC_NOMAD_SKIP_GC_CAPABILITY_GATE=1 by design: the
// ladder drives the RPP surface against a fake with no city and nothing to
// claim. The consequence is that running it on a deployment whose gc cannot
// claim a single bead goes GREEN. An operator following "hermetic, always
// first" got a clean pass on a dead lane.
//
// Both directions matter. If conformance.sh stops setting the flag the
// warning becomes false; if the skill drops the warning while the script
// still sets it, the trap is back.
func TestSkillWarnsThatConformanceDisablesTheGate(t *testing.T) {
	script, err := os.ReadFile("../conformance.sh")
	if err != nil {
		t.Fatalf("reading conformance.sh: %v", err)
	}
	const flag = "GC_NOMAD_SKIP_GC_CAPABILITY_GATE"
	sets := strings.Contains(string(script), "export "+flag+"=1")
	doc := readSkillDoc(t)
	warns := strings.Contains(doc, flag) && strings.Contains(doc, "NOT evidence")

	switch {
	case sets && !warns:
		t.Errorf("conformance.sh disables the capability gate and %s does not warn that a green\n"+
			"conformance run is not evidence the lane can claim — the skill's own recommended\n"+
			"check would hide the fault it exists to find", skillDoc)
	case !sets && warns:
		t.Errorf("%s warns that conformance.sh disables the capability gate, but it no longer does;\n"+
			"the warning is now false", skillDoc)
	}
}

// TestSkillPointsAtTheDurableDiagnostics: the pack writes its own account to a
// file precisely because gc discards its stderr, and an instrument nobody
// knows about is not an instrument. If the file or its directory is ever
// renamed, the skill stops pointing at anything.
func TestSkillPointsAtTheDurableDiagnostics(t *testing.T) {
	doc := readSkillDoc(t)
	if !strings.Contains(doc, diagnosticsFile) {
		t.Errorf("%s does not name %q, the file that holds the cause gc throws away",
			skillDoc, diagnosticsFile)
	}
	if !strings.Contains(doc, "GC_NOMAD_SIDECAR_DIR") {
		t.Errorf("%s names the diagnostics file but not the directory it lives in", skillDoc)
	}
}
