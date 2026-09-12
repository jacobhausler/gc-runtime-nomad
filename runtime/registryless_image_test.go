package main

import (
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
