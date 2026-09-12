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

// TestCompatibilityDocStatesEveryFatalPlacementRule is the guard the first
// four were missing.
//
// An outside review put it exactly right: the original guards pinned the
// facts that do not rot (a protocol string, a pinned filename) and missed the
// ones that do. It was proven the same day — adding the ${attr.cpu.arch}
// fence made this document's arch section false the moment it landed, and not
// one of the four guards fired, because none of them read the constraint LIST.
// This one does: every fatal rule the jobspec sends must appear in the
// document, so adding a rule without saying so goes red.
func TestCompatibilityDocStatesEveryFatalPlacementRule(t *testing.T) {
	doc := readCompatibilityDoc(t)
	// No deployment fences: what is left is exactly the pack's own fatal rules.
	job := parentJobSpec("gc", "", "gc-sessions", logShipperConfig{}, "img", placementConfig{})
	if len(job.Constraints) == 0 {
		t.Fatal("the jobspec carries no constraints at all")
	}
	for _, c := range job.Constraints {
		if !strings.Contains(doc, c.LTarget) {
			t.Errorf("%s never mentions the fatal placement rule %s %s %s.\n"+
				"A placement rule decides where this pack can run at all; adding one\n"+
				"without stating it leaves adopters reading a supported-platform\n"+
				"section that is quietly false.", compatibilityDoc, c.LTarget, c.Operand, c.RTarget)
		}
	}
}

// TestCompatibilityDocDisclosesTheAgentSandboxPosture pins the disclosure of
// the agent config the pack hardcodes. approval_policy/sandbox_mode decide
// what an agent in a box may do, there is no deployment override, and the
// documentation did not mention either one until an outside review found them
// in the source. If the posture changes, or the disclosure is dropped, this
// goes red.
func TestCompatibilityDocDisclosesTheAgentSandboxPosture(t *testing.T) {
	rendered, err := os.ReadFile("codex_home.go")
	if err != nil {
		t.Fatalf("reading codex_home.go: %v", err)
	}
	doc := readCompatibilityDoc(t)
	for _, setting := range []string{"approval_policy", "sandbox_mode", "danger-full-access"} {
		if !strings.Contains(string(rendered), setting) {
			continue // the posture changed; nothing to disclose for this key
		}
		if !strings.Contains(doc, setting) {
			t.Errorf("the pack writes %q into every agent config and %s does not\n"+
				"disclose it", setting, compatibilityDoc)
		}
	}
}

// TestCompatibilityDocFakeBlindnessListIsStillTrue guards the claim that the
// offline fake models none of the jobspec surface. That claim is what tells a
// reader how far to trust a green receipt, and it is the kind of statement
// that rots in the safe-looking direction: someone teaches fakenomad about
// constraints, the document still says it is blind to them, and the receipt
// quietly looks weaker than it is — or, worse, another field is added to the
// fake and a reader trusts the whole list.
func TestCompatibilityDocFakeBlindnessListIsStillTrue(t *testing.T) {
	fake, err := os.ReadFile("../fakenomad/fakenomad.go")
	if err != nil {
		t.Fatalf("reading fakenomad: %v", err)
	}
	// Each token is something COMPATIBILITY.md says the fake does not model.
	for _, token := range []string{
		"ParameterizedJob", "MetaOptional", "Driver", "Constraints",
		"RestartPolicy", "Artifacts", "Lifecycle",
	} {
		if strings.Contains(string(fake), token) {
			t.Errorf("fakenomad now references %q, but %s still tells readers the fake\n"+
				"models none of the jobspec surface. Update the document: how much a\n"+
				"green conformance receipt proves depends on this list being true.",
				token, compatibilityDoc)
		}
	}
}
