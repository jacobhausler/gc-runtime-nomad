package main

import (
	"strings"
)

// The prebaked-image shape has no deployment bootstrap: GC_NOMAD_AGENT_LAUNCH_SCRIPT
// is ignored once GC_NOMAD_AGENT_IMAGE is set (main.go), because the image owns the
// agent toolchain. That makes the IMAGE's PATH, HOME, TMPDIR and XDG dirs the only
// ones an agent in the box can run with - yet stage() sourced the orchestrator's own
// values for every one of them into env.sh, and the estate's provider env carries them
// verbatim (harness scratch toolchain, /opt/homebrew/bin, /Users/home/.local/bin).
// Proof run 11 measured it: the exec session's PATH was the image's
// (/opt/codex/bin/current:...) and /opt/codex/bin/current/codex existed, but after
// paneStagedEnvPrelude sourced env.sh, `command -v codex` was MISSING - the pane died,
// its tmux server exited with it, and gc reported only "tmux exited 1"
// (ops/receipts/nrt-d3/2026-09-10-image-mode-proof-9-11.md, cr-jr2kw7).
//
// cr-usnvaw fixed exactly this class for CODEX_HOME: remove the inherited value from
// env.sh rather than shadow it. This is that treatment applied to the whole
// filesystem-shaped class it left behind, on the same rule - env.sh IS the pane's
// environment, and a path that exists only on the orchestrator is not data there, it
// is a breakage.
//
// Only image mode is in scope. The legacy exec shape's env.sh and launch bytes stay
// byte-identical - the deployment bootstrap owns that shape, and unsetting
// GC_NOMAD_AGENT_IMAGE stays this seam's rollback.

const (
	pathEnvName   = "PATH"
	homeEnvName   = "HOME"
	tmpEnvName    = "TMP"
	tempEnvName   = "TEMP"
	tmpdirEnvName = "TMPDIR"
	gcHomeEnvName = "GC_HOME"
	xdgEnvPrefix  = "XDG_"
)

// boxFilesystemEnvNames are the names that describe the machine's filesystem rather
// than the seat. The image owns every one of them, so an orchestrator value for any of
// them is wrong in the box no matter what it points at - no value inspection needed.
//
// GC_HOME is in this family on purpose: the remote-context bridge derives its own
// in-box GC_HOME line from the UNSTRIPPED env (buildRemoteContextEnvLines) and appends
// it after buildEnvScript, so a Mac GC_HOME here could only ever be a stale loser in
// the same file, and a deployment with no staged context is better off with the home
// the image actually has.
var boxFilesystemEnvNames = map[string]bool{
	pathEnvName:   true,
	homeEnvName:   true,
	tmpEnvName:    true,
	tempEnvName:   true,
	tmpdirEnvName: true,
	gcHomeEnvName: true,
}

// boxFilesystemEnvInput returns env with the orchestrator's filesystem removed from
// it, for an image-mode box: every name in boxFilesystemEnvNames plus the XDG_ family,
// and any other key whose value is or sits under the orchestrator's own HOME or TMPDIR
// - a seat value naming a path the box cannot have. Seat values that are not paths
// (the city URL, the model, the reasoning effort, the context sizes) are untouched and
// still cross.
//
// This filters the generated env.sh and the launch's argv-safe `-e` pairs only. Each
// key keeps its ordinary per-key secret file under $NOMAD_SECRETS_DIR, which is what
// this pack's own diagnostics read, so nothing staged becomes unreachable.
func boxFilesystemEnvInput(env map[string]string) map[string]string {
	roots := orchestratorPathRoots(env)
	filtered := make(map[string]string, len(env))
	for key, value := range env {
		if boxFilesystemEnvNames[key] || strings.HasPrefix(key, xdgEnvPrefix) {
			continue
		}
		if namesOrchestratorPath(value, roots) {
			continue
		}
		filtered[key] = value
	}
	return filtered
}

// orchestratorPathRoots are the two paths that define "the orchestrator's own
// filesystem" inside its own environment. They are read from the staged values and
// never from this process, so the filter says the same thing twice about the same wire
// payload and stays deterministic under test.
func orchestratorPathRoots(env map[string]string) []string {
	var roots []string
	for _, key := range []string{homeEnvName, tmpdirEnvName} {
		root := strings.TrimSuffix(strings.TrimSpace(env[key]), "/")
		// A root of "/" (or "") would name every absolute path there is.
		if len(root) > 1 && strings.HasPrefix(root, "/") {
			roots = append(roots, root)
		}
	}
	return roots
}

// namesOrchestratorPath reports whether value is, or lives under, one of the
// orchestrator's roots. The child test is path-aware on purpose: a plain prefix match
// would call /Users/homeless a child of /Users/home.
func namesOrchestratorPath(value string, roots []string) bool {
	candidate := strings.TrimSuffix(strings.TrimSpace(value), "/")
	for _, root := range roots {
		if candidate == root || strings.HasPrefix(candidate, root+"/") {
			return true
		}
	}
	return false
}
