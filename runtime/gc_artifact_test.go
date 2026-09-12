package main

import (
	"strings"
	"testing"
)

// THE DESIGN THIS TESTS, and why it is shaped this way.
//
// gc used to be baked into the agent image. This estate rolls its pin ~9 times
// a day and rebuilt the image by hand, so a box's gc was routinely several
// rolls behind the gc driving the pack (measured: live pin e78e884c1b, image
// carrying 3a3dda06a). Moving the URL into configuration would have swapped
// "rebuild an image every roll" for "edit a URL every roll" — the same human
// coupling at a lower price. So the URL is DERIVED from the gc that is actually
// running, and a pin roll propagates with nothing to edit.

func TestResolveGCArtifactDerivesFromTheCommit(t *testing.T) {
	got := resolveGCArtifact("https://example.test/rel/gc-{commit}/gc-linux-amd64", "e78e884c1b")
	if got.Source != "https://example.test/rel/gc-e78e884c1b/gc-linux-amd64" {
		t.Errorf("source = %q, want the commit substituted", got.Source)
	}
	// The checksum is a sidecar URL, not a literal: a literal would have to be
	// configured per pin, which puts the human back in the loop.
	if got.Checksum != "file:https://example.test/rel/gc-e78e884c1b/gc-linux-amd64.sha256" {
		t.Errorf("checksum = %q, want a derived file: sidecar reference", got.Checksum)
	}
	if !got.enabled() {
		t.Error("a fully resolved artifact must be enabled")
	}
}

// TestArtifactIsOffWithoutBothInputs keeps the rollout safe. An empty template
// must leave today's behaviour exactly alone, and an unreadable host commit
// must NOT produce a half-formed URL that fails placement on every box.
func TestArtifactIsOffWithoutBothInputs(t *testing.T) {
	for _, c := range [][2]string{
		{"", "e78e884c1b"},            // feature not configured
		{"https://x/{commit}/gc", ""}, // host commit unreadable
		{"", ""},
	} {
		got := resolveGCArtifact(c[0], c[1])
		if got.enabled() {
			t.Errorf("resolveGCArtifact(%q,%q) enabled; missing either input must mean OFF, "+
				"never a partially-substituted URL", c[0], c[1])
		}
		if got.Source != "" || got.Checksum != "" {
			t.Errorf("resolveGCArtifact(%q,%q) = %+v, want zero", c[0], c[1], got)
		}
	}
	// A source without a checksum is an unverified download and must not count.
	if (gcArtifactConfig{Source: "https://x/gc"}).enabled() {
		t.Error("a source with no checksum must not be enabled — that is an unverified download")
	}
	if (gcArtifactConfig{Checksum: "file:https://x/gc.sha256"}).enabled() {
		t.Error("a checksum with no source must not be enabled")
	}
}

// TestSessionTaskOmitsTheArtifactWhenOff is the no-regression guard: with the
// feature off the jobspec must be byte-identical to what it was, or enabling
// this would have silently changed every existing deployment's parent job.
func TestSessionTaskOmitsTheArtifactWhenOff(t *testing.T) {
	task := sessionTask("gc-agent:x", gcArtifactConfig{})
	for _, a := range task.Artifacts {
		if strings.Contains(a.RelativeDest, gcArtifactDest) {
			t.Error("a gc artifact was declared with the feature off")
		}
	}
	if p := task.Env[pathEnvName]; p != "" {
		t.Errorf("PATH was overridden with the feature off: %q — that would change the binary "+
			"resolution order in every existing box", p)
	}
}

// TestFetchedGCWinsThePathRace is the property most likely to regress
// silently, and the most expensive if it does.
//
// The image may still carry a gc — older images do, and an adopter's may too.
// If a baked gc wins the PATH race, the artifact downloads successfully on
// every box and the seat runs the stale binary anyway: the mechanism reports
// perfect health while delivering exactly the staleness it was built to
// remove. There is no failure to observe, which is why this is a test and not
// a comment.
func TestFetchedGCWinsThePathRace(t *testing.T) {
	art := resolveGCArtifact("https://example.test/gc-{commit}/gc", "abc123def")
	task := sessionTask("gc-agent:x", art)

	var found *nomadArtifact
	for i := range task.Artifacts {
		if strings.Contains(task.Artifacts[i].RelativeDest, gcArtifactDest) {
			found = &task.Artifacts[i]
		}
	}
	if found == nil {
		t.Fatal("no gc artifact declared with the feature on")
	}
	// Source and checksum must be INTERPOLATED from meta, not baked, or a
	// dispatch cannot override them per session.
	if !strings.Contains(found.GetterSource, "NOMAD_META_"+gcArtifactMetaKey) {
		t.Errorf("artifact source is not interpolated from meta: %q", found.GetterSource)
	}
	if c := found.GetterOptions["checksum"]; !strings.Contains(c, "NOMAD_META_"+gcArtifactSumMetaKey) {
		t.Errorf("artifact checksum is not interpolated from meta: %q", c)
	}

	// This assertion used to read task.Env["PATH"], require the fetched dir to
	// be its prefix, and — the line that hurts — require the string to contain
	// "${PATH}", CALLING THAT "preserves the image's own PATH". It measured the
	// source text and named it the effect. The text did contain ${PATH}; the
	// box's PATH was still truncated to one directory, because Nomad does not
	// interpolate ${PATH} in a task env. The test passed for as long as the
	// defect existed and went red only when the defect was removed.
	//
	// The property is real. It is just not decided here: PATH order is decided
	// by the in-box shell prelude, so that is where it is asserted
	// (TestPanePreludePrependsFetchedGCInAShell), and the task env is asserted
	// to stay OUT of it (TestJobspecNeverSetsPATH).
	if _, ok := task.Env[pathEnvName]; ok {
		t.Errorf("task env sets %s; the box's PATH cannot be composed from outside the box", pathEnvName)
	}
}

// TestArtifactKeysAreDispatchable pins the Nomad contract: a meta key absent
// from the parent's meta_optional is REFUSED outright on dispatch, so the keys
// must be declared there or the per-session override is impossible.
func TestArtifactKeysAreDispatchable(t *testing.T) {
	job := parentJobSpec("gc", "", "gc-sessions", logShipperConfig{}, "gc-agent:x",
		placementConfig{}, resolveGCArtifact("https://x/gc-{commit}/gc", "abc1234"))

	declared := map[string]bool{}
	for _, g := range job.TaskGroups {
		_ = g
	}
	if job.ParameterizedJob != nil {
		for _, k := range job.ParameterizedJob.MetaOptional {
			declared[k] = true
		}
	}
	for _, k := range []string{gcArtifactMetaKey, gcArtifactSumMetaKey} {
		if !declared[k] {
			t.Errorf("%q is not in meta_optional, so Nomad refuses a dispatch carrying it and the "+
				"per-session override cannot work", k)
		}
		if _, ok := job.Meta[k]; !ok {
			t.Errorf("%q has no job-level default; a key absent from job meta cannot be supplied "+
				"by a dispatch either", k)
		}
	}
}

// TestProductionPathPassesTheArtifact closes the hole the variadic opens.
//
// Making the parameter optional keeps 36 existing callers and every historical
// jobspec unchanged, which is the safe default — but it also means a caller can
// omit it and silently get the feature OFF. That is fine for a test and fatal
// for the one call site that matters, so the production path is asserted to
// pass it rather than trusted to.
func TestProductionPathPassesTheArtifact(t *testing.T) {
	src := readSourceFile(t, "ops.go")
	fn := src[strings.Index(src, "func (l *lifecycle) parentJobSpec()"):]
	if end := strings.Index(fn, "\n// gcArtifact resolves"); end > 0 {
		fn = fn[:end]
	}
	if !strings.Contains(fn, "l.gcArtifact()") {
		t.Error("the production parentJobSpec no longer passes the resolved artifact, so the " +
			"feature is off in the only place it is meant to be on — and the variadic signature " +
			"means this compiles and tests clean")
	}

	// And the resolver must read the LIVE host commit, not a cached one: the
	// pack is exec'd per operation and the pin can roll between two of them.
	res := src[strings.Index(src, "func (l *lifecycle) gcArtifact()"):]
	if end := strings.Index(res, "\n}"); end > 0 {
		res = res[:end+2]
	}
	if !strings.Contains(res, "hostGCCommit(") {
		t.Error("gcArtifact does not derive from hostGCCommit; a configured or captured commit is " +
			"the stale copy this whole design exists to abolish")
	}
	// The IDENTIFIER, not its value: the source reads
	// os.Getenv(envGCArtifactTemplate), so asserting the literal
	// "GC_NOMAD_GC_ARTIFACT_URL" fails against correct code. My first version
	// of this line did exactly that.
	if !strings.Contains(res, "os.Getenv(envGCArtifactTemplate)") {
		t.Error("gcArtifact does not read the artifact URL template from the environment")
	}
}
