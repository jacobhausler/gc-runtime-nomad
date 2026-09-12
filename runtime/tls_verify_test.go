package main

import (
	"crypto/tls"
	"strings"
	"testing"
)

// TestTLSVerificationDisabledIsNamed pins that a client whose transport skips
// certificate verification says so, and names the env var responsible.
//
// This was silent. The pack aliases four TLS knobs — CACERT, CLIENT_CERT,
// CLIENT_KEY, TLS_SERVER_NAME — and not the fifth. NOMAD_SKIP_VERIFY is read
// by the SDK's own DefaultConfig (api.go:383), lands on TLSConfig.Insecure,
// and reaches http.Transport.TLSClientConfig.InsecureSkipVerify (api.go:490).
// The pack then CLONES that config into c.tls (client.go:110,133), which is
// the config the raw alloc-exec WebSocket dialer uses (client.go:818) — the
// channel every staged secret travels over.
//
// So one stray NOMAD_SKIP_VERIFY=1 in the supervisor environment turned off
// verification for the API and for the secret channel, with no log line, no
// warning, and nothing in 1,228 lines of documentation. Found 2026-09-12 by
// an outside adoption review.
func TestTLSVerificationDisabledIsNamed(t *testing.T) {
	if got := tlsVerificationWarning(&tls.Config{InsecureSkipVerify: true}); got == "" {
		t.Fatal("a transport with InsecureSkipVerify set produced no warning")
	} else {
		if !strings.Contains(got, "NOMAD_SKIP_VERIFY") {
			t.Errorf("warning does not name the variable responsible: %q", got)
		}
		if !strings.Contains(got, "alloc-exec") {
			t.Errorf("warning does not say the secret channel is affected: %q", got)
		}
	}
}

func TestTLSVerificationEnabledIsQuiet(t *testing.T) {
	if got := tlsVerificationWarning(&tls.Config{}); got != "" {
		t.Errorf("a verifying transport warned anyway: %q", got)
	}
	if got := tlsVerificationWarning(nil); got != "" {
		t.Errorf("a nil TLS config (plain http) warned anyway: %q", got)
	}
}

// TestCheckNamesDisabledTLSVerification pins that the pre-flight diagnostic —
// the one COMPATIBILITY.md tells people to attach to a bug report — reports a
// disabled-verification environment. `check` builds no client, so without
// this it is the one door that could stay quiet about it.
func TestCheckNamesDisabledTLSVerification(t *testing.T) {
	t.Setenv("NOMAD_SKIP_VERIFY", "1")
	if !skipVerifyRequested() {
		t.Fatal("skipVerifyRequested() = false with NOMAD_SKIP_VERIFY=1")
	}
	for _, off := range []string{"", "0", "false", "not-a-bool"} {
		t.Setenv("NOMAD_SKIP_VERIFY", off)
		if skipVerifyRequested() {
			t.Errorf("skipVerifyRequested() = true for NOMAD_SKIP_VERIFY=%q", off)
		}
	}
}
