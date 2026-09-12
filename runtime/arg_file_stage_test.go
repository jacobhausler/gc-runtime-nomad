package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WHY THE PACK STAGES THESE AT ALL.
//
// An agent's arguments are rendered on the ORCHESTRATOR, where the paths in
// them exist, and run in a BOX, where they do not. image_env.go already fixes
// that asymmetry for the environment by STRIPPING host paths; an argument
// cannot be stripped — dropping `-c model_catalog_json=...` changes what the
// agent does — so the other half of the fix is to make the path true inside
// the box.
//
// The orchestrator is the only side that can read the file, which is what puts
// this in the pack rather than in the image: an image cannot carry a
// deployment's agent configuration without being rebuilt per deployment, which
// is the staleness this pack spent a day removing.
//
// Bounded deliberately, because this copies bytes across a trust boundary:
// only paths that the operator's own arguments already name, only regular
// files, only readable ones, only under a size cap, and never anything under
// the secrets dir — a credential reaches the box through the secrets channel
// with its own 0600 discipline, not because it happened to appear in argv.

func TestArgFilesToStageSelectsOnlyNamedReadableFiles(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(small, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "adir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(big, make([]byte, argFileStageMaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "nope.json")

	cmd := strings.Join([]string{
		"codex exec",
		"-c model_catalog_json=" + small,
		"-c model=not-a-path",
		"--dir " + sub,
		"--big " + big,
		"--missing " + missing,
	}, " ")

	got := argFilesToStage(cmd)
	if len(got) != 1 {
		t.Fatalf("staged %d files, want exactly the one readable named file: %v", len(got), stagePaths(got))
	}
	if got[0].Path != small {
		t.Errorf("staged %q, want %q", got[0].Path, small)
	}
	if string(got[0].Content) != `{"a":1}` {
		t.Errorf("staged content %q", got[0].Content)
	}
	// 0600: the box has no business widening what the orchestrator restricted,
	// and this file was chosen by an argument, not vetted as public.
	if got[0].Mode != 0o600 {
		t.Errorf("staged mode %o, want 0600", got[0].Mode)
	}
}

// A directory, an unreadable file, an oversized file and a missing path must
// all be skipped SILENTLY here — the box's own missing-path report
// (host_path_args.go) is what tells the operator a path did not make it, and
// duplicating that as a stage-time failure would turn a diagnosable condition
// into a launch that never happens.
func TestArgFileStageNeverFailsTheLaunch(t *testing.T) {
	if got := argFilesToStage("agent --config /definitely/not/here.json"); len(got) != 0 {
		t.Fatalf("a missing path produced a staged file: %v", stagePaths(got))
	}
	if got := argFilesToStage(""); len(got) != 0 {
		t.Fatalf("an empty command produced staged files: %v", stagePaths(got))
	}
}

// The secrets boundary. A path under the user's gc secrets directory must
// never be copied into a box by virtue of appearing in an argument: that
// channel exists, has its own discipline, and this one would bypass it.
func TestArgFileStageRefusesSecretsPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir in this environment")
	}
	for _, p := range []string{
		filepath.Join(home, ".gc", "secrets.env"),
		filepath.Join(home, ".gc", "run", "nomad-acl"),
	} {
		if !argFileStageAllowed(p) {
			continue
		}
		t.Errorf("%q would be staged into a box from an argument; the secrets channel must be the only way in", p)
	}
}

func stagePaths(files []stageFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// The delivery must place the file at its ABSOLUTE path, which means it cannot
// ride the workspace tar (extracted at the work dir) or the secrets tar
// (extracted at the secrets dir). It gets its own exec, rooted at /.
func TestArgFileStageCommandPlacesTheAbsolutePath(t *testing.T) {
	cmd := argFileStageCommand("/orchestrator-home/.agent/cat.json")
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "mkdir -p") {
		t.Errorf("the parent directory is not created, so a path whose dir does not exist in the box fails: %q", joined)
	}
	// The path travels as an ARGUMENT, never interpolated into the shell text:
	// a path is operator-supplied data and a quoted path with a metacharacter
	// would otherwise be shell code.
	if strings.Contains(cmd[2], "/orchestrator-home") {
		t.Errorf("the path is interpolated into the shell program instead of passed as an argument: %q", cmd[2])
	}
	var found bool
	for _, a := range cmd {
		if a == "/orchestrator-home/.agent/cat.json" {
			found = true
		}
	}
	if !found {
		t.Errorf("the path is not passed as an argument at all: %v", cmd)
	}
}

// stage() runs only when stagesAnything says there is work. A deployment with
// no CopyFiles and no secret env would therefore skip argument-file staging
// entirely — and skip it SILENTLY, which is the failure shape this pack keeps
// finding in itself. The predicate has to know about this payload too.
func TestStagesAnythingCountsArgumentFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(p, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := stageConfig{Command: "agent -c model_catalog_json=" + p}
	if !stagesAnything(cfg) {
		t.Fatal("a session whose only payload is an argument-named file stages nothing, so the file never reaches the box and nothing says why")
	}
	// NOT asserted here: that a command without an absolute path makes
	// stagesAnything false. It was, and the assertion was wrong — the
	// predicate already returns true for "agent -c model=x" because
	// mergeLaunchOptionEnv derives env from the rendered command, which
	// predates this branch entirely. Testing that through stagesAnything
	// measured someone else's behaviour and called it mine.
	//
	// The property that IS mine: the branch contributes nothing when no
	// absolute path in the command names a readable file.
	if got := argFilesToStage("agent -c model=x"); len(got) != 0 {
		t.Errorf("a command naming no absolute path produced staged files: %v", stagePaths(got))
	}
}
