package main

import (
	"strings"
	"testing"
)

// WHY: two prospective adopters hit this independently, one as blocks-install
// and one as blocks-production.
//
//	"GC_NOMAD_AGENT_IMAGE is required with no default, and producing it is a
//	 two-stage manual process"
//	"Effectively requires a container registry reachable by every Nomad client
//	 ... a bare/local image tag only works via a node's local cache, silently
//	 pinning every session to whichever node built it"
//
// The second is real and this pack already DETECTS it — registrylessImageWarning
// says a bare name "names no registry, so no node can ever pull it". Detecting
// it is not solving it: the adopter is still told to stand up a registry, which
// is infrastructure the pack does not ship and cannot assume.
//
// Nomad's docker driver has its own answer, and it needs no registry and no
// per-client daemon configuration: `load = "<tarball>"`, documented as
// equivalent to `docker load -i`, fed by an artifact stanza. The artifact must
// declare `archive = false` or Nomad unpacks the tar before the driver can load
// it.
//
// This is strictly better than a registry for the case the pack is in: every
// eligible node fetches and loads the SAME checksummed tarball, so the hidden
// placement pin disappears with it.

func imageTarTask(t *testing.T) nomadTask {
	t.Helper()
	return sessionTask("gc-agent:x", gcArtifactConfig{}, imageTarConfig{
		Source:   "https://example.invalid/gc-agent-x.tar",
		Checksum: "file:https://example.invalid/gc-agent-x.tar.sha256",
	})
}

func TestImageTarIsLoadedNotPulled(t *testing.T) {
	task := imageTarTask(t)
	load, ok := task.Config["load"]
	if !ok {
		t.Fatalf("the docker task declares no `load`, so the image can only come from a registry: %v", task.Config)
	}
	if !strings.Contains(load.(string), imageTarDest) {
		t.Errorf("load=%v does not name the fetched tarball", load)
	}
	// image must STILL be set: load supplies the bytes, image names which tag
	// inside the tarball to run.
	if _, ok := task.Config["image"]; !ok {
		t.Errorf("`image` was dropped; load supplies the bytes but the tag still selects what runs")
	}
}

func TestImageTarArtifactDoesNotUnpack(t *testing.T) {
	task := imageTarTask(t)
	var found *nomadArtifact
	for i := range task.Artifacts {
		if strings.Contains(task.Artifacts[i].RelativeDest, imageTarDest) {
			found = &task.Artifacts[i]
		}
	}
	if found == nil {
		t.Fatal("no image-tarball artifact declared")
	}
	if got := found.GetterOptions["archive"]; got != "false" {
		t.Errorf(`archive=%q; Nomad auto-extracts a .tar and the driver then has no tarball to load — it must be "false"`, got)
	}
	if found.GetterOptions["checksum"] == "" {
		t.Errorf("the image tarball is fetched with no checksum: %v", found.GetterOptions)
	}
	// Interpolated from meta, so a dispatch can override the image per session
	// exactly as it can override the tag today.
	if !strings.Contains(found.GetterSource, "NOMAD_META_") {
		t.Errorf("tarball source is baked, not interpolated: %q", found.GetterSource)
	}
}

// Off by default: a deployment that has a registry should keep using it, and
// an unset tarball URL must change nothing at all.
func TestImageTarAbsentChangesNothing(t *testing.T) {
	task := sessionTask("gc-agent:x")
	if _, ok := task.Config["load"]; ok {
		t.Errorf("`load` is set with no tarball configured: %v", task.Config)
	}
	for _, a := range task.Artifacts {
		if strings.Contains(a.RelativeDest, imageTarDest) {
			t.Errorf("an image-tarball artifact is declared with none configured: %+v", a)
		}
	}
}
