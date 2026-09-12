package main

import (
	"strings"
	"testing"
)

// THE DEFECT THIS GUARDS, measured live 2026-09-12 in alloc
// 8c6b1b93-1741-ef32-764c-c145c53695b6:
//
//	$ nomad alloc exec ... /bin/bash -lc 'echo PATH=$PATH; ls'
//	PATH=/local/gcbin:${PATH}
//	/bin/bash: line 1: ls: command not found
//
// The pack put PATH="${NOMAD_TASK_DIR}/gcbin:${PATH}" in the TASK ENV.
// Nomad interpolates its own variables there — ${NOMAD_TASK_DIR} resolved —
// but ${PATH} is not a Nomad variable, so it was passed through as the
// six literal characters. Every box on the lane therefore had a PATH of
// exactly one directory and could not find sh, ls, tmux or the agent.
//
// The box's PATH can only be prepended from INSIDE the box, in a shell, where
// $PATH holds what the image set. image_env.go already states the rule this
// broke: the image owns PATH, and an orchestrator value for it is not a
// preference that crosses harmlessly, it is a breakage.

func TestJobspecNeverSetsPATH(t *testing.T) {
	art := gcArtifactConfig{
		Source:   "https://example.invalid/gc-linux-amd64",
		Checksum: "file:https://example.invalid/gc-linux-amd64.sha256",
	}
	job := parentJobSpec("gc", "prod-services", "gc-sessions", logShipperConfig{}, "img:tag", placementConfig{}, art)
	for _, tg := range job.TaskGroups {
		for _, task := range tg.Tasks {
			got, ok := task.Env[pathEnvName]
			if ok {
				t.Fatalf("task %q sets %s=%q in the task env; Nomad does not interpolate ${PATH} there, so this REPLACES the image's PATH with just this value", task.Name, pathEnvName, got)
			}
		}
	}
}

// Whatever prepends the artifact directory must do it in a shell, so $PATH
// expands to the image's own value, and must tolerate the artifact being
// absent so a deployment that does not pin one is unchanged.
func TestPanePreludePrependsFetchedGCInAShell(t *testing.T) {
	if !strings.Contains(paneStagedEnvPrelude, gcArtifactDest) {
		t.Fatalf("the pane prelude never puts the fetched gc on PATH, so a box that downloaded it still runs the image's gc:\n%s", paneStagedEnvPrelude)
	}
	if !strings.Contains(paneStagedEnvPrelude, `:$PATH`) {
		t.Errorf("the prepend must expand the image's own $PATH in-box, not replace it:\n%s", paneStagedEnvPrelude)
	}
	// Conditional on the directory, which exists exactly when Nomad fetched
	// the artifact — no second copy of the deployment's configuration.
	if !strings.Contains(paneStagedEnvPrelude, "-d ") {
		t.Errorf("the prepend must be conditional on the artifact directory existing:\n%s", paneStagedEnvPrelude)
	}
	// It has to come before the agent script, which is the whole point.
	if strings.Index(paneStagedEnvPrelude, gcArtifactDest) > strings.Index(paneStagedEnvPrelude, "unset GC_CITY") {
		t.Errorf("the prepend runs after the prelude's own tail; it must precede the agent command:\n%s", paneStagedEnvPrelude)
	}
}

// The pack's own staleness probe reads `gc version` INSIDE the box. If it does
// not use the same prepend it reads the IMAGE's gc and compares that to the
// host — so a box correctly running a freshly fetched gc would be reported
// stale, and a box that failed to fetch would be reported fresh. Exactly
// inverted, silently.
func TestInBoxVersionProbeUsesTheFetchedGC(t *testing.T) {
	cmd := inBoxVersionCommand()
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, gcArtifactDest) {
		t.Fatalf("the in-box version probe does not put the fetched gc on PATH, so it measures the image's gc: %q", joined)
	}
	if !strings.Contains(joined, `:$PATH`) {
		t.Errorf("the in-box probe must expand the image's $PATH in-box: %q", joined)
	}
}
