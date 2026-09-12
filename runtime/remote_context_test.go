package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestExtraSecretPEMKeyGetsPointerNotContentInEnvScript is the RED/GREEN
// pair for the staging half: attempt 8 (pack 81c159d) proved the
// prime typing fix live, but the box had no path to the city — no mTLS
// identity staged at all. gc's remote client reads CA/client cert/client key
// as FILE PATHS from GC_CITY_CA/GC_CITY_CLIENT_CERT/GC_CITY_CLIENT_KEY
// (cmd/gc/remote_client.go:92-93), but this pack's existing
// GC_NOMAD_EXTRA_SECRET_KEYS bridge only ever staged a key's own file and
// (for a valid shell identifier) its raw VALUE into env.sh — never a pointer
// another env var name could read.
//
// The generic rule: an extra secret key FOO_PEM still gets its file staged
// exactly as before, but env.sh exports FOO="$NOMAD_SECRETS_DIR/FOO_PEM"
// instead of ever repeating the PEM bytes as a second value in env.sh.
func TestExtraSecretPEMKeyGetsPointerNotContentInEnvScript(t *testing.T) {
	const caPEM = "-----BEGIN CERTIFICATE-----\nfake-ca-bytes\n-----END CERTIFICATE-----"
	t.Setenv("GC_CITY_CA_PEM", caPEM)
	withExtraSecretKeys(t, []string{"GC_CITY_CA_PEM"})

	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-pem-pointer"

	cfg := stageConfig{Env: map[string]string{"GC_SESSION": "safe-value"}}
	if err := l.opStartWithConfig(ctx, session, withPane(cfg)); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The file itself is unchanged: still staged, still readable at its own
	// name under NOMAD_SECRETS_DIR.
	exitCode, out, err := l.opExec(ctx, session, []string{"/bin/sh", "-c", `cat "$NOMAD_SECRETS_DIR/GC_CITY_CA_PEM"`})
	if err != nil || exitCode != 0 || strings.TrimSpace(string(out)) != caPEM {
		t.Fatalf("staged GC_CITY_CA_PEM file = (%d, %q, %v), want the PEM bytes", exitCode, out, err)
	}

	envSh, err := l.opExec2(ctx, session, `cat "$NOMAD_SECRETS_DIR/env.sh"`)
	if err != nil {
		t.Fatalf("read env.sh: %v", err)
	}
	if strings.Contains(envSh, caPEM) || strings.Contains(envSh, "fake-ca-bytes") {
		t.Fatalf("env.sh repeats the PEM bytes as a value, not just a pointer: %s", envSh)
	}
	if !strings.Contains(envSh, `export GC_CITY_CA="$NOMAD_SECRETS_DIR/GC_CITY_CA_PEM"`) {
		t.Fatalf("env.sh missing the GC_CITY_CA pointer export: %s", envSh)
	}
}

// TestRemoteContextStagedWhenCityNameAndURLPresent is the RED/GREEN pair for
// the context half: with GC_CITY_NAME and GC_CITY_URL on the
// wire (city.toml provider env, same channel GC_CITY_URL already rides) and
// the three mTLS *_PEM keys bridged in, the pack stages a GC_HOME whose
// contexts.toml gc's OWN resolver reads with no flags at all
// (cmd/gc/remote_target.go:104-106,179-181: $GC_HOME/contexts.toml).
func TestRemoteContextStagedWhenCityNameAndURLPresent(t *testing.T) {
	t.Setenv("GC_CITY_CA_PEM", "ca-bytes")
	t.Setenv("GC_CITY_CLIENT_CERT_PEM", "cert-bytes")
	t.Setenv("GC_CITY_CLIENT_KEY_PEM", "key-bytes")
	withExtraSecretKeys(t, []string{"GC_CITY_CA_PEM", "GC_CITY_CLIENT_CERT_PEM", "GC_CITY_CLIENT_KEY_PEM"})

	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-remote-context"

	cfg := stageConfig{Env: map[string]string{
		"GC_CITY_NAME": "nomad-canary",
		"GC_CITY_URL":  "https://192.168.0.107:8443",
	}}
	if err := l.opStartWithConfig(ctx, session, withPane(cfg)); err != nil {
		t.Fatalf("start: %v", err)
	}

	envSh, err := l.opExec2(ctx, session, `cat "$NOMAD_SECRETS_DIR/env.sh"`)
	if err != nil {
		t.Fatalf("read env.sh: %v", err)
	}
	for _, want := range []string{
		`export GC_HOME="$NOMAD_SECRETS_DIR/gc-home"`,
		`export GC_CITY_CONTEXT='nomad-canary'`,
	} {
		if !strings.Contains(envSh, want) {
			t.Fatalf("env.sh missing %q: %s", want, envSh)
		}
	}
	if strings.Contains(envSh, "export GC_CITY_URL=") {
		t.Fatalf("env.sh exports GC_CITY_URL after the staged context took ownership of the URL: %s", envSh)
	}

	contextsToml, err := l.opExec2(ctx, session, `cat "$NOMAD_SECRETS_DIR/gc-home/contexts.toml"`)
	if err != nil {
		t.Fatalf("read contexts.toml: %v", err)
	}
	for _, want := range []string{
		"[[context]]",
		`name = "nomad-canary"`,
		`url = "https://192.168.0.107:8443"`,
		`ca_file = "/secrets/GC_CITY_CA_PEM"`,
		`client_cert_file = "/secrets/GC_CITY_CLIENT_CERT_PEM"`,
		`client_key_file = "/secrets/GC_CITY_CLIENT_KEY_PEM"`,
	} {
		if !strings.Contains(contextsToml, want) {
			t.Fatalf("contexts.toml missing %q:\n%s", want, contextsToml)
		}
	}
	for _, forbidden := range []string{"ca-bytes", "cert-bytes", "key-bytes"} {
		if strings.Contains(contextsToml, forbidden) || strings.Contains(envSh, forbidden) {
			t.Fatalf("PEM bytes %q leaked into contexts.toml or env.sh", forbidden)
		}
	}
}

// TestRemoteContextIsNoopWithoutCityNameAndURL: a deployment that never sets
// GC_CITY_NAME (or GC_CITY_URL) gets no GC_HOME, no contexts.toml, and no
// env.sh change at all - the same rollback invariant every other staging
// field on this pack already holds.
func TestRemoteContextIsNoopWithoutCityNameAndURL(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-remote-context-unowed"

	cfg := stageConfig{Env: map[string]string{"GC_CITY_URL": "https://192.168.0.107:8443"}}
	if err := l.opStartWithConfig(ctx, session, withPane(cfg)); err != nil {
		t.Fatalf("start: %v", err)
	}

	exitCode, out, err := l.opExec(ctx, session, []string{"/bin/sh", "-c",
		`[ -e "$NOMAD_SECRETS_DIR/gc-home" ] && echo present || echo absent`})
	if err != nil || exitCode != 0 {
		t.Fatalf("probe gc-home: (%d, %q, %v)", exitCode, out, err)
	}
	if strings.TrimSpace(string(out)) != "absent" {
		t.Fatalf("gc-home was staged without GC_CITY_NAME: %s", out)
	}

	envSh, err := l.opExec2(ctx, session, `cat "$NOMAD_SECRETS_DIR/env.sh"`)
	if err != nil {
		t.Fatalf("read env.sh: %v", err)
	}
	if strings.Contains(envSh, "GC_HOME") || strings.Contains(envSh, "GC_CITY_CONTEXT") {
		t.Fatalf("env.sh carries GC_HOME/GC_CITY_CONTEXT though no context was staged: %s", envSh)
	}
}

// opExec2 is a small convenience wrapper: opExec returns []byte, and every
// caller in this file wants the trimmed string.
func (l *lifecycle) opExec2(ctx context.Context, session, script string) (string, error) {
	exitCode, out, err := l.opExec(ctx, session, []string{"/bin/sh", "-c", script})
	if err != nil {
		return "", err
	}
	if exitCode != 0 {
		return "", fmt.Errorf("exec %q exited %d: %s", script, exitCode, out)
	}
	return string(out), nil
}
