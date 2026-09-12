package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An arbitrary cluster wants to place an allocation anywhere. A registry-less
// image reference cannot support that, and the failure is invisible in the
// jobspec: the constraint list looks clean, the scheduler places freely, and
// the allocation fails to start on any node that was not the build host.
//
// This estate ships exactly such a reference today (a bare gc-agent:<pin>
// built on one node with no registry), so the pack must say so rather than let
// an adopter discover it from a placement that lands one node over.

func TestImageRegistryFollowsDockersOwnRule(t *testing.T) {
	cases := []struct {
		ref, want string
	}{
		// No registry component at all — resolves against Docker Hub, which
		// will not have a privately built agent image.
		{"gc-agent:3a3dda06a", ""},
		{"gc-agent", ""},
		// A Hub NAMESPACE is not a registry host: no dot, no colon.
		{"library/ubuntu:24.04", ""},
		{"myorg/gc-agent:1", ""},
		// Real registry hosts: a dot, a port, or exactly localhost.
		{"registry.example.com/gc-agent:1", "registry.example.com"},
		{"registry.example.com:5000/ns/gc-agent:1", "registry.example.com:5000"},
		{"localhost:5000/gc-agent:1", "localhost:5000"},
		{"localhost/gc-agent:1", "localhost"},
		{"ghcr.io/org/gc-agent@sha256:abc", "ghcr.io"},
		{"", ""},
	}
	for _, c := range cases {
		if got := imageRegistry(c.ref); got != c.want {
			t.Errorf("imageRegistry(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
}

func TestRegistrylessIsDetectedAndExplained(t *testing.T) {
	if !imageIsRegistryless("gc-agent:3a3dda06a") {
		t.Fatal("a bare name must be detected as registry-less; it is this estate's own shape")
	}
	if imageIsRegistryless("registry.example.com/gc-agent:1") {
		t.Fatal("a reference with a registry host must not be flagged")
	}
	if imageIsRegistryless("") {
		t.Fatal("an unset image is a different fault with its own message; do not claim it is registry-less")
	}

	w := registrylessImageWarning("gc-agent:3a3dda06a")
	for _, want := range []struct{ needle, why string }{
		{"gc-agent:3a3dda06a", "the reference is not quoted back, so it is unclear which image"},
		{"LOCAL image cache", "it does not say where the image can come from"},
		{"INVISIBLE", "it does not say the pin carries no constraint, which is why this surprises people"},
		{"registry", "it names no remedy"},
	} {
		if !strings.Contains(w, want.needle) {
			t.Errorf("the warning lacks %q: %s\n%s", want.needle, want.why, w)
		}
	}
	if registrylessImageWarning("registry.example.com/gc-agent:1") != "" {
		t.Error("a registry-qualified image must produce no warning, or the warning becomes noise")
	}
}

// TestCompatibilityDocStatesTheRegistryRequirement holds the page to the code.
// The claim "check warns on a registry-less image" is only true while the
// warning exists, and an adopter planning a cluster reads this page instead of
// the source.
func TestCompatibilityDocStatesTheRegistryRequirement(t *testing.T) {
	doc := compatibilityDocBody(t)
	for _, want := range []struct{ needle, why string }{
		{"registry is effectively required", "the page does not state the requirement as a requirement"},
		{"local image cache", "it does not say where a bare reference can resolve from"},
		{"Docker `auth` block", "it no longer states that private registries have no per-job credential path"},
	} {
		if !strings.Contains(doc, want.needle) {
			t.Errorf("COMPATIBILITY.md lacks %q: %s", want.needle, want.why)
		}
	}
	// The page promises `check` warns. Assert the warning is real, against the
	// live function, not against prose.
	if registrylessImageWarning("gc-agent:1") == "" {
		t.Error("COMPATIBILITY.md tells adopters check warns on a registry-less image, but the " +
			"warning function returns nothing — the page would be promising a check that does not exist")
	}
}

// TestCompatibilityDocDoesNotContradictPerRoleImages guards a self-contradiction
// that an outside install review caught.
//
// COMPATIBILITY.md's known-limitations list claimed there was no per-lane image
// override and that "a codex lane and a claude lane cannot coexist", while
// README documented per-role images and the code implemented them. Of the two
// possible errors, the stale LIMITATION is the expensive one: an adopter who
// believes it stands up a second deployment they do not need.
func TestCompatibilityDocDoesNotContradictPerRoleImages(t *testing.T) {
	doc := compatibilityDocBody(t)
	for _, stale := range []string{
		"cannot\n   coexist on one deployment",
		"There is no per-lane, per-provider or\n   per-agent override",
	} {
		if strings.Contains(doc, stale) {
			t.Errorf("COMPATIBILITY.md still asserts there is no per-role image override:\n  %q\n"+
				"The code supports it (sessionAgentImage) and README documents it. A stale "+
				"limitation costs an adopter a second deployment they do not need.", stale)
		}
	}
	// The live mechanism must still be there, or the retraction is the lie.
	if got := sessionAgentImage(stageConfig{Env: map[string]string{envAgentImage: "r/x:1"}}); got != "r/x:1" {
		t.Fatalf("sessionAgentImage = %q; COMPATIBILITY.md now says the limitation is RESOLVED, so "+
			"the per-session read must exist", got)
	}
}

// TestBuildImageHelpFlagWorks pins the README's own claim. The script rejected
// --help with "unknown command" while the README said it "lists every verb".
func TestBuildImageHelpFlagWorks(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "toolchain", "build-image.sh"))
	if err != nil {
		t.Fatalf("reading build-image.sh: %v", err)
	}
	if !strings.Contains(string(body), "--help|-h") {
		t.Error("build-image.sh does not accept --help/-h, but README claims " +
			"`toolchain/build-image.sh --help` lists every verb")
	}
	readme, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	if !strings.Contains(string(readme), "build-image.sh --help") {
		t.Skip("README no longer claims --help; nothing to keep honest")
	}
}
