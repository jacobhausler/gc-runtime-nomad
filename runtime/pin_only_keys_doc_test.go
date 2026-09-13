package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PIN-ONLY CONFIG KEYS MUST NOT BE HANDED TO A STRANGER UNMARKED.
//
// The pack's own install path is read by someone whose gc is whatever `go
// install` or a release gave them. Two city-config keys the maintainer's
// estate uses every day do not exist on that gc at all — they are in the
// private pin lineage only — and TOML decoding drops an unknown key in
// silence. So an instruction naming one produces the darkest failure this pack
// has: the config is accepted, the key evaporates, and the lane comes up
// looking configured.
//
// This guard exists because README.md carried exactly that, in a bullet whose
// whole purpose was to tell an adopter what they DID NOT have to do: "No
// wrapper provider. A lane onboards by naming its runtime (`runtime_provider
// = "nomad"` on the agent, the nomad-only half in the provider's
// `runtime_env.nomad`)." Both keys are pin-only. The bullet removed one
// obligation and silently added two.
//
// Measured at the upstream base `6ba434271` (zero occurrences of either key),
// with the stock-gc alternative for each traced hop by hop in
// docs/PIN-DEPENDENCIES.md.
var pinOnlyConfigKeys = []string{
	"runtime_provider",
	"runtime_env",
}

// pinOnlyMarkers are the phrasings that count as marking a key pin-only. Any
// one of them in the same paragraph is enough — the requirement is that a
// reader cannot meet the key without being told, not that we police wording.
var pinOnlyMarkers = []string{
	"pin-only",
	"pin only",
	"not upstream",
	"only in the pin",
	"requires the pin",
	"PIN-DEPENDENCIES",
}

// TestReaderIsNeverHandedAPinOnlyKeyUnmarked asserts that every paragraph of
// the adopter-facing docs that names a pin-only key also says so.
//
// Paragraph scope, not line scope: the qualifier belongs near the key but a
// sentence can wrap, and a whole-document check would pass on a marker
// hundreds of lines away from the instruction.
func TestReaderIsNeverHandedAPinOnlyKeyUnmarked(t *testing.T) {
	for _, name := range []string{"README.md", "COMPATIBILITY.md"} {
		body, err := os.ReadFile(filepath.Join("..", name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for i, para := range strings.Split(string(body), "\n\n") {
			for _, key := range pinOnlyConfigKeys {
				if !strings.Contains(para, key) {
					continue
				}
				if paragraphMarksPinOnly(para) {
					continue
				}
				t.Errorf("%s paragraph %d names the pin-only config key %q with nothing marking "+
					"it pin-only. A stranger's gc drops that key in SILENCE, so the instruction "+
					"produces a lane that reads as configured and serves nothing. Either say it "+
					"is pin-only (one of %v) or give the stock-gc spelling.\n\n%s",
					name, i+1, key, pinOnlyMarkers, strings.TrimSpace(para))
			}
		}
	}
}

func paragraphMarksPinOnly(para string) bool {
	lower := strings.ToLower(para)
	for _, m := range pinOnlyMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// And the declaration itself must stay honest: a key listed here has to be
// documented as pin-only in the dependency page, or this list is a claim with
// no evidence behind it.
func TestEveryPinOnlyKeyIsDocumentedAsSuch(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "docs", "PIN-DEPENDENCIES.md"))
	if err != nil {
		t.Fatalf("reading PIN-DEPENDENCIES.md: %v", err)
	}
	doc := string(body)
	for _, key := range pinOnlyConfigKeys {
		if !strings.Contains(doc, key) {
			t.Errorf("docs/PIN-DEPENDENCIES.md never mentions %q, which this package declares "+
				"pin-only. The declaration has to be backed where an adopter reads it.", key)
		}
	}
}
