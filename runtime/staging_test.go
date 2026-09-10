package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gc-runtime-nomad/fakenomad"
)

// TestM3StagingReceiptWorkspaceProbe is the first half of the M3 staging
// receipt (fnrt-a8h acceptance, R2c-07): env.workspace probe pass. It
// starts a session with a stageConfig carrying two workspace files (one
// nested), then probes the alloc over exec to confirm both actually landed
// under WorkDir with their real content — proof the tar-over-exec-stdin
// channel (04 §5) genuinely materializes the workspace, not a canned reply.
//
// The fixture is real files on THIS host and a `copy_files` entry naming
// their directory, because that is the wire gc actually sends: gc's
// copyEntry (internal/runtime/exec/json.go:17-20) names orchestrator-side
// paths, never inline bodies (cr-u7b0s.40). The bodies are read here and
// archived onto the pack's one file transport, so the probe below is
// unchanged — same paths, same expected content.
func TestM3StagingReceiptWorkspaceProbe(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-stage-workspace"

	srcRoot := t.TempDir()
	writeStageSource(t, srcRoot, "hello.txt", "hello from staging\n", 0o644)
	writeStageSource(t, srcRoot, "nested/deep.txt", "deep content\n", 0o600)

	cfg := stageConfig{
		WorkDir:   "workspace",
		CopyFiles: []copyEntry{{Src: srcRoot}},
	}
	if err := l.opStartWithConfig(ctx, session, withPane(cfg)); err != nil {
		t.Fatalf("start with staging config: %v", err)
	}

	for path, want := range map[string]string{
		"workspace/hello.txt":       "hello from staging",
		"workspace/nested/deep.txt": "deep content",
	} {
		exitCode, out, err := l.opExec(ctx, session, []string{"cat", path})
		if err != nil {
			t.Fatalf("env.workspace probe: exec cat %s: %v", path, err)
		}
		if exitCode != 0 {
			t.Fatalf("env.workspace probe: cat %s exited %d: %s", path, exitCode, out)
		}
		if !strings.Contains(string(out), want) {
			t.Fatalf("env.workspace probe: cat %s = %q, want it to contain %q", path, out, want)
		}
	}
}

// TestStagingWritesSourceableEnvironmentScript proves that allocation
// environment values delivered through the secrets channel can be loaded by
// the launched process, including shell metacharacters that must remain data.
func TestStagingWritesSourceableEnvironmentScript(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-stage-env-script"

	cfg := stageConfig{Env: map[string]string{
		"GC_CITY_URL":     "https://relay.example:8443",
		"GC_CITY_CONTEXT": "westlands",
		"GC_SESSION_ID":   "worker-123",
		"ENV_WITH_QUOTES": "value with spaces; $(echo must-not-run) 'quoted'",
	}}
	if err := l.opStartWithConfig(ctx, session, withPane(cfg)); err != nil {
		t.Fatalf("start with staged environment: %v", err)
	}

	exitCode, out, err := l.opExec(ctx, session, []string{"/bin/sh", "-c", `set -eu; . "$NOMAD_SECRETS_DIR/env.sh"; printf '%s\n' "$GC_CITY_URL|$GC_CITY_CONTEXT|$GC_SESSION_ID|$ENV_WITH_QUOTES"`})
	if err != nil || exitCode != 0 {
		t.Fatalf("source staged environment: exit=%d err=%v out=%s", exitCode, err, out)
	}
	want := "https://relay.example:8443|westlands|worker-123|value with spaces; $(echo must-not-run) 'quoted'\n"
	if string(out) != want {
		t.Fatalf("sourced environment = %q, want %q", out, want)
	}
}

// writeStageSource writes one source file under root at the given mode,
// creating parents. The copy_files tests need files that genuinely exist on
// the orchestrator, because that is what gc's copy_files names.
func writeStageSource(t *testing.T, root, rel, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write source %s: %v", path, err)
	}
}

// TestCopyFilesToStageFilesReadsFilesAndTrees pins the copy_files -> stageFile
// translation gc's native wire needs and the pack's own tar transport
// already provides: rel_dst honoured, an empty rel_dst landing at the file's
// base name (gc documents that as "the workDir root"), a directory source
// keeping its tree, and the source's permission bits carried through.
func TestCopyFilesToStageFilesReadsFilesAndTrees(t *testing.T) {
	root := t.TempDir()
	writeStageSource(t, root, "prompt.txt", "do the work\n", 0o600)
	writeStageSource(t, root, "hooks/pre-start.sh", "#!/bin/sh\nexit 0\n", 0o755)
	writeStageSource(t, root, "hooks/skills/notes.md", "# notes\n", 0o644)

	files, err := copyFilesToStageFiles([]copyEntry{
		{Src: filepath.Join(root, "prompt.txt"), RelDst: "gc/prompt.txt"},
		{Src: filepath.Join(root, "hooks")},
		{Src: filepath.Join(root, "hooks"), RelDst: ".gc/skills"},
	})
	if err != nil {
		t.Fatalf("copyFilesToStageFiles: %v", err)
	}
	got := map[string]stageFile{}
	for _, f := range files {
		got[filepath.ToSlash(f.Path)] = f
	}
	for path, want := range map[string]struct {
		content string
		mode    int64
	}{
		"gc/prompt.txt":              {"do the work\n", 0o600},
		"pre-start.sh":               {"#!/bin/sh\nexit 0\n", 0o755},
		"skills/notes.md":            {"# notes\n", 0o644},
		".gc/skills/pre-start.sh":    {"#!/bin/sh\nexit 0\n", 0o755},
		".gc/skills/skills/notes.md": {"# notes\n", 0o644},
	} {
		f, ok := got[path]
		if !ok {
			t.Errorf("no staged file at %q; got %v", path, keysOf(got))
			continue
		}
		if string(f.Content) != want.content {
			t.Errorf("%s content = %q, want %q", path, f.Content, want.content)
		}
		if f.Mode != want.mode {
			t.Errorf("%s mode = %o, want %o", path, f.Mode, want.mode)
		}
	}
	if _, err := buildTar(files); err != nil {
		t.Errorf("translated files must archive on the pack's own transport: %v", err)
	}
}

func keysOf(m map[string]stageFile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestCopyFilesToStageFilesRefusesEscapingDestinations is the security half.
// rel_dst arrives from the wire, so an absolute or ".."-escaping destination
// must be refused by validateStagePath at translation time - the same rule
// that already governs the pack's secret and prime staging - rather than
// archived and honored by the extraction.
func TestCopyFilesToStageFilesRefusesEscapingDestinations(t *testing.T) {
	root := t.TempDir()
	writeStageSource(t, root, "x.txt", "x\n", 0o644)
	src := filepath.Join(root, "x.txt")
	for _, relDst := range []string{"../escape.txt", "nested/../../escape.txt", "/absolute/escape.txt", ""} {
		_, err := copyFilesToStageFiles([]copyEntry{{Src: src, RelDst: relDst}})
		if relDst == "" {
			if err != nil {
				t.Fatalf("empty rel_dst (the workDir root): %v", err)
			}
			continue
		}
		if !errors.Is(err, errStagePathInvalid) {
			t.Errorf("rel_dst %q: err = %v, want it to wrap errStagePathInvalid", relDst, err)
		}
	}
}

// TestCopyFilesToStageFilesSkipsNonRegularSources pins the other half of the
// conservative boundary: a symlink inside a copied tree is skipped, not
// followed, so a source directory cannot reach a file outside itself.
func TestCopyFilesToStageFilesSkipsNonRegularSources(t *testing.T) {
	root := t.TempDir()
	writeStageSource(t, root, "real.txt", "real\n", 0o644)
	elsewhere := t.TempDir()
	writeStageSource(t, elsewhere, "secret.txt", "secret\n", 0o600)
	if err := os.Symlink(filepath.Join(elsewhere, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	files, err := copyFilesToStageFiles([]copyEntry{{Src: root, RelDst: "ws"}})
	if err != nil {
		t.Fatalf("copyFilesToStageFiles: %v", err)
	}
	if len(files) != 1 || filepath.ToSlash(files[0].Path) != "ws/real.txt" {
		t.Fatalf("staged files = %+v, want only ws/real.txt", files)
	}
}

// TestCopyFilesToStageFilesFailsLoudlyOnUnreadableSource pins the
// error-not-warning rule: gc documents Provider.Start as handling the copy
// atomically, and an agent launched without a file its config promised looks
// healthy while it fails later.
func TestCopyFilesToStageFilesFailsLoudlyOnUnreadableSource(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.txt")
	if _, err := copyFilesToStageFiles([]copyEntry{{Src: missing, RelDst: "gone.txt"}}); err == nil {
		t.Fatal("missing src: err = nil, want a failure naming the source")
	} else if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the source %q", err, missing)
	}
	for _, src := range []string{"", "   "} {
		if _, err := copyFilesToStageFiles([]copyEntry{{Src: src, RelDst: "x"}}); err == nil {
			t.Errorf("src %q accepted, want a failure", src)
		}
	}
}

// TestStagesAnythingTreatsNativeCopyFilesAsStaging keeps the no-op guard
// honest after the wire key changed: a config carrying only copy_files still
// has a payload to materialize, and stage() makes an exec call that can fail
// partway - which is exactly what StagePending exists to record.
func TestStagesAnythingTreatsNativeCopyFilesAsStaging(t *testing.T) {
	if !stagesAnything(stageConfig{WorkDir: "workspace", CopyFiles: []copyEntry{{Src: "/tmp/x", RelDst: "x"}}}) {
		t.Error("stagesAnything ignored a copy_files payload")
	}
	if stagesAnything(stageConfig{WorkDir: "workspace"}) {
		t.Error("stagesAnything reported staging for a config with no payload")
	}
}

// withExtraSecretKeys sets the package-level extraSecretKeys seam for the
// duration of one test and restores it afterward, — stage() has no lifecycle handle to
// this config, so tests must not leak it across each other.
func withExtraSecretKeys(t *testing.T, keys []string) {
	t.Helper()
	prev := extraSecretKeys
	extraSecretKeys = keys
	t.Cleanup(func() { extraSecretKeys = prev })
}

// TestParseExtraSecretKeys is a focused unit check on
// GC_NOMAD_EXTRA_SECRET_KEYS's init-time parsing: comma-separated names,
// trimmed, blanks dropped.
func TestParseExtraSecretKeys(t *testing.T) {
	got := parseExtraSecretKeys(" CODEX_AUTH_JSON, OTHER_KEY ,,SPACED_OUT  ")
	want := []string{"CODEX_AUTH_JSON", "OTHER_KEY", "SPACED_OUT"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseExtraSecretKeys = %v, want %v", got, want)
	}
	if got := parseExtraSecretKeys(""); got != nil {
		t.Fatalf("parseExtraSecretKeys(\"\") = %v, want nil", got)
	}
}

// TestExtraSecretKeysStagedFromProcessEnv is the GREEN half of
// GC_NOMAD_EXTRA_SECRET_KEYS (cr-u4plc.2): a key named in that list and
// present in this process's own env (standing in for the supervisor's
// ~/.gc/secrets.env passthrough, which gc's own city.toml provider env map
// has no way to interpolate) lands in NOMAD_SECRETS_DIR exactly like an
// explicit cfg.Env secret would, even though the caller's stageConfig never
// mentioned it.
func TestExtraSecretKeysStagedFromProcessEnv(t *testing.T) {
	const secretValue = "codex-auth-json-bytes-do-not-leak"
	t.Setenv("CODEX_AUTH_JSON", secretValue)
	withExtraSecretKeys(t, []string{"CODEX_AUTH_JSON"})

	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-extra-secret"

	cfg := stageConfig{Env: map[string]string{"GC_SESSION": "safe-value"}}
	if err := l.opStartWithConfig(ctx, session, withPane(cfg)); err != nil {
		t.Fatalf("start with extra secret key configured: %v", err)
	}

	exitCode, out, err := l.opExec(ctx, session, []string{"/bin/sh", "-c", `cat "$NOMAD_SECRETS_DIR/CODEX_AUTH_JSON"`})
	if err != nil || exitCode != 0 {
		t.Fatalf("reading extra-secret file back: exit=%d err=%v out=%s", exitCode, err, out)
	}
	if strings.TrimSpace(string(out)) != secretValue {
		t.Fatalf("staged CODEX_AUTH_JSON = %q, want %q", out, secretValue)
	}

	// It must also never ride the launch command's argv, regardless of the
	// fact that it was sourced outside cfg.Env — an extra secret key is
	// always secret by declaration.
	cmd := buildLaunchCommandForPane(cfg.Env, "", testPaneCommand)
	if strings.Contains(strings.Join(cmd, " "), secretValue) {
		t.Fatalf("launch command %v carries the extra secret value", cmd)
	}
}

// TestExtraSecretKeysCallerEnvWins proves caller-supplied cfg.Env beats the
// process-env value on a key collision — the explicit judgment call this
// bead's diff must document.
func TestExtraSecretKeysCallerEnvWins(t *testing.T) {
	t.Setenv("CODEX_AUTH_JSON", "process-env-value-must-not-win")
	withExtraSecretKeys(t, []string{"CODEX_AUTH_JSON"})

	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-extra-secret-collision"

	cfg := stageConfig{Env: map[string]string{"CODEX_AUTH_JSON": "caller-supplied-value"}}
	if err := l.opStartWithConfig(ctx, session, withPane(cfg)); err != nil {
		t.Fatalf("start with colliding extra secret key: %v", err)
	}

	exitCode, out, err := l.opExec(ctx, session, []string{"/bin/sh", "-c", `cat "$NOMAD_SECRETS_DIR/CODEX_AUTH_JSON"`})
	if err != nil || exitCode != 0 {
		t.Fatalf("reading staged secret back: exit=%d err=%v out=%s", exitCode, err, out)
	}
	if strings.TrimSpace(string(out)) != "caller-supplied-value" {
		t.Fatalf("staged CODEX_AUTH_JSON = %q, want caller-supplied value to win", out)
	}
}

// TestExtraSecretKeysUnsetIsNoop is the RED-becomes-rollback half: with
// GC_NOMAD_EXTRA_SECRET_KEYS unset (the default, extraSecretKeys nil),
// staging behaves exactly as before this seam existed — a key present only
// in the process env and never named in cfg.Env is not staged at all: an unset
// seam is the no-op, and reverting needs no code change.
func TestExtraSecretKeysUnsetIsNoop(t *testing.T) {
	t.Setenv("UNCONFIGURED_SECRET", "should-never-be-staged")
	withExtraSecretKeys(t, nil)

	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-extra-secret-unset"

	cfg := stageConfig{Env: map[string]string{"GC_SESSION": "safe-value"}}
	if err := l.opStartWithConfig(ctx, session, withPane(cfg)); err != nil {
		t.Fatalf("start with no extra secret keys configured: %v", err)
	}

	exitCode, out, err := l.opExec(ctx, session, []string{"/bin/sh", "-c", `[ -f "$NOMAD_SECRETS_DIR/UNCONFIGURED_SECRET" ] && echo present || echo absent`})
	if err != nil || exitCode != 0 {
		t.Fatalf("probing for unconfigured secret file: exit=%d err=%v out=%s", exitCode, err, out)
	}
	if strings.TrimSpace(string(out)) != "absent" {
		t.Fatalf("UNCONFIGURED_SECRET was staged though GC_NOMAD_EXTRA_SECRET_KEYS never named it: %s", out)
	}
}

func TestMergeLaunchOptionEnvExtractsRenderedCodexDefaultsWithoutOverwritingEnv(t *testing.T) {
	env := map[string]string{
		"GC_MODEL":                 "explicit-model",
		"GC_CONTEXT_WINDOW_TOKENS": "350000",
	}

	got := mergeLaunchOptionEnv(env, `codex --model gpt-5.6-luna -c model_reasoning_effort=xhigh`)

	if got["GC_MODEL"] != "explicit-model" {
		t.Fatalf("GC_MODEL = %q, want explicit-model from explicit provider env", got["GC_MODEL"])
	}
	if got["GC_REASONING_EFFORT"] != "xhigh" {
		t.Fatalf("GC_REASONING_EFFORT = %q, want xhigh from rendered command", got["GC_REASONING_EFFORT"])
	}
	if got["GC_CONTEXT_WINDOW_TOKENS"] != "350000" {
		t.Fatalf("GC_CONTEXT_WINDOW_TOKENS = %q, want explicit context window preserved", got["GC_CONTEXT_WINDOW_TOKENS"])
	}
}

func TestMergeLaunchOptionEnvExtractsCodexConfigModel(t *testing.T) {
	got := mergeLaunchOptionEnv(nil, `codex exec --skip-git-repo-check -c model=gpt-5.6-luna -c model_reasoning_effort=xhigh`)

	if got["GC_MODEL"] != "gpt-5.6-luna" {
		t.Fatalf("GC_MODEL = %q, want gpt-5.6-luna from -c model=...", got["GC_MODEL"])
	}
	if got["GC_REASONING_EFFORT"] != "xhigh" {
		t.Fatalf("GC_REASONING_EFFORT = %q, want xhigh from -c model_reasoning_effort=...", got["GC_REASONING_EFFORT"])
	}
}

func TestStageEnvScriptIncludesRenderedLaunchOptions(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-rendered-launch-options"

	if err := l.opStartWithConfig(ctx, session, stageConfig{
		Command: `codex --model gpt-5.6-luna -c model_reasoning_effort=xhigh`,
	}); err != nil {
		t.Fatalf("start with rendered launch options: %v", err)
	}

	exitCode, out, err := l.opExec(ctx, session, []string{"/bin/sh", "-c", `cat "$NOMAD_SECRETS_DIR/env.sh"`})
	if err != nil || exitCode != 0 {
		t.Fatalf("read staged env.sh = (%d, %q, %v)", exitCode, out, err)
	}
	for _, want := range []string{
		"export GC_MODEL='gpt-5.6-luna'",
		"export GC_REASONING_EFFORT='xhigh'",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("env.sh = %q, missing %q", out, want)
		}
	}
}

// TestExtraSecretKeysNoCanaryLeak extends TestM3StagingReceiptNoCanaryLeak's
// style to the extra-secret-key path: the value never appears in any Nomad
// API request, sidecar file, or CLI stderr — only the declared KEY name
// (CODEX_AUTH_JSON) is referenced anywhere in this test, never the value.
func TestExtraSecretKeysNoCanaryLeak(t *testing.T) {
	const canary = "CANARY-EXTRA-SECRET-4c1a9f-do-not-leak"

	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)
	sidecarDir := t.TempDir()

	c, err := newClient(srv.URL(), "", "default")
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	rec := &requestRecorder{}
	c.http.Transport = rec
	sc, err := newSidecar(sidecarDir)
	if err != nil {
		t.Fatalf("newSidecar: %v", err)
	}
	l := &lifecycle{client: c, sidecar: sc, parentJobID: "gc-sessions"}
	ctx := context.Background()
	const session = "sess-extra-secret-canary"

	t.Setenv("CODEX_AUTH_JSON", canary)
	withExtraSecretKeys(t, []string{"CODEX_AUTH_JSON"})

	cfg := stageConfig{Env: map[string]string{"GC_SESSION": "safe-value-not-a-secret"}}
	if err := l.opStartWithConfig(ctx, session, withPane(cfg)); err != nil {
		t.Fatalf("start with extra secret key configured: %v", err)
	}

	// Positive control: it really landed.
	exitCode, out, err := l.opExec(ctx, session, []string{"/bin/sh", "-c", `cat "$NOMAD_SECRETS_DIR/CODEX_AUTH_JSON"`})
	if err != nil || exitCode != 0 {
		t.Fatalf("reading staged extra secret back: exit=%d err=%v out=%s", exitCode, err, out)
	}
	if !strings.Contains(string(out), canary) {
		t.Fatalf("staged extra secret did not land in NOMAD_SECRETS_DIR: %s", out)
	}

	if dump := rec.dump(); strings.Contains(dump, canary) {
		t.Fatalf("canary leaked into a Nomad API request (job spec/dispatch payload):\n%s", dump)
	}

	entries, err := os.ReadDir(sidecarDir)
	if err != nil {
		t.Fatalf("reading sidecar dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(sidecarDir, e.Name()))
		if err != nil {
			t.Fatalf("reading sidecar file %s: %v", e.Name(), err)
		}
		if bytes.Contains(data, []byte(canary)) {
			t.Fatalf("canary leaked into sidecar file %s:\n%s", e.Name(), data)
		}
	}
}

// TestM3StagingReceiptNoCanaryLeak is the second half of the M3 staging
// receipt: zero planted-canary hits across job specs, request logs, stderr,
// and sidecar files (R2c-07's single-receipt claim; the rollback trigger is
// 05 §6/R3c-06 — "secrets found in job spec, argv, logs, or provider error
// text"). It plants a canary value as a non-argvSafe env var (so it must
// route to the secrets dir, never argv/job-spec/sidecar), stages it, and
// then scans every channel the acceptance criteria name.
func TestM3StagingReceiptNoCanaryLeak(t *testing.T) {
	const canary = "CANARY-SECRET-9f3e2b7c-do-not-leak"

	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)
	sidecarDir := t.TempDir()

	c, err := newClient(srv.URL(), "", "default")
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	rec := &requestRecorder{}
	c.http.Transport = rec
	sc, err := newSidecar(sidecarDir)
	if err != nil {
		t.Fatalf("newSidecar: %v", err)
	}
	l := &lifecycle{client: c, sidecar: sc, parentJobID: "gc-sessions"}
	ctx := context.Background()
	const session = "sess-canary-direct"

	cfg := stageConfig{
		WorkDir: "workspace",
		// The Docker task runs the command gc rendered, so the wire has to carry
		// one now that the placeholder pane is deleted (cr-3vvnb5.2).
		Command: "/bin/sh",
		Env: map[string]string{
			"GC_SESSION":     "safe-value-not-a-secret",
			"OPENAI_API_KEY": canary, // not on the argvSafe allow-list -> must route to secrets dir
		},
	}
	if err := l.opStartWithConfig(ctx, session, withPane(cfg)); err != nil {
		t.Fatalf("start with staged secret: %v", err)
	}

	// Positive control: the secret actually landed in $NOMAD_SECRETS_DIR,
	// proving this is a real delivery-channel check, not a vacuous "we never
	// call anything with it" pass.
	exitCode, out, err := l.opExec(ctx, session, []string{"/bin/sh", "-c", `cat "$NOMAD_SECRETS_DIR/OPENAI_API_KEY"`})
	if err != nil || exitCode != 0 {
		t.Fatalf("reading staged secret back: exit=%d err=%v out=%s", exitCode, err, out)
	}
	if !strings.Contains(string(out), canary) {
		t.Fatalf("staged secret did not land in NOMAD_SECRETS_DIR: %s", out)
	}

	// Channel 1: job specs / request logs — every Nomad API request this
	// pack issued (job register carrying parentJobSpec, dispatch carrying
	// Meta, allocation/job reads). The alloc-exec WebSocket the secret
	// bytes actually traverse to reach the secrets dir does NOT go through
	// this HTTP transport (dialExecWS hijacks a raw TCP connection), so a
	// clean scan here specifically proves secrets never entered the
	// job-spec/API-request surface — the R9 residual (05 §7) is the WS
	// delivery channel itself, not this one.
	if dump := rec.dump(); strings.Contains(dump, canary) {
		t.Fatalf("canary leaked into a Nomad API request (job spec/dispatch payload):\n%s", dump)
	}

	// Channel 2: sidecar files — the binding this pack persists to disk
	// never carries Env at all (sidecar.go's binding type), but the receipt
	// checks the actual bytes on disk rather than trusting that by
	// inspection.
	entries, err := os.ReadDir(sidecarDir)
	if err != nil {
		t.Fatalf("reading sidecar dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(sidecarDir, e.Name()))
		if err != nil {
			t.Fatalf("reading sidecar file %s: %v", e.Name(), err)
		}
		if bytes.Contains(data, []byte(canary)) {
			t.Fatalf("canary leaked into sidecar file %s:\n%s", e.Name(), data)
		}
	}

	// Channel 3: stderr — drive the same scenario through the actual CLI
	// entrypoint (run(), main.go) against the same fake server/sidecar dir,
	// so the check covers the real gc-runtime-nomad<->caller boundary, not
	// just the internal lifecycle methods.
	t.Setenv(envAddr, srv.URL())
	t.Setenv(envToken, "")
	t.Setenv(envNamespace, "")
	t.Setenv(envParentJob, "gc-sessions")
	t.Setenv(envSidecarDir, sidecarDir)
	t.Setenv(envAgentImage, testAgentImage)
	const cliSession = "sess-canary-cli"

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshaling stage config: %v", err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	got := run([]string{"start", cliSession}, bytes.NewReader(cfgJSON), w, w)
	w.Close()
	stderrOut, _ := io.ReadAll(r)
	r.Close()
	if got != exitOK {
		t.Fatalf("run(start) = %d, want %d (output: %s)", got, exitOK, stderrOut)
	}
	if bytes.Contains(stderrOut, []byte(canary)) {
		t.Fatalf("canary leaked into CLI stderr:\n%s", stderrOut)
	}

	r, w, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	got = run([]string{"exec", cliSession}, strings.NewReader(`cat "$NOMAD_SECRETS_DIR/OPENAI_API_KEY"`), w, w)
	w.Close()
	execOut, _ := io.ReadAll(r)
	r.Close()
	if got != 0 {
		t.Fatalf("run(exec cat secret) = %d, want 0 (output: %s)", got, execOut)
	}
	if !strings.Contains(string(execOut), canary) {
		t.Fatalf("CLI-path secret did not land in NOMAD_SECRETS_DIR: %s", execOut)
	}
}

// TestBuildLaunchCommandClassifiesEnv is a focused unit check on
// envArgvSafe's consumer: an argvSafe key rides the launch command's argv,
// anything else (credential-shaped, per E1a §4.5) never does — deterministic
// and independent of exec/tmux, unlike the end-to-end canary receipt above.
func TestBuildLaunchCommandClassifiesEnv(t *testing.T) {
	cmd := buildLaunchCommandForPane(map[string]string{
		"GC_SESSION":     "safe-value",
		"OPENAI_API_KEY": "must-not-appear-on-argv",
	}, "", testPaneCommand)
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "GC_SESSION=safe-value") {
		t.Fatalf("launch command %v missing argvSafe env GC_SESSION", cmd)
	}
	if strings.Contains(joined, "must-not-appear-on-argv") {
		t.Fatalf("launch command %v carries a non-argvSafe env value", cmd)
	}
}

// TestRemoteStageStripsLocalCityStoreEnv prevents a remote allocation from
// inheriting the local city/store selectors that gc rejects beside
// GC_CITY_URL/GC_CITY_CONTEXT. Identity and session metadata remain part of
// the remote worker contract.
func TestRemoteStageStripsLocalCityStoreEnv(t *testing.T) {
	l, _ := newTestLifecycle(t)
	cfg := stageConfig{Env: map[string]string{
		"GC_CITY":               "local-city",
		"GC_CITY_PATH":          "/local/city",
		"GC_CITY_ROOT":          "/local/root",
		"GC_CITY_RUNTIME_DIR":   "/local/runtime",
		"GC_BEADS":              "file",
		"GC_BEADS_SCOPE_ROOT":   "/local/scope",
		"GC_CITY_NAME":          "remote-city",
		"GC_CITY_URL":           "https://relay.example:8443",
		"BEADS_ACTOR":           "mechanic-review-2-pool",
		"GC_SESSION_ID":         "session-123",
		"GC_SESSION_EPOCH":      "epoch-123",
		"UNRELATED_RUNTIME_ENV": "preserve-me",
	}}
	if err := l.opStartWithConfig(context.Background(), "sess-remote-local-env", withPane(cfg)); err != nil {
		t.Fatalf("start with remote environment: %v", err)
	}

	envSh, err := l.opExec2(context.Background(), "sess-remote-local-env", `cat "$NOMAD_SECRETS_DIR/env.sh"`)
	if err != nil {
		t.Fatalf("read staged environment: %v", err)
	}
	for _, key := range []string{
		"GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT", "GC_CITY_RUNTIME_DIR",
		"GC_BEADS", "GC_BEADS_SCOPE_ROOT",
	} {
		if strings.Contains(envSh, "export "+key+"=") {
			t.Fatalf("remote env.sh exports local key %s: %s", key, envSh)
		}
	}
	for _, want := range []string{
		"export GC_CITY_NAME='remote-city'",
		"export BEADS_ACTOR='mechanic-review-2-pool'",
		"export GC_SESSION_ID='session-123'",
		"export GC_SESSION_EPOCH='epoch-123'",
		"export UNRELATED_RUNTIME_ENV='preserve-me'",
	} {
		if !strings.Contains(envSh, want) {
			t.Fatalf("remote env.sh missing %q: %s", want, envSh)
		}
	}

	launch := strings.Join(buildLaunchCommandForPane(cfg.Env, "", testPaneCommand), " ")
	if strings.Contains(launch, "GC_CITY=local-city") {
		t.Fatalf("remote launch forwards local GC_CITY: %s", launch)
	}
}

// TestLocalStagePreservesLocalCityStoreEnv is the rollback guard: without a
// remote selector, local city/store routing remains unchanged.
func TestLocalStagePreservesLocalCityStoreEnv(t *testing.T) {
	l, _ := newTestLifecycle(t)
	cfg := stageConfig{Env: map[string]string{
		"GC_CITY":             "local-city",
		"GC_CITY_PATH":        "/local/city",
		"GC_CITY_ROOT":        "/local/root",
		"GC_CITY_RUNTIME_DIR": "/local/runtime",
		"GC_BEADS":            "file",
	}}
	if err := l.opStartWithConfig(context.Background(), "sess-local-env", withPane(cfg)); err != nil {
		t.Fatalf("start with local environment: %v", err)
	}

	envSh, err := l.opExec2(context.Background(), "sess-local-env", `cat "$NOMAD_SECRETS_DIR/env.sh"`)
	if err != nil {
		t.Fatalf("read staged environment: %v", err)
	}
	for _, want := range []string{
		"export GC_CITY='local-city'",
		"export GC_CITY_PATH='/local/city'",
		"export GC_CITY_ROOT='/local/root'",
		"export GC_CITY_RUNTIME_DIR='/local/runtime'",
		"export GC_BEADS='file'",
	} {
		if !strings.Contains(envSh, want) {
			t.Fatalf("local env.sh missing %q: %s", want, envSh)
		}
	}
}

// TestBuildLaunchCommandSourcesStagedEnvironment ensures launch imports the
// values staged under NOMAD_SECRETS_DIR before creating tmux.
func TestBuildLaunchCommandSourcesStagedEnvironment(t *testing.T) {
	cmd := buildLaunchCommandForPane(nil, "", testPaneCommand)
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, `NOMAD_SECRETS_DIR/env.sh`) {
		t.Fatalf("launch command %v does not source staged environment", cmd)
	}
}

// TestLaunchCommandPaneSourcesStagedEnv is the RED half of cr-gdeav.6.8: tmux
// gives a pane the SERVER's environment and never the CLIENT's, so sourcing
// the staged env.sh in the tmux client (launchEnvScript) delivers nothing to
// the agent — measured on tmux 3.7b for both verbs gc uses
// (ops/receipts/nrt-d3/2026-09-06-g3-attempt2-forensics.md §(2)). The staged
// environment must therefore be sourced inside the PANE, on both the
// new-session and the respawn-pane form, or a session launched over gc's
// separable transport starts an agent with no GC_CITY_URL, no session
// identity and no relay pointers (launched=True, attached=false — the G3
// attempt-2 shape). Unit tier: it inspects the built argv and must not exec
// tmux (the builder image has none).
func TestLaunchCommandPaneSourcesStagedEnv(t *testing.T) {
	const rendered = "exec codex"

	for _, tmuxArgs := range [][]string{
		{"new-session", "-d", "-s", tmuxSessionName},
		{"respawn-pane", "-k", "-t", tmuxSessionName},
	} {
		cmd := buildLaunchCommandForTmuxWithPaneCommand(nil, tmuxArgs, "", rendered)
		if len(cmd) < 3 || cmd[len(cmd)-3] != "/bin/sh" || cmd[len(cmd)-2] != "-c" {
			t.Fatalf("%s: launch command %v does not end in a /bin/sh -c pane body", tmuxArgs[0], cmd)
		}
		pane := cmd[len(cmd)-1]
		if !strings.Contains(pane, `. "$NOMAD_SECRETS_DIR/env.sh"`) {
			t.Errorf("%s: pane body does not source the staged environment: %q", tmuxArgs[0], pane)
		}
		if !strings.HasSuffix(pane, rendered) {
			t.Errorf("%s: pane body %q no longer ends with gc's rendered command", tmuxArgs[0], pane)
		}
	}
}

// requestRecorder wraps an http.RoundTripper and records every request's
// method, URL, and body — the "request log" channel the M3 canary receipt
// scans. Safe for concurrent use (http.Client may reuse a transport across
// goroutines even in single-threaded test code paths).
type requestRecorder struct {
	mu   sync.Mutex
	logs []string
}

func (r *requestRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err == nil {
			body = b
			req.Body = io.NopCloser(bytes.NewReader(b))
		}
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	r.mu.Lock()
	r.logs = append(r.logs, fmt.Sprintf("%s %s\n%s", req.Method, req.URL.String(), body))
	r.mu.Unlock()
	return resp, err
}

func (r *requestRecorder) dump() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.logs, "\n---\n")
}

// TestLaunchCommandPaneCdToStagedWorkDir is deliverable 3 of cr-gdeav.6.10:
// attempt 3's agent ran in "/" because nothing ever cd'd the pane, and gc's
// cfg.WorkDir is a supervisor-host path that usually does not exist inside the
// alloc. The pane must therefore try the staged work dir first and fall back to
// the per-alloc scratch directory in the SAME shell, so neither a rig checkout
// that is present nor one that is absent can fail the launch. Unit tier: it
// inspects the built argv and must not exec tmux.
func TestLaunchCommandPaneCdToStagedWorkDir(t *testing.T) {
	const rendered = "exec codex"

	cmd := buildLaunchCommandForPane(nil, "/tmp/staged-rig", rendered)
	body := cmd[len(cmd)-1]
	wantCd := `cd '/tmp/staged-rig' 2>/dev/null || `
	if !strings.Contains(body, wantCd) {
		t.Fatalf("pane body %q does not cd to the staged work dir first", body)
	}
	scratch := `cd "$NOMAD_ALLOC_DIR/work"`
	idxCd := strings.Index(body, wantCd)
	if !strings.Contains(body, scratch) {
		t.Fatalf("pane body %q has no scratch-directory fallback after the staged work dir", body)
	}
	if strings.Index(body, scratch) < idxCd {
		t.Fatalf("pane body %q tries the scratch directory before the staged work dir", body)
	}
	if !strings.HasSuffix(body, rendered) {
		t.Fatalf("pane body %q no longer ends with the rendered command the cd must precede", body)
	}
}

// TestLaunchCommandPaneCdQuotesWorkDir pins that a work dir is data, not
// pane-shell code.
func TestLaunchCommandPaneCdQuotesWorkDir(t *testing.T) {
	cmd := buildLaunchCommandForPane(nil, "/tmp/it's a trap;touch /tmp/pwned", `exec codex`)
	body := cmd[len(cmd)-1]
	if strings.Contains(body, "cd /tmp/it") {
		t.Fatalf("pane body %q interpolates the work dir unquoted", body)
	}
	if !strings.Contains(body, shellQuote("/tmp/it's a trap;touch /tmp/pwned")) {
		t.Fatalf("pane body %q does not carry the work dir single-quoted", body)
	}
}
