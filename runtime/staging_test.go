package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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
func TestM3StagingReceiptWorkspaceProbe(t *testing.T) {
	l, _ := newTestLifecycle(t)
	ctx := context.Background()
	const session = "sess-stage-workspace"

	cfg := stageConfig{
		WorkDir: "workspace",
		Files: []stageFile{
			{Path: "hello.txt", Content: []byte("hello from staging\n")},
			{Path: "nested/deep.txt", Content: []byte("deep content\n")},
		},
	}
	if err := l.opStartWithConfig(ctx, session, cfg); err != nil {
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
	if err := l.opStartWithConfig(ctx, session, cfg); err != nil {
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
		Env: map[string]string{
			"GC_SESSION":     "safe-value-not-a-secret",
			"OPENAI_API_KEY": canary, // not on the argvSafe allow-list -> must route to secrets dir
		},
	}
	if err := l.opStartWithConfig(ctx, session, cfg); err != nil {
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
	cmd := buildLaunchCommand(map[string]string{
		"GC_SESSION":     "safe-value",
		"OPENAI_API_KEY": "must-not-appear-on-argv",
	})
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "GC_SESSION=safe-value") {
		t.Fatalf("launch command %v missing argvSafe env GC_SESSION", cmd)
	}
	if strings.Contains(joined, "must-not-appear-on-argv") {
		t.Fatalf("launch command %v carries a non-argvSafe env value", cmd)
	}
}

// TestBuildLaunchCommandSourcesStagedEnvironment ensures launch imports the
// values staged under NOMAD_SECRETS_DIR before creating tmux.
func TestBuildLaunchCommandSourcesStagedEnvironment(t *testing.T) {
	cmd := buildLaunchCommand(nil)
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, `NOMAD_SECRETS_DIR/env.sh`) {
		t.Fatalf("launch command %v does not source staged environment", cmd)
	}
}

// withAgentLaunchScript sets the package-level agentLaunchScript seam for
// the duration of one test and restores it afterward — cr-zwjsi's bounded
// launch contract is a global (buildLaunchCommand has no lifecycle handle),
// so tests must not leak configuration across each other.
func withAgentLaunchScript(t *testing.T, script string) {
	t.Helper()
	prev := agentLaunchScript
	agentLaunchScript = script
	t.Cleanup(func() { agentLaunchScript = prev })
}

// TestBuildLaunchCommandDefaultIsBareTmuxPlaceholder is the RED half of the
// bounded launch seam (cr-zwjsi): with GC_NOMAD_AGENT_LAUNCH_SCRIPT unset,
// the tmux pane's initial command must stay unspecified (the legacy
// placeholder shell) — this is both the generic-command-execution default
// and the seam's rollback path.
func TestBuildLaunchCommandDefaultIsBareTmuxPlaceholder(t *testing.T) {
	withAgentLaunchScript(t, "")
	cmd := buildLaunchCommand(nil)
	if got, want := cmd[len(cmd)-1], tmuxSessionName; got != want {
		t.Fatalf("launch command %v ends with %q, want tmux session name %q as the last argv (no configured pane command)", cmd, got, want)
	}
}

// TestBuildLaunchCommandRunsConfiguredAgentScript is the GREEN half: with
// GC_NOMAD_AGENT_LAUNCH_SCRIPT configured to a Codex-shaped bootstrap
// (PATH prepend to the pinned artifact toolchain plus CODEX_HOME inside
// $NOMAD_SECRETS_DIR, per cr-u4plc.1's wiring recipe), the tmux new-session
// call must carry that exact script as its initial pane command — the
// configured agent command line, not the bare placeholder.
func TestBuildLaunchCommandRunsConfiguredAgentScript(t *testing.T) {
	const script = `export PATH="/mnt/nomad/codex/bin/current:/mnt/nomad/gc/bin/current:$PATH"
export CODEX_HOME="$NOMAD_SECRETS_DIR/codex-home"
mkdir -p "$CODEX_HOME"
if [ -f "$NOMAD_SECRETS_DIR/CODEX_AUTH_JSON" ]; then
	install -m 600 "$NOMAD_SECRETS_DIR/CODEX_AUTH_JSON" "$CODEX_HOME/auth.json"
fi
exec codex`
	withAgentLaunchScript(t, script)

	cmd := buildLaunchCommand(map[string]string{"GC_SESSION": "safe-value"})
	joined := strings.Join(cmd, " ")

	if !strings.Contains(joined, "/mnt/nomad/codex/bin/current") {
		t.Fatalf("launch command %v does not resolve PATH to the pinned codex toolchain", cmd)
	}
	if !strings.Contains(joined, `CODEX_HOME="$NOMAD_SECRETS_DIR/codex-home"`) {
		t.Fatalf("launch command %v does not point CODEX_HOME inside the per-session secrets directory", cmd)
	}
	if strings.Contains(joined, "must-not-appear") {
		t.Fatalf("launch command %v unexpectedly carries credential-shaped content", cmd)
	}
	if got, want := cmd[len(cmd)-3:], []string{"/bin/sh", "-c", script}; !reflect.DeepEqual(got, want) {
		t.Fatalf("launch command %v does not exec the configured agent script as the tmux pane's initial command", cmd)
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
