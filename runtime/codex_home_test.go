package main

import (
	"context"
	"strings"
	"testing"
)

// TestImageModeStagesCodexHomeOverridingProviderEnv is the RED/GREEN pair for
// cr-usnvaw. Proof runs 5-7 (ops/receipts/nrt-d3/2026-09-10-disposable-proof-
// 5-7-image-mode.md) killed every image-mode box at launch: the box inherited
// the orchestrator's CODEX_HOME (the estate's provider env pins it to
// /Users/home/.codex, city.toml:82), codex refused to start ("Error finding
// codex home"), its pane died, the tmux server went with it, and gc reported
// only "tmux exited 1". The image shape has no bootstrap to fix this (the
// pack's former launch-script seam is deleted) - so the pack stages the agent
// home itself: CODEX_HOME exported inside the mount the box reports, provider
// config rendered from the seat's staged env, auth from the staged secret.
func TestImageModeStagesCodexHomeOverridingProviderEnv(t *testing.T) {
	const authCanary = "CANARY-CODEX-AUTH-9f3c41-do-not-leak"

	l, _ := newTestLifecycle(t)
	l.agentImage = "localhost/gc-agent:test"
	withExtraSecretKeys(t, []string{codexAuthJSONKey})
	t.Setenv(codexAuthJSONKey, authCanary)

	ctx := context.Background()
	const session = "sess-image-codex-home"
	cfg := stageConfig{Env: map[string]string{
		// The provider env as the city stages it today: a Mac CODEX_HOME.
		codexHomeEnvName:          "/Users/home/.codex",
		codexModelProviderKey:     "haus_qwen38_rtxpro6000",
		codexModelProviderBaseURL: "http://192.168.0.122:18030/v1",
		codexModelKey:             "rtxpro6000-gateway/qwen38-flashnext-nvfp4",
		codexReasoningEffortKey:   "medium",
		codexCompactTokensKey:     "131072",
		"GC_CITY_URL":             "https://192.168.0.107:8443/api",
	}}

	// provision, not start: the defect is in staging, and this keeps the pane
	// (and tmux) out of the test.
	if err := l.opProvisionWithConfig(ctx, session, cfg); err != nil {
		t.Fatalf("image-mode provision: %v", err)
	}

	secretsDir, err := l.opExec2(ctx, session, `printf %s "$NOMAD_SECRETS_DIR"`)
	if err != nil {
		t.Fatalf("reading the box's secrets dir: %v", err)
	}

	envSh, err := l.opExec2(ctx, session, `cat "$NOMAD_SECRETS_DIR/env.sh"`)
	if err != nil {
		t.Fatalf("read env.sh: %v", err)
	}
	if strings.Contains(envSh, "/Users/home/.codex") {
		t.Fatalf("env.sh still carries the orchestrator's CODEX_HOME: %s", envSh)
	}
	if want := `export CODEX_HOME="$NOMAD_SECRETS_DIR/` + codexHomeDir + `"`; !strings.Contains(envSh, want) {
		t.Fatalf("env.sh missing the in-box CODEX_HOME export %q: %s", want, envSh)
	}
	if strings.Count(envSh, "export CODEX_HOME=") != 1 {
		t.Fatalf("env.sh exports CODEX_HOME more than once, so the winner depends on order: %s", envSh)
	}

	// The string alone is not the claim: sourced, it must resolve to a path
	// inside the mount this box actually has. That is what a single-quoted
	// "$NOMAD_SECRETS_DIR/codex-home" inherited through a provider env block
	// never did.
	sourced, err := l.opExec2(ctx, session, `. "$NOMAD_SECRETS_DIR/env.sh"; printf %s "$CODEX_HOME"`)
	if err != nil {
		t.Fatalf("sourcing env.sh: %v", err)
	}
	if want := secretsDir + "/" + codexHomeDir; sourced != want {
		t.Fatalf("sourced CODEX_HOME = %q, want the in-box path %q", sourced, want)
	}

	configToml, err := l.opExec2(ctx, session, `cat "`+secretsDir+`/`+codexHomeRelConfigPath+`"`)
	if err != nil {
		t.Fatalf("read staged codex config.toml: %v", err)
	}
	for _, want := range []string{
		`model = "rtxpro6000-gateway/qwen38-flashnext-nvfp4"`,
		`model_provider = "haus_qwen38_rtxpro6000"`,
		`model_reasoning_effort = "medium"`,
		"model_auto_compact_token_limit = 131072",
		"[model_providers.haus_qwen38_rtxpro6000]",
		`base_url = "http://192.168.0.122:18030/v1"`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(configToml, want) {
			t.Fatalf("staged config.toml missing %q:\n%s", want, configToml)
		}
	}
	if strings.Contains(configToml, authCanary) {
		t.Fatalf("auth bytes leaked into the rendered provider config:\n%s", configToml)
	}

	authJSON, err := l.opExec2(ctx, session, `cat "`+secretsDir+`/`+codexHomeRelAuthPath+`"`)
	if err != nil {
		t.Fatalf("read staged codex auth.json: %v", err)
	}
	if strings.TrimSpace(authJSON) != authCanary {
		t.Fatalf("staged auth.json = %q, want the staged secret", authJSON)
	}
}

// TestLegacyExecModeLeavesCodexHomeUnchanged protects the rollback shape: with
// no image the deployment bootstrap owns the agent home (staging_test.go's
// TestBuildLaunchCommandRunsConfiguredAgentScript is that seam's coverage), so
// env.sh bytes must be exactly what they were before cr-usnvaw - the inherited
// CODEX_HOME still exported verbatim, and no codex-home staged at all.
func TestLegacyExecModeLeavesCodexHomeUnchanged(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-legacy-codex-home"

	cfg := stageConfig{Env: map[string]string{
		codexHomeEnvName:          "/Users/home/.codex",
		codexModelProviderKey:     "haus_qwen38_rtxpro6000",
		codexModelProviderBaseURL: "http://192.168.0.122:18030/v1",
		"GC_SESSION":              "legacy-lane",
	}}
	if err := l.opProvisionWithConfig(ctx, session, cfg); err != nil {
		t.Fatalf("legacy-mode provision: %v", err)
	}

	envSh, err := l.opExec2(ctx, session, `cat "$NOMAD_SECRETS_DIR/env.sh"`)
	if err != nil {
		t.Fatalf("read env.sh: %v", err)
	}
	if !strings.Contains(envSh, `export CODEX_HOME='/Users/home/.codex'`) {
		t.Fatalf("legacy env.sh no longer passes the deployment's CODEX_HOME through unchanged: %s", envSh)
	}
	if strings.Contains(envSh, codexHomeDir) {
		t.Fatalf("legacy env.sh stages a pack-owned agent home the bootstrap already owns: %s", envSh)
	}

	present, err := l.opExec2(ctx, session, `[ -e "$NOMAD_SECRETS_DIR/`+codexHomeDir+`" ] && echo present || echo absent`)
	if err != nil {
		t.Fatalf("checking for a staged codex home: %v", err)
	}
	if strings.TrimSpace(present) != "absent" {
		t.Fatalf("legacy exec mode staged a codex home (%s); the launch script owns that shape", present)
	}
}

// TestCodexHomeOwnedOnlyWhenTheBoxHasNoHome covers the stand-down rule that
// keeps this from overriding a deployment that already points inside the
// secrets mount - the docker shape's mount point is resolved from the box, so
// it cannot be written into a test as a constant.
func TestCodexHomeOwnedOnlyWhenTheBoxHasNoHome(t *testing.T) {
	for _, tc := range []struct {
		name       string
		secretsDir string
		env        map[string]string
		want       bool
	}{
		{"legacy exec shape is the bootstrap's", "", map[string]string{codexHomeEnvName: "/Users/home/.codex"}, false},
		{"image with no CODEX_HOME at all", "/secrets", map[string]string{}, true},
		{"image inheriting an orchestrator path", "/secrets", map[string]string{codexHomeEnvName: "/Users/home/.codex"}, true},
		{"image already inside the mount", "/secrets", map[string]string{codexHomeEnvName: "/secrets/codex-home"}, false},
		{"image pointing at the mount itself", "/secrets", map[string]string{codexHomeEnvName: "/secrets"}, false},
		{"image with a same-prefix sibling outside the mount", "/secrets", map[string]string{codexHomeEnvName: "/secrets-backup/codex-home"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexHomeOwned(tc.secretsDir, tc.env); got != tc.want {
				t.Fatalf("codexHomeOwned(%q, %v) = %v, want %v", tc.secretsDir, tc.env, got, tc.want)
			}
		})
	}
}

// TestRenderCodexConfigNoOpAndConfigErrors: an unnamed provider is the
// ordinary no-op every other staging field performs (a baked image home is left
// alone); a named-but-malformed one is a config error that stops the launch,
// and the error names the key rather than the value it rejected.
func TestRenderCodexConfigNoOpAndConfigErrors(t *testing.T) {
	if got, err := renderCodexConfig(map[string]string{"GC_SESSION": "plain"}); err != nil || got != nil {
		t.Fatalf("renderCodexConfig with no provider named = (%q, %v), want (nil, nil)", got, err)
	}

	const badName = `haus"; drop = "x`
	got, err := renderCodexConfig(map[string]string{
		codexModelProviderKey:     badName,
		codexModelProviderBaseURL: "http://192.168.0.122:18030/v1",
	})
	if err == nil || got != nil {
		t.Fatalf("malformed provider name accepted: (%q, %v)", got, err)
	}
	if !strings.Contains(err.Error(), codexModelProviderKey) {
		t.Fatalf("error does not name the key at fault (%s): %v", codexModelProviderKey, err)
	}

	if _, err := renderCodexConfig(map[string]string{
		codexModelProviderKey:     "haus_qwen38",
		codexModelProviderBaseURL: "192.168.0.122:18030/v1",
	}); err == nil {
		t.Fatal("a base URL with no scheme accepted; it must be a config error")
	}

	// A non-numeric window is dropped rather than rendered as invalid TOML.
	got, err = renderCodexConfig(map[string]string{
		codexModelProviderKey:     "haus_qwen38",
		codexModelProviderBaseURL: "http://192.168.0.122:18030/v1",
		codexCompactTokensKey:     "131072 tokens",
	})
	if err != nil {
		t.Fatalf("a non-numeric context window must drop, not fail: %v", err)
	}
	if strings.Contains(string(got), "model_auto_compact_token_limit") {
		t.Fatalf("non-numeric context window rendered into TOML:\n%s", got)
	}
}
