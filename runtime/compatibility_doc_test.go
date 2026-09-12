package main

import (
	"os"
	"strings"
	"testing"
)

// compatibilityDoc is the pack's compatibility statement (gate 8). It quotes
// concrete values out of this package — the handshake, the pinned log-shipper
// archive, the fatal placement constraint. A statement that silently rots is
// worse than none, so every value it hard-states is asserted here against the
// code it describes. When one of these goes red, fix the doc, do not fix the
// test: the doc is downstream of the code.
//
// Run these with -count=1. The doc lives OUTSIDE this package directory, so
// Go's test cache does not fingerprint it: edit COMPATIBILITY.md, re-run a
// bare `go test`, and you get a cached PASS over a doc that no longer
// matches. Measured 2026-09-12 — all four of these reported ok against a
// deliberately falsified doc until -count=1 was added. receipt.sh already
// passes -count=1 on every leg, so the pack's own receipt path is honest;
// this note is for the human running go test by hand.
const compatibilityDoc = "../COMPATIBILITY.md"

func readCompatibilityDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(compatibilityDoc)
	if err != nil {
		t.Fatalf("reading %s: %v", compatibilityDoc, err)
	}
	return string(b)
}

func TestCompatibilityDocQuotesTheLiveHandshake(t *testing.T) {
	doc := readCompatibilityDoc(t)
	if !strings.Contains(doc, protocolHandshakeJSON) {
		t.Errorf("%s does not quote the live protocol handshake.\nwant it to contain: %s",
			compatibilityDoc, protocolHandshakeJSON)
	}
}

func TestCompatibilityDocNamesThePinnedLogShipperArchive(t *testing.T) {
	doc := readCompatibilityDoc(t)
	if !strings.Contains(doc, vectorArchive) {
		t.Errorf("%s does not name the pinned log-shipper archive %q. The amd64-only\n"+
			"claim in that document rests on this exact filename.", compatibilityDoc, vectorArchive)
	}
}

func TestCompatibilityDocNamesTheFatalLinuxConstraint(t *testing.T) {
	doc := readCompatibilityDoc(t)
	job := parentJobSpec("gc", "", "gc-sessions", logShipperConfig{}, "img", placementConfig{})
	var found bool
	for _, c := range job.Constraints {
		if c.LTarget == "${attr.kernel.name}" && c.RTarget == "linux" {
			found = true
			if !strings.Contains(doc, c.LTarget+" "+c.Operand+" "+c.RTarget) {
				t.Errorf("%s does not state the fatal placement constraint as the jobspec\n"+
					"actually sends it: %s %s %s", compatibilityDoc, c.LTarget, c.Operand, c.RTarget)
			}
		}
	}
	if !found {
		t.Fatalf("the jobspec no longer carries a ${attr.kernel.name}=linux constraint; %s\n"+
			"claims Linux-only placement is enforced and would now be false", compatibilityDoc)
	}
}

func TestCompatibilityDocStatesTheModuleGoVersion(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	var goVersion string
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok {
			goVersion = strings.TrimSpace(rest)
			break
		}
	}
	if goVersion == "" {
		t.Fatal("go.mod carries no go directive")
	}
	doc := readCompatibilityDoc(t)
	if !strings.Contains(doc, goVersion) {
		t.Errorf("%s does not state the module's go directive %q", compatibilityDoc, goVersion)
	}
}
