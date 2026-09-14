package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A published pack must be readable by someone who is not its author. Three
// independent adopter reviews of this repository — platform engineer, auditor,
// SRE, none with access to this estate — each independently reported the same
// class of defect: identifiers, hostnames, paths and jargon that resolve only
// on the author's machine, several of them load-bearing inside error messages
// an outsider would be reading at 3am.
//
// The cost is not cosmetic. When a reviewer asked "is this behaviour
// intended?", the answer was a document that does not ship. When one asked
// "which machine builds the image?", the answer was a hostname with no
// referent. An artifact whose reasoning terminates in unresolvable pointers
// has not actually been written down.
//
// This guard is the mechanism that keeps it out, because a one-time purge
// without one is a purge that lasts until the next commit.

// leakageClass is one family of estate-private token.
type leakageClass struct {
	name string
	pat  *regexp.Regexp
	why  string
}

func leakageClasses() []leakageClass {
	return []leakageClass{
		{
			"bead identifier",
			regexp.MustCompile(`(?i)\b(cr-[a-z0-9]+(\.[0-9]+)*|gcg--[a-z0-9]+|fnrt-[a-z0-9.]+|nrt-[a-z0-9._-]+)\b`),
			"a ticket id in this estate's tracker. The nrt- arm was WIDENED after an outside install review found 13 survivors the first pattern missed — it required NRT-P<n>-<n>, so lowercase nrt-p1-90, nrt-d3 and NRT-OBS-001 all slipped through, including into CONFORMANCE-RECEIPT.json, a SHIPPED artifact. A guard with a pattern narrower than the thing it guards reports clean and means nothing; an outsider cannot resolve it, and several " +
				"sit inside error messages and doc claims as the only justification given",
		},
		{
			"private hostname",
			regexp.MustCompile(`\b(rhuidean|haus-gpu|gaidin|callindor|haus-ubuntu)\b`),
			"a machine on the author's network; naming it in a build receipt or an error " +
				"tells an adopter to go somewhere that does not exist for them. The " +
				"`haus-ubuntu` arm was added 2026-09-14 after a shipped runner log carried " +
				"`node=haus-ubuntu`: this class had every builder node the author had named " +
				"by hand and not the one a machine wrote into the capture on its own.",
		},
		{
			"private filesystem path",
			regexp.MustCompile(`/Users/home/|/mnt/nomad/`),
			"an absolute path on the author's machine. A script or test that hard-codes one " +
				"cannot run anywhere else",
		},
		{
			"unshipped normative reference",
			regexp.MustCompile(`research/outputs/`),
			"cited as the authority for behaviour, and not in this repository — the spec is " +
				"on someone else's disk",
		},
		{
			"private design-doc citation",
			regexp.MustCompile("\\b0[0-9] \u00a7|\\bE1a \u00a7|\\bR[12][abc]-[0-9]+|\\be[12]a-[a-z-]+"),
			"a section reference into the maintainer's private design documents (\"04 \u00a75\", " +
				"\"R2b-03\"). This class was MISSED by the first pass of this guard and found by " +
				"hand afterwards, in files no purge had been pointed at — which is the argument " +
				"for the guard existing rather than for trusting a sweep",
		},
		{
			"unfilled placeholder",
			regexp.MustCompile(`to-be-assigned|ratified-by: pending`),
			"a template field that was never filled in; it reads as an unfinished artifact",
		},
	}
}

// leakageScanRoots are the shipped surfaces. The guard's own file is excluded:
// it necessarily contains every pattern it searches for.
func leakageScanRoots() []string {
	return []string{"..", "."}
}

var leakageSkipNames = map[string]bool{
	"estate_leakage_test.go": true,
}

func shouldScanForLeakage(path string) bool {
	if strings.Contains(path, "/.git/") {
		return false
	}
	if leakageSkipNames[filepath.Base(path)] {
		return false
	}
	// The extension allowlist is a allowlist over FILE TYPES, and it has to be
	// as wide as the surface that actually ships. It did not cover `.log` until
	// 2026-09-14, which is how a runner log with the author's checkout path and
	// builder node in it passed this guard and shipped inside the conformance
	// receipt's own citation: the pack started shipping captured logs when
	// RECEIPT_RUNNER_LOG became a receipt field, and nothing widened this
	// function to follow it. A runner log is the single most estate-shaped
	// artifact a pack can ship — it is machine-written, so nobody read it before
	// it was committed — so a `.log` that reaches this walk is read in full, not
	// skipped for size. Redact a capture (`repo=`, `node=` and friends) BEFORE
	// it is committed; a log admitted here is a shipped file.
	switch filepath.Ext(path) {
	case ".go", ".md", ".sh", ".toml", ".json", ".lock", ".yaml", ".yml", ".log":
		return true
	}
	return filepath.Base(path) == "Dockerfile"
}

func TestNoEstateLeakageInShippedFiles(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	classes := leakageClasses()

	// file -> class -> count, so the failure is a work list rather than 478
	// identical lines.
	type hit struct {
		class string
		count int
	}
	findings := map[string][]hit{}
	total := 0

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !shouldScanForLeakage(path) {
			if info != nil && info.IsDir() && info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for _, c := range classes {
			if n := len(c.pat.FindAllIndex(body, -1)); n > 0 {
				findings[rel] = append(findings[rel], hit{c.name, n})
				total += n
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking the repo: %v", walkErr)
	}

	if total == 0 {
		return
	}

	t.Errorf("%d estate-private tokens remain across %d files. An adopter cannot resolve any "+
		"of them, and three independent outside reviews each reported this class.",
		total, len(findings))
	for _, c := range classes {
		t.Logf("  class %q: %s", c.name, c.why)
	}
	for file, hits := range findings {
		parts := make([]string, 0, len(hits))
		for _, h := range hits {
			parts = append(parts, h.class+"="+itoa(h.count))
		}
		t.Logf("  %s: %s", file, strings.Join(parts, " "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
