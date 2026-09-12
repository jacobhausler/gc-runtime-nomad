package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// "How do I get an image?" was the most common dead end reported by outside
// reviewers of this pack, and the answer existed only as caveats scattered
// through the README. These guards hold the instructions to the LIVE code, so
// the page cannot describe a mechanism the pack no longer has.

// TestReadmeStatesHowToObtainAnImage pins the two-layer build. Either layer
// alone leaves an adopter with something that is not the agent image: the
// toolchain base has no city content, and city content composed onto nothing
// has no agent toolchain.
func TestReadmeStatesHowToObtainAnImage(t *testing.T) {
	doc := readReadme(t)
	for _, want := range []struct{ needle, why string }{
		{"toolchain/build-image.sh build", "the toolchain base layer is not named as a command to run"},
		{"gc build-image", "the city-content layer is not named"},
		{"linux/amd64", "the architecture constraint is not stated, and a darwin or arm64 build cannot ship"},
	} {
		if !strings.Contains(doc, want.needle) {
			t.Errorf("README.md does not mention %q: %s", want.needle, want.why)
		}
	}
}

// TestReadmePerRoleImageMatchesTheCode is the one that can rot into a lie.
//
// The README tells an adopter to put GC_NOMAD_AGENT_IMAGE in a provider's
// runtime_env so one role gets its own toolchain. That only works while the
// pack reads the image from the SESSION's env rather than from the process
// environment alone. If sessionAgentImage stops doing that, the documented
// per-role mechanism silently becomes a deployment-wide default — and the
// symptom is a lane quietly running the wrong agent, which is the hardest
// class of failure in this pack to see.
func TestReadmePerRoleImageMatchesTheCode(t *testing.T) {
	doc := readReadme(t)
	if !strings.Contains(doc, "runtime_env.nomad") {
		t.Error("README.md no longer shows the per-role runtime_env spelling")
	}

	// The live mechanism: the image is resolved out of the start payload's env.
	if got := sessionAgentImage(stageConfig{Env: map[string]string{envAgentImage: "registry.example/x:1"}}); got != "registry.example/x:1" {
		t.Fatalf("sessionAgentImage from the session env = %q, want the per-session value — "+
			"the README documents a per-role image that the code no longer honours", got)
	}
	// And empty must mean "inherit the deployment default", which is what makes
	// adding one role's image safe for every other lane.
	if got := sessionAgentImage(stageConfig{Env: map[string]string{}}); got != "" {
		t.Errorf("sessionAgentImage with no per-session value = %q, want empty so the parent "+
			"job's default applies; a non-empty answer would override every lane", got)
	}
}

func readReadme(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	return string(body)
}
