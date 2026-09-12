package main

import (
	"fmt"
	"strings"
)

// The prebaked-image shape has no deployment bootstrap: the pack's former
// GC_NOMAD_AGENT_LAUNCH_SCRIPT seam is deleted outright, because
// the image owns the agent toolchain. That makes the IMAGE's PATH, HOME, TMPDIR and XDG dirs the only
// ones an agent in the box can run with - yet stage() sourced the orchestrator's own
// values for every one of them into env.sh, and the estate's provider env carries them
// verbatim (harness scratch toolchain, /opt/homebrew/bin, $HOME/.local/bin).
// Proof run 11 measured it: the exec session's PATH was the image's
// (/opt/codex/bin/current:...) and /opt/codex/bin/current/codex existed, but after
// paneStagedEnvPrelude sourced env.sh, `command -v codex` was MISSING - the pane died,
// its tmux server exited with it, and gc reported only "tmux exited 1".
//
// The CODEX_HOME staging fixed exactly this class: it removes the inherited value from
// env.sh rather than shadowing it. This is that treatment applied to the whole
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
	// tmuxTmpdirEnvName decides WHERE TMUX LOOKS FOR ITS SOCKET, so an
	// orchestrator value for it is not a preference that crosses harmlessly —
	// it repoints the box at a directory chosen for another machine.
	tmuxTmpdirEnvName = "TMUX_TMPDIR"
	gcHomeEnvName     = "GC_HOME"
	xdgEnvPrefix      = "XDG_"
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
	// TMUX_TMPDIR belongs here and the VALUE RULE BELOW DOES NOT CATCH IT.
	//
	// This estate's supervisor sets TMUX_TMPDIR=/tmp deliberately (its own
	// header calls that a hard requirement), while a Mac's TMPDIR is under
	// /var/folders — so "/tmp" is not under any orchestrator root, survives
	// the value filter, and crosses into the box. tmux inside then resolves
	// $TMUX_TMPDIR/tmux-<uid>/default against a path the orchestrator chose.
	//
	// Observed 2026-09-12 in this pack's own diagnostics, on a launch that
	// failed with the empty-output error this runtime is worst at explaining:
	//   exec ... exited 1: stderr="error connecting to /tmp/tmux-0/default
	//   (No such file or directory)"
	//   exec FAILED: spawning prime window: tmux new-window exited 1:
	// The second line is what an operator sees; the first is the cause.
	//
	// The image owns its own socket directory, exactly as it owns PATH, HOME
	// and TMPDIR. Nothing here needs to know what the right value IS — only
	// that the orchestrator's is wrong in a box.
	tmuxTmpdirEnvName: true,
	gcHomeEnvName:     true,
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
// would call /home/username a child of /home/user.
func namesOrchestratorPath(value string, roots []string) bool {
	candidate := strings.TrimSuffix(strings.TrimSpace(value), "/")
	for _, root := range roots {
		if candidate == root || strings.HasPrefix(candidate, root+"/") {
			return true
		}
	}
	return false
}

// imageRegistry returns the registry host of an image reference, or "" when the
// reference names none.
//
// WHY THIS MATTERS MORE THAN IT LOOKS. A reference with no registry host — say
// "gc-agent:3a3dda06a" — cannot be pulled by anything. Docker resolves a bare
// name against Docker Hub, which will not have a privately built agent image,
// so in practice the only way such a reference ever resolves is from a node's
// LOCAL image cache. That silently pins every session to whichever nodes happen
// to have been the build host, and it is invisible in the jobspec: the
// constraint list looks clean, the scheduler places freely, and the allocation
// then fails to start on any node that was not there when the image was built.
//
// An arbitrary cluster wants to place allocations anywhere, which is exactly
// what a registry-less reference cannot support. The pack cannot fix that for
// an adopter — it does not build their image and must not push one — but it can
// refuse to let it be a surprise.
//
// The rule is Docker's own: the first path segment is a registry host only if
// it contains a "." or a ":", or is exactly "localhost".
func imageRegistry(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	first, rest, ok := strings.Cut(ref, "/")
	if !ok {
		return "" // "name" or "name:tag" — no registry component at all
	}
	_ = rest
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return first
	}
	return "" // "library/name" — a Docker Hub namespace, not a registry host
}

// imageIsRegistryless reports whether this reference can only ever resolve from
// a node's local image cache.
func imageIsRegistryless(ref string) bool {
	return strings.TrimSpace(ref) != "" && imageRegistry(ref) == ""
}

// registrylessImageWarning is the sentence an adopter needs BEFORE the first
// placement that lands on the wrong node, not after.
func registrylessImageWarning(ref string) string {
	if !imageIsRegistryless(ref) {
		return ""
	}
	return fmt.Sprintf("warning: the agent image %q names no registry, so it can only ever resolve "+
		"from a node's LOCAL image cache. Nothing can pull it: a bare name resolves against Docker "+
		"Hub, which will not have a privately built agent image. Every session is therefore pinned to "+
		"whichever nodes already hold a copy, and that pin is INVISIBLE — the jobspec carries no "+
		"constraint for it, the scheduler places freely, and the allocation fails to start on any "+
		"other node. For a cluster that should be able to place a session anywhere, push the image to "+
		"a registry your clients can reach and reference it by that name; otherwise pre-load it on "+
		"every eligible node and expect to redo that on every image change.", ref)
}
