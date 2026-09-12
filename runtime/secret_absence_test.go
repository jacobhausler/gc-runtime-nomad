package main

import (
	"bytes"
	"errors"
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

// TestForbiddenHintSeparatesNoTokenFromRefusedToken is gate 4's expired-token
// case: the layer that knows naming which of two very different faults it is.
//
// Nomad answers "Permission denied" whether no token was sent or a token was
// sent and rejected. Those need opposite responses — one is configuration, the
// other a live credential that aged out — and the bare answer sends an
// operator to edit ACL policy either way. This estate has the receipt for the
// second: a seat token that worked at T and 403'd by T+15min.
func TestForbiddenHintSeparatesNoTokenFromRefusedToken(t *testing.T) {
	none := forbiddenHint(false)
	sent := forbiddenHint(true)
	if none == sent {
		t.Fatal("both 403 causes produce the same hint; the distinction is the whole point")
	}
	if !strings.Contains(none, "GC_NOMAD_TOKEN") {
		t.Errorf("the no-token hint does not name the variable that supplies one: %q", none)
	}
	for _, want := range []string{"expired", "revoked", "check the token before the policy"} {
		if !strings.Contains(sent, want) {
			t.Errorf("the refused-token hint does not mention %q: %q", want, sent)
		}
	}
}

// TestStatusErrorCarriesTheForbiddenHint pins that the hint reaches the error
// an operator actually reads, and that other statuses are untouched.
func TestStatusErrorCarriesTheForbiddenHint(t *testing.T) {
	err := statusError(403, "/v1/jobs", []byte(`{"error":"Permission denied"}`), true)
	if !errors.Is(err, errJobForbidden) {
		t.Fatalf("403 no longer maps to errJobForbidden: %v", err)
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("the 403 error does not carry the hint: %v", err)
	}
	gone := statusError(404, "/v1/jobs", nil, true)
	if !errors.Is(gone, errJobGone) || strings.Contains(gone.Error(), "expired") {
		t.Errorf("a 404 was altered by the 403 hint: %v", gone)
	}
}
