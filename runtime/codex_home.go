package main

// Image-mode Codex home (cr-usnvaw).
//
// Why this file exists. A box launched from GC_NOMAD_AGENT_IMAGE takes its
// agent toolchain from the image, and the pack's deployment-bootstrap seam
// (GC_NOMAD_AGENT_LAUNCH_SCRIPT) is deliberately not consulted in that shape:
// the image owns the toolchain and image mode mounts no artifact volume for a
// bootstrap to live on (main.go's envAgentImage / envAgentLaunchScript
// contract, welded in cr-hcgaxt). The legacy exec shape got its agent HOME,
// provider config and auth file from exactly that bootstrap
// (ops/nomad/agent-launch.sh: export CODEX_HOME="$NOMAD_SECRETS_DIR/codex-home",
// install auth.json, write config.toml). The native image shape had no
// equivalent, so the box inherited the orchestrator's CODEX_HOME — the city
// pins it to a Mac path in its provider env (city.toml:82) — and Codex died at
// launch: "Error finding codex home: CODEX_HOME points to
// /Users/home/.codex, but that path does not exist". The pane's command died,
// its tmux server exited with it, and gc reported only "tmux exited 1" (proof
// runs 5-7, ops/receipts/nrt-d3/2026-09-10-disposable-proof-5-7-image-mode.md).
//
// What it does, and why this shape. stage() already owns the only channel that
// reaches the box: the staged secret files plus the sourceable env.sh that
// paneStagedEnvPrelude sources before the agent runs. So the Codex home is
// staged the same way the gc remote-context bridge (cr-gdeav.6.15) stages its
// GC_HOME: render the file under $NOMAD_SECRETS_DIR, then export the variable
// that names it dynamically, so the box's own Nomad-provided mount point is
// what lands in the environment. CODEX_HOME is exported after every staged
// provider value — and the inherited value is removed from env.sh rather than
// merely shadowed, so the orchestrator's path never appears in the box at all.
//
// Native forms checked before adding this (and why they do not suffice):
//
//   - GC_NOMAD_AGENT_LAUNCH_SCRIPT: the pack's own seam, and the code that
//     today carries this recipe. Ignored once GC_NOMAD_AGENT_IMAGE is set
//     (main.go:56) — there is no bootstrap volume in the image shape.
//   - gc build-image baking a Codex home into the image: gc's toolchain, not
//     this pack, and the owner ruled this item pack-only ("no launcher, no
//     per-seat writer").
//   - gc's provider base_url_env (internal/worker/builtin/profiles.go:202):
//     binds gc's OWN harness env on the orchestrator; it never crosses into the
//     allocation, and gc renders model/provider as command flags only.
//   - jobspec.go's sessionSecretsMB: reserves the secrets tmpfs a Codex home
//     lives on (its own comment says so) but writes nothing into it.
//
// What stays deployment-owned. The pack invents no lane: the provider stanza is
// rendered from keys the seat's provider env block stages
// (GC_CODEX_MODEL_PROVIDER + GC_CODEX_MODEL_PROVIDER_BASE_URL, optionally
// GC_CODEX_WIRE_API), which is the one per-seat channel gc has. No credential
// value is interpolated into env.sh, argv, or the job spec, and a seat that
// names no provider renders no config — it gets only the CODEX_HOME override,
// which is the one thing that cannot be left to the host.

import (
	"fmt"
	"strings"
)

const (
	// codexHomeDir is the in-box Codex home, under $NOMAD_SECRETS_DIR. It is
	// the same directory name the legacy bootstrap used, so a deployment that
	// still stages ops/nomad/agent-launch.sh points at one Codex home rather
	// than two. A subdirectory (not the secrets root) keeps the agent's files
	// out of the per-key secret-file namespace staged beside it — the same
	// reason the gc context bridge uses gc-home/ (remoteContextDir).
	codexHomeDir = "codex-home"

	// These paths are relative to $NOMAD_SECRETS_DIR, like every other
	// stageFile this pack emits.
	codexHomeRelConfigPath = codexHomeDir + "/config.toml"
	codexHomeRelAuthPath   = codexHomeDir + "/auth.json"

	// codexHomeEnvName is the agent's HOME variable. It is named here as data:
	// the value is always the mount point the box reported, never a path this
	// pack guesses, and never the orchestrator's.
	codexHomeEnvName = "CODEX_HOME"

	// codexAuthJSONKey is the staged secret holding the agent's auth JSON. It
	// arrives through GC_NOMAD_EXTRA_SECRET_KEYS (staging.go's
	// mergeExtraSecretEnv), the same key the legacy bootstrap installed.
	codexAuthJSONKey = "CODEX_AUTH_JSON"

	// The per-seat keys that name the lane. Provider env is the only channel
	// that reaches the box per seat, so the seat declares its own provider
	// here; unset means the pack renders no provider stanza at all.
	codexModelProviderKey     = "GC_CODEX_MODEL_PROVIDER"
	codexModelProviderBaseURL = "GC_CODEX_MODEL_PROVIDER_BASE_URL"
	codexWireAPIKey           = "GC_CODEX_WIRE_API"
	codexModelKey             = "GC_MODEL"
	codexReasoningEffortKey   = "GC_REASONING_EFFORT"
	codexCompactTokensKey     = "GC_CONTEXT_WINDOW_TOKENS"

	// codexDefaultWireAPI and codexStreamIdleTimeoutMS are the launcher's own
	// values, carried verbatim from ops/nomad/agent-launch.sh so an image-mode
	// seat and a bootstrap-launched seat on the same lane behave identically.
	codexDefaultWireAPI      = "responses"
	codexStreamIdleTimeoutMS = 1800000
)

// codexHomeOwned reports whether this pack must own the box's Codex home.
//
// Only the image shape is in scope: the legacy exec shape already gets its
// agent home from the deployment bootstrap, and its env.sh bytes must stay
// byte-identical (that shape is this seam's rollback). Within image mode the
// pack stands down when the staged CODEX_HOME already points inside the mount
// the box reported — that deployment has an in-box home and needs no override.
// An empty or foreign path (the estate's Mac path, and every lane today) is
// the broken shape this fixes.
func codexHomeOwned(secretsDir string, secretEnv map[string]string) bool {
	if strings.TrimSpace(secretsDir) == "" {
		return false
	}
	staged := strings.TrimSpace(secretEnv[codexHomeEnvName])
	if staged == "" {
		return true
	}
	return staged != secretsDir && !strings.HasPrefix(staged, secretsDir+"/")
}

// codexHomeEnvScriptInput drops the inherited CODEX_HOME from the env.sh
// input, so the orchestrator's path is not merely shadowed but absent — the
// same treatment remoteContextEnvScriptInput gives GC_CITY_URL once the staged
// context owns it. The value keeps its ordinary per-key secret file, which is
// what the pack's own diagnostics read.
func codexHomeEnvScriptInput(env map[string]string) map[string]string {
	if _, ok := env[codexHomeEnvName]; !ok {
		return env
	}
	filtered := make(map[string]string, len(env))
	for key, value := range env {
		if key != codexHomeEnvName {
			filtered[key] = value
		}
	}
	return filtered
}

// buildCodexHomeEnvLines returns the env.sh lines that point the agent at the
// home this pack stages. CODEX_HOME is exported in double quotes, not
// shellQuote's single quotes, so $NOMAD_SECRETS_DIR expands at source time to
// the mount the box actually has — the same reasoning as the *_PEM pointer
// exports and the GC_HOME line in buildRemoteContextEnvLines. A literal path
// here would assume Nomad's mount point instead of reading it, which is what
// made the legacy recipe's single-quoted "$NOMAD_SECRETS_DIR/codex-home"
// unusable the moment a provider env block carried it.
func buildCodexHomeEnvLines() []byte {
	return []byte("export " + codexHomeEnvName + `="$NOMAD_SECRETS_DIR/` + codexHomeDir + "\"\n")
}

// buildCodexHomeFiles returns the agent-home files to stage for secretEnv, in
// addition to the per-key secret files stage() already emits. A malformed
// provider declaration is a config error and stops the op — a broken TOML the
// agent silently falls back from is worse than a launch that names which key
// was wrong — while an absent declaration is the ordinary no-op every other
// staging field here already performs.
//
// Secret bytes never enter the rendered config: the auth file is staged from
// its own already-staged value, the same copy the legacy bootstrap installed
// with install -m 600, and the config names only a provider, a URL, and the
// seat's model.
func buildCodexHomeFiles(secretEnv map[string]string) ([]stageFile, error) {
	var files []stageFile

	config, err := renderCodexConfig(secretEnv)
	if err != nil {
		return nil, err
	}
	if len(config) > 0 {
		files = append(files, stageFile{Path: codexHomeRelConfigPath, Content: config, Mode: 0o600})
	}
	if auth, ok := secretEnv[codexAuthJSONKey]; ok && strings.TrimSpace(auth) != "" {
		files = append(files, stageFile{Path: codexHomeRelAuthPath, Content: []byte(auth), Mode: 0o600})
	}
	return files, nil
}

// renderCodexConfig renders the Codex provider config for a seat from its
// staged environment, or nothing at all when the seat names no provider (a
// baked image home, or a lane that needs none, is left alone).
//
// Anything gc renders as a command flag is deliberately NOT written here: the
// image-mode pane runs gc's rendered command, and its -c flags override this
// file, so writing them would only create a second, staler copy.
func renderCodexConfig(secretEnv map[string]string) ([]byte, error) {
	provider := strings.TrimSpace(secretEnv[codexModelProviderKey])
	baseURL := strings.TrimSpace(secretEnv[codexModelProviderBaseURL])
	if provider == "" && baseURL == "" {
		return nil, nil
	}
	if !validCodexProviderName(provider) {
		return nil, fmt.Errorf("codex provider config: %s is not a plain provider name ([A-Za-z0-9._-] only)", codexModelProviderKey)
	}
	if !validCodexBaseURL(baseURL) {
		return nil, fmt.Errorf("codex provider config: %s is not a single http(s):// URL", codexModelProviderBaseURL)
	}
	wireAPI := strings.TrimSpace(secretEnv[codexWireAPIKey])
	if wireAPI == "" {
		wireAPI = codexDefaultWireAPI
	}
	if !validCodexProviderName(wireAPI) {
		return nil, fmt.Errorf("codex provider config: %s is not a plain wire API name", codexWireAPIKey)
	}

	var b strings.Builder
	if model := strings.TrimSpace(secretEnv[codexModelKey]); model != "" {
		fmt.Fprintf(&b, "model = %s\n", tomlQuote(model))
	}
	fmt.Fprintf(&b, "model_provider = %s\n", tomlQuote(provider))
	if effort := strings.TrimSpace(secretEnv[codexReasoningEffortKey]); effort != "" {
		fmt.Fprintf(&b, "model_reasoning_effort = %s\n", tomlQuote(effort))
	}
	// gc pairs GC_CONTEXT_WINDOW_TOKENS 1:1 with this key (the estate's
	// provider env blocks carry the same number in both); anything that is not
	// a plain integer is omitted, so the agent keeps its own default rather
	// than a value that is not a number.
	if compact := strings.TrimSpace(secretEnv[codexCompactTokensKey]); allDigits(compact) {
		fmt.Fprintf(&b, "model_auto_compact_token_limit = %s\n", compact)
	}
	b.WriteString("approval_policy = \"never\"\n")
	b.WriteString("sandbox_mode = \"danger-full-access\"\n")
	b.WriteByte('\n')
	fmt.Fprintf(&b, "[model_providers.%s]\n", provider)
	fmt.Fprintf(&b, "name = %s\n", tomlQuote(provider))
	fmt.Fprintf(&b, "base_url = %s\n", tomlQuote(baseURL))
	fmt.Fprintf(&b, "wire_api = %s\n", tomlQuote(wireAPI))
	b.WriteString("requires_openai_auth = false\n")
	b.WriteString("request_max_retries = 0\n")
	b.WriteString("stream_max_retries = 0\n")
	fmt.Fprintf(&b, "stream_idle_timeout_ms = %d\n", codexStreamIdleTimeoutMS)
	return []byte(b.String()), nil
}

// validCodexProviderName accepts the portable bare-key form of a TOML table
// name. The provider name is rendered into a table header, so a name that is
// not a plain token would turn deployment configuration into TOML syntax.
func validCodexProviderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// validCodexBaseURL accepts an http(s) URL with no whitespace or quoting
// characters. It is a URL, not a credential, and it is TOML-quoted regardless
// — the check exists so a truncated or multi-line value is rejected as the
// config error it is instead of rendering a file the agent mis-parses.
func validCodexBaseURL(u string) bool {
	if u == "" {
		return false
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return false
	}
	return !strings.ContainsAny(u, " \t\n\r\"'\\")
}

// allDigits reports whether s is a non-empty run of decimal digits.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
