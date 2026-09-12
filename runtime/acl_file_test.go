package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gc drops any inherited env key CONTAINING "TOKEN" before handing the
// environment to a child process (internal/execenv.IsSensitiveKey). That is
// correct of gc and fatal for this pack: GC_NOMAD_TOKEN cannot reach this
// binary by inheritance, because its own NAME matches the filter, and
// PackRuntimeEntry has no env map to carry it explicitly either.
//
// Measured on a live deployment: GC_NOMAD_AGENT_IMAGE, exported by the same
// wrapper in the same way, arrives fine; GC_NOMAD_TOKEN does not. The only
// difference is the name.
//
// So the token travels as a PATH. These tests pin the two properties that make
// that work: the variable's name must survive the filter, and a named-but-bad
// file must be REFUSED rather than silently yielding no token.

// sensitiveMarkers mirrors gc's own list. Kept here deliberately rather than
// imported: this pack does not depend on gc's source tree, and the whole point
// is that the name must not match.
var sensitiveMarkers = []string{
	"PASSWORD", "TOKEN", "SECRET", "PRIVATE_KEY", "PRIVATE-KEY",
	"API_KEY", "API-KEY", "ACCESS_KEY", "ACCESS-KEY", "CREDENTIAL",
	"OAUTH", "AUTH_JSON", "AUTH-JSON",
}

// TestACLFileVarSurvivesTheSensitiveFilter is the entire reason the variable is
// not called GC_NOMAD_TOKEN_FILE. That name contains "TOKEN" and would be
// dropped exactly like the value it was meant to replace.
func TestACLFileVarSurvivesTheSensitiveFilter(t *testing.T) {
	up := strings.ToUpper(envACLFile)
	for _, m := range sensitiveMarkers {
		if strings.Contains(up, m) {
			t.Errorf("%s contains %q, so gc's inherited-env filter will DROP it and the "+
				"path will never arrive — which is the same failure this variable exists to fix. "+
				"Rename it to something carrying none of gc's sensitivity markers.", envACLFile, m)
		}
	}
	// And the thing it replaces must still match, or the premise is wrong.
	if !strings.Contains(strings.ToUpper(envToken), "TOKEN") {
		t.Errorf("%s no longer contains TOKEN; re-check whether the file source is still needed", envToken)
	}
}

func TestResolveNomadTokenPrefersTheDirectValue(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "acl")
	if err := os.WriteFile(f, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	t.Setenv(envToken, "direct")
	t.Setenv(envACLFile, f)
	got, err := resolveNomadToken()
	if err != nil {
		t.Fatalf("resolveNomadToken: %v", err)
	}
	if got != "direct" {
		t.Errorf("got %q, want the direct value to win so no existing deployment changes behaviour", got)
	}
}

func TestResolveNomadTokenReadsTheFileAndTrims(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "acl")
	// A trailing newline is what every ordinary way of writing this file
	// produces, and an untrimmed token is rejected by Nomad as malformed.
	if err := os.WriteFile(f, []byte("  abc-123\n"), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	t.Setenv(envToken, "")
	t.Setenv(envACLFile, f)
	got, err := resolveNomadToken()
	if err != nil {
		t.Fatalf("resolveNomadToken: %v", err)
	}
	if got != "abc-123" {
		t.Errorf("got %q, want the trimmed file contents", got)
	}
}

// TestResolveNomadTokenRefusesAnUnusableFile is the fail-closed half. Falling
// through to "no token" is precisely how a deployment ends up unauthenticated
// while believing it is configured — and unauthenticated means every session
// reports healthy while claiming nothing.
func TestResolveNomadTokenRefusesAnUnusableFile(t *testing.T) {
	t.Setenv(envToken, "")

	t.Run("missing", func(t *testing.T) {
		t.Setenv(envACLFile, filepath.Join(t.TempDir(), "nope"))
		got, err := resolveNomadToken()
		if err == nil {
			t.Fatalf("a missing token file returned %q and no error; it must refuse", got)
		}
		if !strings.Contains(err.Error(), envACLFile) {
			t.Errorf("the error does not name %s: %v", envACLFile, err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "acl")
		if err := os.WriteFile(f, []byte("   \n"), 0o600); err != nil {
			t.Fatalf("writing: %v", err)
		}
		t.Setenv(envACLFile, f)
		if _, err := resolveNomadToken(); err == nil {
			t.Fatal("an empty token file must be refused, not treated as no token")
		}
	})
}

// TestResolveNomadTokenIsAbsentWhenNeitherIsSet keeps ACL-less clusters working:
// no token configured at all is not an error, it is a cluster without ACLs.
func TestResolveNomadTokenIsAbsentWhenNeitherIsSet(t *testing.T) {
	t.Setenv(envToken, "")
	t.Setenv(envACLFile, "")
	got, err := resolveNomadToken()
	if err != nil || got != "" {
		t.Errorf("got (%q, %v), want empty and no error — an ACL-less cluster needs no token", got, err)
	}
}
