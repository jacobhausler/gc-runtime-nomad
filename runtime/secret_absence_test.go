package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestMergeExtraSecretEnvReportsDeclaredButAbsentKeys pins gate 4's
// missing-secrets case: an operator names a key in GC_NOMAD_EXTRA_SECRET_KEYS
// and the supervisor's environment does not carry it.
//
// That used to be `if !ok { continue }` — skipped with no error, no warning,
// no log. Staging succeeded, the box launched without a credential it was told
// to carry, and everything downstream looked healthy: the agent starts, the
// pane is fine, and the first operation needing that credential fails
// somewhere the operator reads as a provider or a network defect. The classic
// shape this runtime exists to stop producing.
func TestMergeExtraSecretEnvReportsDeclaredButAbsentKeys(t *testing.T) {
	t.Setenv("GC_DRILL_PRESENT_KEY", "value-never-printed")

	base := map[string]string{"ALREADY_STAGED": "from-cfg"}
	keys := []string{"GC_DRILL_PRESENT_KEY", "GC_DRILL_ABSENT_ONE", "GC_DRILL_ABSENT_TWO", "ALREADY_STAGED"}

	merged, extra, absent := mergeExtraSecretEnv(base, keys)

	if !extra["GC_DRILL_PRESENT_KEY"] || merged["GC_DRILL_PRESENT_KEY"] == "" {
		t.Error("a present key was not bridged from the supervisor environment")
	}
	// A key already supplied by the caller is not absent — the caller wins.
	if extra["ALREADY_STAGED"] {
		t.Error("a caller-supplied key was re-sourced from the process environment")
	}
	want := []string{"GC_DRILL_ABSENT_ONE", "GC_DRILL_ABSENT_TWO"}
	if strings.Join(absent, ",") != strings.Join(want, ",") {
		t.Errorf("absent = %v, want %v", absent, want)
	}
}

// TestAbsentSecretWarningIsValueBlind: this is the one function in the pack
// that holds secret values, so a diagnostic that leaked one would be a worse
// defect than the one it reports. It names keys and counts, never values.
func TestAbsentSecretWarningIsValueBlind(t *testing.T) {
	const secret = "sk-do-not-print-me"
	t.Setenv("GC_DRILL_PRESENT_KEY", secret)
	_, _, absent := mergeExtraSecretEnv(nil, []string{"GC_DRILL_PRESENT_KEY", "GC_DRILL_MISSING"})

	var out bytes.Buffer
	warnAbsentSecretKeys(&out, "sess-probe", absent)
	got := out.String()

	if !strings.Contains(got, "GC_DRILL_MISSING") {
		t.Errorf("the warning does not name the absent key:\n%s", got)
	}
	if strings.Contains(got, secret) {
		t.Fatalf("THE WARNING LEAKED A SECRET VALUE:\n%s", got)
	}
	if strings.Contains(got, "GC_DRILL_PRESENT_KEY") {
		t.Errorf("the warning names a key that WAS staged:\n%s", got)
	}

	// Nothing absent, nothing said.
	var quiet bytes.Buffer
	warnAbsentSecretKeys(&quiet, "sess-probe", nil)
	if quiet.Len() != 0 {
		t.Errorf("warned with no absent keys: %q", quiet.String())
	}
}

// TestAbsentSecretsAreReportedEvenWhenNothingStages is the "what would this
// look like if it had silently failed" check, applied to the fix above.
//
// The report first lived on the staging path. stage() runs only when
// stagesAnything() finds something to materialize, and a session whose ONLY
// declared secret keys are all absent materializes nothing — so the warning
// was silent in exactly the case it exists for. Measured: stagesAnything is
// false for an empty config with one declared-absent key.
func TestAbsentSecretsAreReportedEvenWhenNothingStages(t *testing.T) {
	old := extraSecretKeys
	defer func() { extraSecretKeys = old }()
	extraSecretKeys = []string{"GC_DRILL_NEVER_SET_ANYWHERE"}

	empty := stageConfig{}
	if stagesAnything(empty) {
		t.Skip("premise changed: an empty config now stages something, so this hole cannot occur")
	}

	// The reporting path must not depend on that predicate.
	_, _, absent := mergeExtraSecretEnv(empty.Env, extraSecretKeys)
	if len(absent) != 1 || absent[0] != "GC_DRILL_NEVER_SET_ANYWHERE" {
		t.Fatalf("absent = %v, want the one declared key even with nothing to stage", absent)
	}
}
