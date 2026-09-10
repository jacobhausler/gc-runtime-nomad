package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// imageModeProviderEnv is the provider environment as the estate stages it
// today (city.toml's provider env plus the harness scratch dirs it runs under):
// a seat description wrapped in the orchestrator's own filesystem. Proof run 11
// measured this exact shape inside a live box - the box's exec session had the
// image's PATH with /opt/codex/bin/current/codex present, and after the pane
// sourced env.sh, `command -v codex` was MISSING (cr-jr2kw7).
func imageModeProviderEnv() map[string]string {
	return map[string]string{
		pathEnvName:       "/private/tmp/gctc-42/toolchain:/Users/home/scratch/p62/bin:/Users/home/.local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		homeEnvName:       "/Users/home",
		tmpEnvName:        "/tmp",
		tempEnvName:       "/tmp",
		tmpdirEnvName:     "/var/folders/zz/zyxvpxvq6722/T/",
		"XDG_CONFIG_HOME": "/Users/home/.config",
		"XDG_CACHE_HOME":  "/Users/home/.cache",
		gcHomeEnvName:     "/Users/home/.gc",
		codexHomeEnvName:  "/Users/home/.codex",
		// A seat value that names the orchestrator's HOME without naming the
		// key: the class rule, not a key list, is what has to catch this.
		"GC_WRAPPED_BIN": "/Users/home/scratch/p62/bin/codex",
		// What the seat actually needs: no paths at all.
		"GC_SESSION":              "image-lane",
		"TERM":                    "xterm-256color",
		"GC_CITY_URL":             "https://192.168.0.107:8443/api",
		codexModelProviderKey:     "haus_qwen38_rtxpro6000_direct",
		codexModelProviderBaseURL: "http://192.168.0.122:18030/v1",
		codexModelKey:             "rtxpro6000-gateway/qwen38-next",
		codexReasoningEffortKey:   "medium",
		codexCompactTokensKey:     "131072",
	}
}

// TestImageModeEnvScriptDropsOrchestratorFilesystem is the RED/GREEN pair for
// cr-jr2kw7: an image-mode box must source an env.sh that leaves its own
// filesystem alone. RED as measured live (env.sh:76 exported the orchestrator's
// PATH, so the pane could not find codex, died, and took its tmux server with
// it); GREEN here is that the sourced PATH is still the box's and its shell is
// still reachable, while the seat's non-path values crossed unchanged.
func TestImageModeEnvScriptDropsOrchestratorFilesystem(t *testing.T) {
	l, _ := newTestLifecycle(t)
	l.agentImage = "localhost/gc-agent:test"
	ctx := context.Background()
	const session = "sess-image-box-filesystem"

	if err := l.opProvisionWithConfig(ctx, session, stageConfig{Env: imageModeProviderEnv()}); err != nil {
		t.Fatalf("image-mode provision: %v", err)
	}

	envSh, err := l.opExec2(ctx, session, `cat "$NOMAD_SECRETS_DIR/env.sh"`)
	if err != nil {
		t.Fatalf("read env.sh: %v", err)
	}
	for _, leak := range []string{"/Users/home", "/private/tmp/gctc", "/var/folders", "/opt/homebrew"} {
		if strings.Contains(envSh, leak) {
			t.Fatalf("env.sh carries the orchestrator path %q: %s", leak, envSh)
		}
	}
	for _, name := range []string{pathEnvName, homeEnvName, tmpEnvName, tempEnvName, tmpdirEnvName, "XDG_CONFIG_HOME", "XDG_CACHE_HOME", gcHomeEnvName, "GC_WRAPPED_BIN"} {
		if strings.Contains(envSh, "export "+name+"=") {
			t.Fatalf("env.sh still exports the orchestrator's %s: %s", name, envSh)
		}
	}
	for _, want := range []string{
		`export GC_SESSION='image-lane'`,
		`export TERM='xterm-256color'`,
		`export GC_CITY_URL='https://192.168.0.107:8443/api'`,
		`export ` + codexModelProviderKey + `='haus_qwen38_rtxpro6000_direct'`,
		`export ` + codexModelKey + `='rtxpro6000-gateway/qwen38-next'`,
		`export ` + codexReasoningEffortKey + `='medium'`,
		`export ` + codexCompactTokensKey + `='131072'`,
	} {
		if !strings.Contains(envSh, want) {
			t.Fatalf("env.sh dropped a seat value the pane needs (%s): %s", want, envSh)
		}
	}
	// The strip must not disturb the home cr-usnvaw stages in the same file.
	if want := `export CODEX_HOME="$NOMAD_SECRETS_DIR/` + codexHomeDir + `"`; !strings.Contains(envSh, want) {
		t.Fatalf("env.sh missing the in-box CODEX_HOME export %q: %s", want, envSh)
	}

	// The measurement that was RED: what the pane sees after sourcing.
	boxPath, err := l.opExec2(ctx, session, `printf %s "$PATH"`)
	if err != nil {
		t.Fatalf("reading the box PATH: %v", err)
	}
	sourcedPath, err := l.opExec2(ctx, session, `. "$NOMAD_SECRETS_DIR/env.sh"; printf %s "$PATH"`)
	if err != nil {
		t.Fatalf("reading the sourced PATH: %v", err)
	}
	if sourcedPath != boxPath {
		t.Fatalf("sourcing env.sh replaced the box PATH:\n box: %q\n sourced: %q", boxPath, sourcedPath)
	}
	sh, err := l.opExec2(ctx, session, `. "$NOMAD_SECRETS_DIR/env.sh"; command -v sh`)
	if err != nil {
		t.Fatalf("the pane could not resolve its own shell after sourcing env.sh: %v", err)
	}
	if strings.TrimSpace(sh) == "" {
		t.Fatalf("sourcing env.sh left no resolvable shell on PATH: %s", sourcedPath)
	}
}

// TestLegacyExecModeLeavesOrchestratorFilesystemInEnvScript protects the
// stage() side of the split: the stripping is image-mode only, so a box with no
// image keeps the provider's own env.sh bytes untouched.
func TestLegacyExecModeLeavesOrchestratorFilesystemInEnvScript(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-legacy-box-filesystem"

	if err := l.opProvisionWithConfig(ctx, session, stageConfig{Env: imageModeProviderEnv()}); err != nil {
		t.Fatalf("legacy-mode provision: %v", err)
	}

	envSh, err := l.opExec2(ctx, session, `cat "$NOMAD_SECRETS_DIR/env.sh"`)
	if err != nil {
		t.Fatalf("read env.sh: %v", err)
	}
	if !strings.Contains(envSh, "export "+pathEnvName+"='/private/tmp/gctc-42/toolchain:") {
		t.Fatalf("legacy env.sh no longer passes the deployment's PATH through unchanged: %s", envSh)
	}
	if !strings.Contains(envSh, "export "+homeEnvName+"='/Users/home'") {
		t.Fatalf("legacy env.sh no longer passes the deployment's HOME through unchanged: %s", envSh)
	}
}

// TestImageModeLaunchDoesNotSeedTmuxWithTheOrchestratorFilesystem covers the
// second door: tmux `new-session -e` seeds the SERVER environment every pane
// inherits, so a clean env.sh alone would not save the toolchain. Identity and
// terminal keys keep riding argv exactly as before (E1a §4.5 allow-list).
func TestImageModeLaunchDoesNotSeedTmuxWithTheOrchestratorFilesystem(t *testing.T) {
	env := imageModeProviderEnv()
	joined := strings.Join(buildLaunchCommandForPane(boxFilesystemEnvInput(env), "/work", "codex"), " ")
	for _, name := range []string{pathEnvName, homeEnvName, tmpdirEnvName, gcHomeEnvName} {
		if strings.Contains(joined, "-e "+name+"=") {
			t.Fatalf("image-mode launch seeds the tmux server with %s: %s", name, joined)
		}
	}
	if !strings.Contains(joined, "-e TERM=xterm-256color") {
		t.Fatalf("image-mode launch stopped carrying the argv-safe terminal key: %s", joined)
	}
}

// TestBoxFilesystemEnvInputStripsTheClassNotTheSeat is the unit seam: the class
// is names plus values naming the orchestrator's own roots, and it must not be
// a prefix match that eats a sibling directory.
func TestBoxFilesystemEnvInputStripsTheClassNotTheSeat(t *testing.T) {
	got := boxFilesystemEnvInput(map[string]string{
		pathEnvName:              "/opt/homebrew/bin:/usr/bin",
		homeEnvName:              "/Users/home",
		tmpdirEnvName:            "/tmp/orchestrator-t",
		"XDG_STATE_HOME":         "/Users/home/.local/state",
		gcHomeEnvName:            "/Users/home/.gc",
		codexHomeEnvName:         "/Users/home/.codex",
		"GC_WRAPPED_BIN":         "/Users/home/.codex/bin/codex",
		"GC_BOX_TOOL":            "/Users/homeless/tools/codex",
		"GC_BOX_SHORT":           "/Users/homeless",
		"GC_TOOL_INSIDE_TMPROOT": "/tmp/orchestrator-t/toolchain/bin",
		"GC_TMP_SIBLING":         "/tmp/orchestrator-toolchain",
		"GC_MODEL":               "qwen38-next",
		"GC_CITY_URL":            "https://192.168.0.107:8443/api",
	})

	for _, name := range []string{pathEnvName, homeEnvName, tmpdirEnvName, "XDG_STATE_HOME", gcHomeEnvName, codexHomeEnvName, "GC_WRAPPED_BIN", "GC_TOOL_INSIDE_TMPROOT"} {
		if _, ok := got[name]; ok {
			t.Fatalf("%s survived the filesystem strip: %v", name, got)
		}
	}
	for _, name := range []string{"GC_BOX_TOOL", "GC_BOX_SHORT", "GC_TMP_SIBLING", "GC_MODEL", "GC_CITY_URL"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("%s is not the orchestrator's filesystem and must cross: %v", name, got)
		}
	}
}

// TestImageModePaneKeepsTheImageToolchain is the in-box proof of both doors: it
// starts a real image-mode pane whose staged env carries an orchestrator PATH
// that names a directory the box does not have, and asks the PANE what its PATH
// became and whether it can still resolve a shell. Pre-fix the answer was the
// Mac's PATH and no shell at all - the shape proof run 11 hit when codex could
// not be found and the pane died with its tmux server (cr-jr2kw7).
func TestImageModePaneKeepsTheImageToolchain(t *testing.T) {
	requireTmuxForDriverTest(t)

	l, _ := newTestLifecycle(t)
	l.agentImage = "localhost/gc-agent:test"
	ctx := context.Background()
	const session = "sess-image-pane-path"

	env := imageModeProviderEnv()
	// The orchestrator PATH, rewritten to a directory this builder certainly
	// does not have: whatever the pane inherits after sourcing env.sh names it.
	const foreignBin = "/nonexistent/orchestrator-toolchain/bin"
	env[pathEnvName] = foreignBin + ":/Users/home/scratch/p62/bin:/usr/bin:/bin"

	dir := t.TempDir()
	pathMarker := filepath.Join(dir, "pane-path")
	shellMarker := filepath.Join(dir, "pane-shell")
	rendered := `printf %s "$PATH" > ` + pathMarker + `; command -v sh > ` + shellMarker

	if err := l.opStartWithConfig(ctx, session, stageConfig{Env: env, Command: rendered}); err != nil {
		t.Fatalf("image-mode start: %v", err)
	}
	if !waitForPath(shellMarker, 10*time.Second) {
		t.Fatalf("the image-mode pane never reached its command (no %s)", shellMarker)
	}

	panePath, err := os.ReadFile(pathMarker)
	if err != nil {
		t.Fatalf("read the pane PATH: %v", err)
	}
	if strings.Contains(string(panePath), foreignBin) || strings.Contains(string(panePath), "/Users/home") {
		t.Fatalf("the pane ran with the orchestrator PATH: %q", string(panePath))
	}
	paneShell, err := os.ReadFile(shellMarker)
	if err != nil {
		t.Fatalf("read the pane shell: %v", err)
	}
	if strings.TrimSpace(string(paneShell)) == "" {
		t.Fatalf("the pane could not resolve a shell on its PATH (%q): the toolchain is hidden", string(panePath))
	}
}
