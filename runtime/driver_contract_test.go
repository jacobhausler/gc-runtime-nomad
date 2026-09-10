package main

// Contract tests for the docker-driver (prebaked gc build-image) session shape:
// the launch weld and the runtime-secret mount contract (cr-hcgaxt).
//
// There is one session shape and one pane body: the docker task's pane runs
// gc's OWN rendered command (F1 shipped the gc half; this is the pack half).
// The pack's deployment-bootstrap seam is deleted (cr-3vvnb5.3), so there is no
// second launch surface to drift into one another, and a box with no rendered
// command fails closed instead of reporting a launch that starts no agent.
// Runtime secrets are file-delivered into the box's own $NOMAD_SECRETS_DIR in
// both shapes; the image shape resolves that path from the box rather than
// assuming the exec driver's constant also names a container mount.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireTmuxForDriverTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
}

// TestImageModeLaunchRunsRenderedProviderCommand is the weld: an image-mode
// start runs what gc rendered, in the box, with the pack's staged-env and cwd
// preludes ahead of it — and records it so a later relaunch (which carries no
// command of its own) re-runs the same agent instead of failing closed.
func TestImageModeLaunchRunsRenderedProviderCommand(t *testing.T) {
	requireTmuxForDriverTest(t)
	l, _ := newTestLifecycle(t)
	l.agentImage = "localhost/gc-agent:test"
	ctx := context.Background()
	const session = "sess-image-weld"

	marker := filepath.Join(t.TempDir(), "agent-ran")
	rendered := "touch " + marker

	if err := l.opStartWithConfig(ctx, session, stageConfig{Command: rendered}); err != nil {
		t.Fatalf("image-mode start: %v", err)
	}
	if !waitForPath(marker, 10*time.Second) {
		t.Fatalf("image-mode pane never ran gc's rendered command (no %s)", marker)
	}

	b, ok, err := l.sidecar.load(session)
	if err != nil || !ok {
		t.Fatalf("sidecar.load = (%v, %v, %v)", b, ok, err)
	}
	if !b.Launched {
		t.Fatal("image-mode start did not record the launch")
	}
	if b.ProviderCommand != rendered {
		t.Fatalf("binding ProviderCommand = %q, want the rendered command %q", b.ProviderCommand, rendered)
	}

	// opRelaunch sends no command: without the recorded one an image-mode
	// relaunch would hit the fail-closed door and the box could never be
	// re-launched in place.
	if err := l.opRelaunch(ctx, session); err != nil {
		t.Fatalf("image-mode relaunch: %v", err)
	}
	after, _, err := l.sidecar.load(session)
	if err != nil {
		t.Fatalf("sidecar.load after relaunch: %v", err)
	}
	if after.ProviderCommand != rendered {
		t.Fatalf("relaunch dropped the recorded provider command: %q", after.ProviderCommand)
	}
}

// TestImageModeLaunchWithoutRenderedCommandFailsClosed: an agent-less pane that
// reports launched is the wedge this weld exists to prevent.
func TestImageModeLaunchWithoutRenderedCommandFailsClosed(t *testing.T) {
	l, _ := newTestLifecycle(t)
	l.agentImage = "localhost/gc-agent:test"
	const session = "sess-image-no-command"

	err := l.opStartWithConfig(context.Background(), session, stageConfig{})
	if err == nil {
		t.Fatal("image-mode start with no rendered command succeeded; it must fail closed")
	}
	if !strings.Contains(err.Error(), envAgentImage) {
		t.Fatalf("error does not name the configuration at fault (%s): %v", envAgentImage, err)
	}
	if b, ok, err := l.sidecar.load(session); err != nil {
		t.Fatalf("sidecar.load: %v", err)
	} else if ok && b.Launched {
		t.Fatal("a launch that never happened was recorded as launched")
	}
}

// TestImageModeSecretsStageIntoResolvedMount proves the mount contract
// end-to-end in the shape that resolves it: the staged env.sh is readable at
// the path the box itself reports, and gc's rendered command runs against it.
func TestImageModeSecretsStageIntoResolvedMount(t *testing.T) {
	requireTmuxForDriverTest(t)
	l, _ := newTestLifecycle(t)
	l.agentImage = "localhost/gc-agent:test"
	ctx := context.Background()
	const session = "sess-image-secrets"

	marker := filepath.Join(t.TempDir(), "env-reached-agent")
	cfg := stageConfig{
		Env:     map[string]string{"GC_CITY_TEST_SECRET": "value-that-must-not-reach-argv"},
		Command: `test -r "$NOMAD_SECRETS_DIR/env.sh" && touch ` + marker,
	}
	if err := l.opStartWithConfig(ctx, session, withPane(cfg)); err != nil {
		t.Fatalf("image-mode start with secrets: %v", err)
	}
	if !waitForPath(marker, 10*time.Second) {
		t.Fatal("staged env.sh never reached the image-mode pane through the resolved mount")
	}
}

func TestValidatedSecretsDirFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reported string
		wantErr  bool
	}{
		{"resolved path", "/secrets", false},
		{"pack-untypical mount point", "/nomad/secrets/alloc-7", false},
		{"empty", "", true},
		{"whitespace only", "   ", true},
		{"not a single path", "/secrets\n/etc", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validatedSecretsDir("sess-validate", tc.reported)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validatedSecretsDir(%q) = %q, want an error", tc.reported, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validatedSecretsDir(%q): %v", tc.reported, err)
			}
			if got != strings.TrimSpace(tc.reported) {
				t.Fatalf("validatedSecretsDir(%q) = %q", tc.reported, got)
			}
		})
	}
}

// TestBuildRemoteContextFileWritesResolvedMountPaths is the reason the mount is
// resolved at all: contexts.toml is literal TOML, so a guessed /secrets is a
// dangling credential path rather than a variable that resolves later.
func TestBuildRemoteContextFileWritesResolvedMountPaths(t *testing.T) {
	env := map[string]string{
		"GC_CITY_NAME":            "westlands",
		"GC_CITY_URL":             "https://city.invalid",
		"GC_CITY_CA_PEM":          "ca-bytes",
		"GC_CITY_CLIENT_CERT_PEM": "cert-bytes",
		"GC_CITY_CLIENT_KEY_PEM":  "key-bytes",
	}
	resolved := "/nomad/secrets"
	file := buildRemoteContextFile(env, resolved)
	if file == nil {
		t.Fatal("no contexts.toml for a complete remote context")
	}
	body := string(file.Content)
	for _, key := range []string{"GC_CITY_CA_PEM", "GC_CITY_CLIENT_CERT_PEM", "GC_CITY_CLIENT_KEY_PEM"} {
		if want := resolved + "/" + key; !strings.Contains(body, want) {
			t.Fatalf("contexts.toml did not name the resolved mount (%s):\n%s", want, body)
		}
	}
	if strings.Contains(body, `"`+nomadSecretsDirPath+"/GC_CITY") {
		t.Fatalf("contexts.toml kept the exec-driver constant instead of the resolved mount:\n%s", body)
	}
	for _, value := range []string{"ca-bytes", "cert-bytes", "key-bytes"} {
		if strings.Contains(body, value) {
			t.Fatalf("contexts.toml carried a secret VALUE (%s), not only its path", value)
		}
	}

	// Empty resolution means the legacy exec shape: the platform constant, unchanged.
	legacy := buildRemoteContextFile(env, "")
	if legacy == nil || !strings.Contains(string(legacy.Content), `"`+nomadSecretsDirPath+`/GC_CITY_CA_PEM"`) {
		t.Fatalf("legacy shape lost the platform constant:\n%s", string(legacy.Content))
	}
}

func waitForPath(path string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
