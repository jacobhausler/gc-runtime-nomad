package main

import (
	"os"
	"strings"
	"testing"
)

// COMPATIBILITY.md tells an adopter this pack asks for no host volumes, no
// Vault, no Consul and no declared workload identity. That list is the reason
// the cluster-side ask is modest enough to evaluate in an hour, and three
// separate adopter reviews leaned on it.
//
// Each of those is one line away from becoming false, and a host volume is the
// worst of them: it is a ROOT-level cluster prerequisite (an operator must add
// it to every eligible client's config and restart the agent), and
// `nomad validate` is blind to whether the named volume exists — so a jobspec
// requesting one passes validation and fails at placement, on someone else's
// cluster, with a message about a volume they never configured.
//
// The claim is therefore asserted against the jobspec rather than trusted.

func TestJobspecAsksForNoClusterPrerequisites(t *testing.T) {
	src, err := os.ReadFile("jobspec.go")
	if err != nil {
		t.Fatalf("reading jobspec.go: %v", err)
	}
	code := string(src)

	for _, f := range []struct{ marker, why string }{
		{"HostVolume", "a host volume is a ROOT-level cluster prerequisite on every eligible client, " +
			"and nomad validate cannot tell you the volume is missing — the failure lands at placement " +
			"on the adopter's cluster"},
		{"VolumeRequest", "same: a volume request makes the pack's cluster ask non-modest"},
		{"Vault:", "a Vault stanza makes Vault integration mandatory"},
		{"Consul:", "a Consul stanza makes Consul integration mandatory"},
		{"Identities:", "a declared workload identity changes the ACL model an adopter must set up"},
	} {
		if strings.Contains(code, f.marker) {
			t.Errorf("jobspec.go now contains %q, but COMPATIBILITY.md tells adopters the pack asks "+
				"for none of these. %s. Update the page in the same commit or drop the request.",
				f.marker, f.why)
		}
	}

	// And the page must still make the claim, or the guard is protecting nothing.
	doc := compatibilityDocBody(t)
	if !strings.Contains(doc, "no host volumes") {
		t.Error("COMPATIBILITY.md no longer states that no host volumes are required; that line is " +
			"load-bearing for an adopter sizing the cluster-side work")
	}
}
