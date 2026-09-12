package main

import (
	"strings"
	"testing"
)

// A SECURITY REVIEWER'S FINDING, verbatim, from a reader who had only the
// shipped docs and refused to approve the pack for real credentials:
//
//	"No enforcement or even a lint warning ties a credential-shaped
//	 GC_NOMAD_EXTRA_SECRET_KEYS name to the `_PEM` suffix. The docs say plainly
//	 'the name you choose decides which one you get... it is not reversible
//	 after the fact', but [nothing checks it]."
//
// The documented contract is that a `_PEM`-suffixed name stages the value as a
// FILE and hands the agent a path, while any other name exports the value into
// the agent's process environment — where it is readable in
// /proc/<pid>/environ by the agent and everything it spawns. That is a real,
// irreversible difference decided by a string, and the pack was silent about
// it.
//
// Advisory, deliberately, NOT a refusal: the pack cannot know that
// MY_API_TOKEN is a credential rather than a feature flag named after one, and
// refusing on a name would break deployments over a guess. Naming the risk
// once, at the layer that knows the routing rule, is the honest amount of
// opinion to have.

func TestCredentialShapedNamesWithoutPEMAreNamed(t *testing.T) {
	var b strings.Builder
	warnCredentialShapedNames(&b, "sess-1", []string{"ANTHROPIC_API_KEY", "GH_TOKEN", "DB_PASSWORD"})
	out := b.String()
	if out == "" {
		t.Fatal("three credential-shaped keys staged into the process environment and the pack said nothing")
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "GH_TOKEN", "DB_PASSWORD"} {
		if !strings.Contains(out, k) {
			t.Errorf("the warning does not name %q, so the operator cannot tell which key to change", k)
		}
	}
	// It must name the remedy, which is a rename — and say why it cannot be
	// fixed later.
	if !strings.Contains(out, "_PEM") {
		t.Errorf("the warning does not name the _PEM suffix, which is the entire remedy: %q", out)
	}
	if !strings.Contains(strings.ToLower(out), "environ") {
		t.Errorf("the warning does not say where the value ends up, which is the reason to care: %q", out)
	}
}

// It must stay quiet for names already routed to a file, and for names that
// are not credential-shaped at all — or it becomes noise and stops being read,
// which is how the absent-secret warning nearly died.
func TestPEMAdviceStaysQuietWhenThereIsNothingToSay(t *testing.T) {
	for name, keys := range map[string][]string{
		"already a file":    {"ANTHROPIC_API_KEY_PEM", "GH_TOKEN_PEM"},
		"not credentials":   {"GC_MODEL", "GC_CONTEXT_WINDOW_TOKENS", "TZ"},
		"none declared":     {},
		"mixed but all pem": {"DB_PASSWORD_PEM"},
	} {
		var b strings.Builder
		warnCredentialShapedNames(&b, "sess-1", keys)
		if b.String() != "" {
			t.Errorf("%s: warned with nothing to warn about: %q", name, b.String())
		}
	}
}

// The vocabulary must match the one gc itself uses to decide what is
// sensitive, or the pack is teaching a different rule than the platform
// applies.
func TestCredentialShapeVocabularyMatchesTheDocumentedList(t *testing.T) {
	for _, k := range []string{
		"X_TOKEN", "X_SECRET", "X_PASSWORD", "X_CREDENTIAL",
		"X_OAUTH", "X_PRIVATE_KEY", "X_API_KEY", "X_ACCESS_KEY", "X_AUTH_JSON",
	} {
		if !credentialShapedName(k) {
			t.Errorf("%q is not recognised as credential-shaped; gc's own IsSensitiveKey drops this family from inherited env", k)
		}
	}
	// The narrowing, each one a name that would otherwise cry wolf.
	// GC_CONTEXT_WINDOW_TOKENS is not hypothetical: it is a real key in a real
	// city config, and this test caught it warning about a context-size
	// number. A credential is a TOKEN; a budget is TOKENS.
	for _, k := range []string{
		"GC_MODEL", "PATH", "HOME", "GC_REASONING_EFFORT",
		"GC_CONTEXT_WINDOW_TOKENS", "MAX_TOKENS", "GC_TOKEN_LIMIT",
	} {
		if credentialShapedName(k) {
			t.Errorf("%q is treated as credential-shaped and would produce noise", k)
		}
	}
}
