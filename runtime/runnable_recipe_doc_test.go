package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// EVERY RUNNABLE COMMAND IN THE INSTALL PATH MUST PRODUCE A WORKING RESULT.
//
// README.md's Part 2 carries a copy-pasteable shell block — the one thing a
// stranger actually runs. It used to invoke `build --published`, and a bullet
// fourteen lines BELOW the block explained that `--published` produces an image
// whose in-box gc cannot run this pack. So the only runnable recipe in the file
// was the broken one, with its own retraction underneath it.
//
// Two independent outside readers, each given the docs and asked for a
// go/no-go, both stopped at exactly that and reported it unprompted. Nothing in
// this suite could see it: every existing doc guard checks that a CLAIM is
// present or self-consistent, and none checks that a COMMAND we tell someone to
// run is one we would want them to run.
//
// publishedFlagIsNotForDeployableImages names the rule in one place.
const publishedFlag = "--published"

// TestNoRunnableBlockBuildsWithPublished asserts the flag never appears on a
// build command inside a fenced shell block in the adopter-facing docs. Prose
// may discuss it freely — that is how it gets explained — but a fenced block is
// a thing people paste.
func TestNoRunnableBlockBuildsWithPublished(t *testing.T) {
	for _, name := range []string{"README.md", "COMPATIBILITY.md"} {
		body, err := os.ReadFile(filepath.Join("..", name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for _, block := range fencedBlocks(string(body)) {
			for _, line := range strings.Split(block, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "#") {
					continue // a comment inside the block, not a command
				}
				if !strings.Contains(trimmed, publishedFlag) {
					continue
				}
				if !strings.Contains(trimmed, "build-image.sh") {
					continue
				}
				t.Errorf("%s has a pasteable command using %s:\n\n    %s\n\n"+
					"That flag builds against the upstream published gc, which carries none of "+
					"the required worker families, so the image cannot run this pack and a seat "+
					"in it drains no_work forever. A fenced block is what a stranger runs — an "+
					"explanation below it does not undo the paste. Use --gc-release=<commit>.",
					name, publishedFlag, trimmed)
			}
		}
	}
}

// And the tool must say so itself, at the moment it is asked. A doc-only rule
// would leave anyone who found the flag through --help with no warning at all.
func TestBuildImageRefusesToBeQuietAboutPublished(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "toolchain", "build-image.sh"))
	if err != nil {
		t.Fatalf("reading build-image.sh: %v", err)
	}
	src := string(body)
	if !strings.Contains(src, "CANNOT RUN THIS PACK") {
		t.Error("toolchain/build-image.sh never says the --published image cannot run this pack. " +
			"It previously said only that the image 'will NOT carry the live pin', which reads as " +
			"staleness — the failure is total and no configuration fixes it.")
	}
	// The understatement itself, as a phrase, must not come back on this path:
	// "may be older than the live pin" is what the check verb used to print.
	if strings.Contains(src, "which may be older than the live pin") {
		t.Error("toolchain/build-image.sh describes the published gc as merely 'older than the " +
			"live pin'. It is not old, it is incapable: it carries none of the worker families, " +
			"so a seat built on it cannot claim at all.")
	}
}

// fencedBlocks returns the contents of every ``` fenced block.
func fencedBlocks(doc string) []string {
	var out []string
	parts := strings.Split(doc, "```")
	// Odd indices are inside fences: text, FENCE, text, FENCE, ...
	for i := 1; i < len(parts); i += 2 {
		out = append(out, parts[i])
	}
	return out
}
