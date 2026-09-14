package main

import (
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
