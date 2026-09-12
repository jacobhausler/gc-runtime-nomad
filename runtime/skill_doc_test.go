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
