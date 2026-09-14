package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// A box gets its city checkout from a read-only mirror, cloned at prime
// behind an operator knob (absent = no checkout). These tests pin that
// contract at the one seam that decides it
// — paneCwdPrelude, which every launch path (buildLaunchCommandForPane) runs
// through — using the same paneBody helper agent_capture_test.go already
// established for locating the rendered pane body by content.

func TestCityMirrorUnsetProducesNoCloneCommand(t *testing.T) {
	t.Setenv(envCityMirrorURL, "")
	body := paneBody(t)
	if strings.Contains(body, "git clone") || strings.Contains(body, "git fetch") {
		t.Fatalf("GC_NOMAD_CITY_MIRROR_URL unset must not add a clone/fetch step; pane body: %s", body)
	}
}

func TestCityMirrorSetProducesCloneCommand(t *testing.T) {
	t.Setenv(envCityMirrorURL, "git://mirror.internal:9418/city.git")
	body := paneBody(t)
	if !strings.Contains(body, "git clone") {
		t.Fatalf("GC_NOMAD_CITY_MIRROR_URL set must add a git clone step; pane body: %s", body)
	}
	if !strings.Contains(body, "git://mirror.internal:9418/city.git") {
		t.Fatalf("pane body must carry the configured mirror URL verbatim; pane body: %s", body)
	}
	if !strings.Contains(body, "git fetch") {
		t.Fatalf("GC_NOMAD_CITY_MIRROR_URL set must also add the already-cloned (fetch+reset) branch, for an adopted/relaunched box; pane body: %s", body)
	}
}

func TestCityMirrorBranchIsPassedToClone(t *testing.T) {
	t.Setenv(envCityMirrorURL, "git://mirror.internal:9418/city.git")
	t.Setenv(envCityMirrorBranch, "main")
	body := paneBody(t)
	if !strings.Contains(body, "--branch main") && !strings.Contains(body, "--branch 'main'") {
		t.Fatalf("GC_NOMAD_CITY_MIRROR_BRANCH must be passed to git clone as --branch; pane body: %s", body)
	}
}

func TestCityMirrorPreludeEmptyWhenURLUnset(t *testing.T) {
	t.Setenv(envCityMirrorURL, "")
	if got := cityMirrorPrelude(); got != "" {
		t.Fatalf("cityMirrorPrelude() with no URL configured must return \"\" (byte-identical to pre-knob behavior), got: %q", got)
	}
}

func TestCityMirrorPreludeWritesShaMarker(t *testing.T) {
	t.Setenv(envCityMirrorURL, "git://mirror.internal:9418/city.git")
	got := cityMirrorPrelude()
	if !strings.Contains(got, "NOMAD_SECRETS_DIR/"+cityMirrorDirName+"/"+cityMirrorShaFile) {
		t.Fatalf("cityMirrorPrelude() must record the resulting HEAD sha under $NOMAD_SECRETS_DIR/%s/%s for syncCityCheckoutMarker to read back; got: %q", cityMirrorDirName, cityMirrorShaFile, got)
	}
}

// TestCityMirrorRenderedPaneScriptHasValidShellSyntax is the regression test
// for the incident this pack shipped once already: cityMirrorPrelude used to
// return a `{ ...; }` GROUP with no trailing separator, and paneCwdPrelude
// concatenates its return value directly onto agentCaptureBody's next
// command with only a space in between — "} __gc_cap=..." — which is a
// shell SYNTAX ERROR (a `}` group, unlike a `)` subshell, is not itself a
// command terminator). Every worker-local box failed within seconds of the
// knob going live in production: the pane's `/bin/sh -c` script died before
// agentCaptureBody's own exit-record trap ever ran, so there was no
// agent.exit, no diagnostics.log line, nothing — indistinguishable from the
// outside from tmux itself being broken. `sh -n` catches exactly this class
// of defect for a few milliseconds of cost, on every pane body this pack can
// render, with or without a live Nomad/tmux anywhere in reach.
func TestCityMirrorRenderedPaneScriptHasValidShellSyntax(t *testing.T) {
	for _, tc := range []struct {
		name   string
		url    string
		branch string
	}{
		{"unset", "", ""},
		{"set", "git://mirror.internal:9418/city.git", ""},
		{"set with branch", "git://mirror.internal:9418/city.git", "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envCityMirrorURL, tc.url)
			t.Setenv(envCityMirrorBranch, tc.branch)
			for _, wd := range []string{"", "/some/work/dir"} {
				script := paneStagedEnvPrelude + paneCwdPrelude(wd) + agentCaptureBody(testPaneCommand)
				cmd := exec.Command("sh", "-n", "-c", script)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("workDir=%q: rendered pane script is not valid shell syntax: %v\noutput: %s\nscript:\n%s", wd, err, out, script)
				}
			}
		})
	}
}

// TestCityMirrorKnobDoesNotBreakPaneLaunch is the end-to-end sibling of the
// syntax test above: it drives a REAL start through the fakenomad harness
// (which really execs /bin/sh and real tmux, per ops_test.go's
// newTestLifecycle/testPaneCommand doc) with the knob set to an
// intentionally UNREACHABLE mirror URL. The point is not that the clone
// succeeds — it can't, there is nothing listening — it is that an
// unreachable/failing mirror must never take the pane down with it.
// opIsRunning kill -0's the pane's pid (testPaneCommand's own doc comment:
// "a pane that exits at once reads as an agent that died"), so this is
// exactly the signal that went red in production and must stay green here.
func TestCityMirrorKnobDoesNotBreakPaneLaunch(t *testing.T) {
	t.Setenv(envCityMirrorURL, "git://127.0.0.1:1/unreachable-city-mirror.git")
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-city-mirror-unreachable"

	if err := l.opStartWithConfig(ctx, session, stageConfig{Command: testPaneCommand}); err != nil {
		t.Fatalf("start with GC_NOMAD_CITY_MIRROR_URL set to an unreachable mirror: %v", err)
	}

	running, err := l.opIsRunning(ctx, session)
	if err != nil || !running {
		t.Fatalf("is-running after start with an unreachable city mirror = (%v, %v), want (true, nil) — the pane must survive a mirror it cannot reach", running, err)
	}
}
