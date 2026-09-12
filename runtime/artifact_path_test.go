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
	// Conditional on the fetched artifact being present, which is true exactly
	// when one was declared and downloaded — no second copy of the
	// deployment's configuration. Either spelling of the test satisfies this:
	// the file is the stricter of the two and the prelude uses it, but the
	// property asserted here is "conditional", not one operator.
	if !strings.Contains(paneStagedEnvPrelude, "-f \"$NOMAD_TASK_DIR/") &&
		!strings.Contains(paneStagedEnvPrelude, "-d \"$NOMAD_TASK_DIR/") {
		t.Errorf("the prepend must be conditional on the fetched artifact existing:\n%s", paneStagedEnvPrelude)
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

// THE THREE DEFECTS BELOW were all live at once, in a mechanism two tests
// already declared proven. Measured on the Nomad client node, in the alloc directory of
// 8c6b1b93, which is the only place they are visible:
//
//	<nomad data dir>/alloc/<alloc>/agent/
//	  drwxr-xr-x root   root    gcbin          <- SIBLING of local/, not inside it
//	  drwxrwxrwx nobody nogroup local          <- the only dir mounted in the box
//	<nomad data dir>/alloc/<alloc>/agent/gcbin/
//	  -rw-r--r-- root root 273795840 gc-linux-amd64
//
// so inside the box $NOMAD_TASK_DIR was EMPTY: the download succeeded, Nomad
// reported "Downloading Artifacts", the task started, and the file was
// somewhere the container cannot see. Had it been visible it was named
// gc-linux-amd64, which no PATH lookup for `gc` finds. Had it been named
// right it was mode 0644, which cannot execute.
//
// Three independent failures in series, each of which alone makes the feature
// a no-op, and the shape of all three is the same: the fetch reports success
// and the seat runs the image's stale gc. Nothing failed. The log shipper's
// artifact, 140 lines away in the same file, already used the right prefix.

func TestArtifactLandsWhereTheContainerCanSeeIt(t *testing.T) {
	art := resolveGCArtifact("https://example.test/gc-{commit}/gc", "abc123def")
	task := sessionTask("gc-agent:x", art)
	var dest string
	for _, a := range task.Artifacts {
		if strings.Contains(a.RelativeDest, gcArtifactDest) {
			dest = a.RelativeDest
		}
	}
	if dest == "" {
		t.Fatal("no gc artifact declared with the feature on")
	}
	// Nomad resolves RelativeDest against the TASK ROOT, whose children are
	// local/, secrets/, tmp/ and private/ — and only some of those are mounted
	// into the container. A bare "gcbin" is a fourth sibling, invisible in the
	// box.
	if !strings.HasPrefix(dest, "local/") {
		t.Errorf("artifact dest %q is not under local/, so it lands beside the task dir and the container never sees it", dest)
	}
	// Named, not defaulted: go-getter in directory mode keeps the URL's own
	// basename (gc-linux-amd64), which no lookup for `gc` resolves.
	if !strings.HasSuffix(dest, "/"+gcBinaryName) {
		t.Errorf("artifact dest %q does not name the file %q, so the fetched binary keeps the URL's basename and is not findable as gc", dest, gcBinaryName)
	}
}

func TestArtifactIsFetchedAsASingleFile(t *testing.T) {
	art := resolveGCArtifact("https://example.test/gc-{commit}/gc", "abc123def")
	task := sessionTask("gc-agent:x", art)
	for _, a := range task.Artifacts {
		if !strings.Contains(a.RelativeDest, gcArtifactDest) {
			continue
		}
		// "any" lets go-getter decide, and it decides "directory" for a dest
		// with no recognised archive extension — which is how the binary kept
		// its own basename. "file" makes the dest the filename.
		if a.GetterMode != "file" {
			t.Errorf("artifact getter mode is %q; it must be \"file\" or the dest is treated as a directory and the binary keeps the URL's basename", a.GetterMode)
		}
	}
}

func TestPreludeMakesTheFetchedGCExecutable(t *testing.T) {
	// 0644 from an HTTP download. Nomad's artifact stanza has no mode option,
	// so the only layer that can fix it is the box's own shell.
	if !strings.Contains(paneStagedEnvPrelude, "chmod") {
		t.Fatalf("nothing makes the fetched gc executable; it arrives 0644 and every invocation is Permission denied:\n%s", paneStagedEnvPrelude)
	}
	// And it must NOT fall back to the image's gc when chmod is unavailable:
	// a loud "Permission denied" is recoverable, a silent stale binary is the
	// defect this whole mechanism exists to remove.
	if strings.Contains(paneStagedEnvPrelude, "-x ") && strings.Contains(paneStagedEnvPrelude, "else") {
		t.Errorf("the prelude appears to branch on executability; it must prepend regardless so a broken fetch fails loudly instead of silently running the image's gc:\n%s", paneStagedEnvPrelude)
	}
}
