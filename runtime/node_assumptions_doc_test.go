package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// COMPATIBILITY.md states what this pack assumes of a fleet, so an adopter can
// check those assumptions against their own cluster before installing. Every
// row is asserted against the LIVE jobspec here rather than pinned as prose: a
// compatibility statement that drifts from the code is worse than none, because
// it is the document an adopter trusts instead of reading the source.

func compatibilityDocBody(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "COMPATIBILITY.md"))
	if err != nil {
		t.Fatalf("reading COMPATIBILITY.md: %v", err)
	}
	return string(body)
}

// TestDocGPUClaimMatchesTheJobspec is the row most likely to become false and
// the most costly if it does. The doc tells an adopter that GPUs are NOT
// brokered — no device stanza, nothing reserved or fenced — so someone whose
// agents need a GPU knows the work is theirs. If a device stanza is ever added
// without updating the page, an adopter reads "nothing requests a GPU" while
// the pack requests one, and their dispatches pend forever on a fleet with no
// device plugin.
func TestDocGPUClaimMatchesTheJobspec(t *testing.T) {
	src, err := os.ReadFile("jobspec.go")
	if err != nil {
		t.Fatalf("reading jobspec.go: %v", err)
	}
	// api.TaskDevice / Devices is how a device request would appear.
	for _, marker := range []string{"Devices:", "TaskDevice", "device_ids"} {
		if strings.Contains(string(src), marker) {
			t.Errorf("jobspec.go now contains %q — the pack requests a device, but COMPATIBILITY.md "+
				"tells adopters GPUs are not brokered and nothing reserves one. Update the page.", marker)
		}
	}
	doc := compatibilityDocBody(t)
	if !strings.Contains(doc, "not brokered") {
		t.Error("COMPATIBILITY.md no longer states that GPUs are not brokered; that is an explicit " +
			"node assumption an adopter needs in order to size their own fleet")
	}
	if !strings.Contains(doc, "nomad-device-nvidia") {
		t.Error("COMPATIBILITY.md no longer names the device plugin it was exercised WITHOUT, " +
			"which is the concrete form of the claim")
	}
}

// TestDocDriverClaimsMatchTheJobspec pins the two drivers. A fleet without the
// docker driver cannot run this pack at all, and that is a thing to learn from
// a compatibility page rather than from a pending allocation.
func TestDocDriverClaimsMatchTheJobspec(t *testing.T) {
	src, err := os.ReadFile("jobspec.go")
	if err != nil {
		t.Fatalf("reading jobspec.go: %v", err)
	}
	code := string(src)
	doc := compatibilityDocBody(t)

	for _, d := range []string{"docker", "exec"} {
		if !strings.Contains(code, `Driver: "`+d+`"`) {
			t.Errorf("the jobspec no longer uses the %q driver, but COMPATIBILITY.md names it as a "+
				"fleet requirement", d)
		}
		if !strings.Contains(doc, "`"+d+"`") {
			t.Errorf("COMPATIBILITY.md does not name the %q driver as a fleet requirement", d)
		}
	}
}

// TestDocArchClaimMatchesTheConstraint keeps the amd64 row honest. It is the
// row that turns "my dispatches never place" into a one-line answer.
func TestDocArchClaimMatchesTheConstraint(t *testing.T) {
	src, err := os.ReadFile("jobspec.go")
	if err != nil {
		t.Fatalf("reading jobspec.go: %v", err)
	}
	if !strings.Contains(string(src), "amd64") {
		t.Fatal("the jobspec no longer mentions amd64, but COMPATIBILITY.md states it as a hard constraint")
	}
	doc := compatibilityDocBody(t)
	if !strings.Contains(doc, "amd64") || !strings.Contains(doc, "arm64") {
		t.Error("COMPATIBILITY.md must state BOTH that amd64 is required and that there is no arm64 " +
			"path — an adopter on a Graviton fleet needs to learn that from the page, not from a " +
			"dispatch that stays pending")
	}
}
