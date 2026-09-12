package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gc-runtime-nomad/fakenomad"
)

func TestRun(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
		want  int
	}{
		{"missing op", nil, "", exitError},
		{"protocol", []string{"protocol"}, "", exitOK},
		{"unknown op", []string{"get-last-activity", "s1"}, "", exitUnknown},
		{"lifecycle op missing session name", []string{"start"}, "", exitError},
		{"provision missing session name", []string{"provision"}, "", exitError},
		{"relaunch missing session name", []string{"relaunch"}, "", exitError},
		{"exec missing session name", []string{"exec"}, "", exitError},
		{"exec missing command on stdin", []string{"exec", "s1"}, "", exitError},
		{"nudge missing session name", []string{"nudge"}, "", exitError},
		{"peek missing session name", []string{"peek"}, "", exitError},
		{"interrupt missing session name", []string{"interrupt"}, "", exitError},
		{"send-keys missing session name", []string{"send-keys"}, "", exitError},
		{"clear-scrollback missing session name", []string{"clear-scrollback"}, "", exitError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got := run(tc.args, strings.NewReader(tc.stdin), w, w)
			w.Close()
			if got != tc.want {
				t.Fatalf("run(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// TestProtocolHandshakeDeclaresProvisionAndExec confirms the protocol
// handshake advertises proc.provision and proc.exec now that both are
// implemented — a provision-capable pack must also declare exec (the
// controller launches the agent over exec after provision).
func TestProtocolHandshakeDeclaresProvisionAndExec(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := run([]string{"protocol"}, strings.NewReader(""), w, w); got != exitOK {
		t.Fatalf("run(protocol) = %d, want %d", got, exitOK)
	}
	w.Close()
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		t.Fatalf("reading protocol output: %v", err)
	}
	for _, capName := range []string{"proc.provision", "proc.exec"} {
		if !strings.Contains(line, capName) {
			t.Fatalf("protocol handshake %q missing capability %q", line, capName)
		}
	}
}

// testAgentImage is the session image every lifecycle test now supplies:
// GC_NOMAD_AGENT_IMAGE became required when the prebaked-image shape
// deleted the exec task, and fakenomad only records the jobspec it was sent — it never pulls.
const testAgentImage = "registry.example/gc-agent:test"

// imageModeStartPayload is the minimal start wire a launch needs now that
// the exec placeholder pane is gone: the Docker task runs the command gc
// rendered, and an empty one fails the launch closed. /bin/sh keeps the
// pane an interactive shell so the key-driving rows still have a pane to
// type into.
const imageModeStartPayload = `{"command":"/bin/sh"}`

func TestRunCheckWarnsWhenLogSinkUnset(t *testing.T) {
	t.Setenv(envAgentImage, testAgentImage)
	t.Setenv(envLogSink, "")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	got := run([]string{"check"}, strings.NewReader(""), w, w)
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	if got != exitOK {
		t.Fatalf("run(check) = %d, want %d", got, exitOK)
	}
	if want := "warning: session logs will not be shipped (GC_NOMAD_LOG_SINK unset)"; !strings.Contains(string(out), want) {
		t.Fatalf("run(check) output = %q, want warning %q", out, want)
	}

}

func TestRunCheckOmitsWarningsWhenOptionalInputsConfigured(t *testing.T) {
	t.Setenv(envAgentImage, testAgentImage)
	t.Setenv(envLogSink, "https://logs.example.internal/ingest")
	t.Setenv(envLogArtifact, "/var/lib/vector/vector.tar.gz")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	got := run([]string{"check"}, strings.NewReader(""), w, w)
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	if got != exitOK {
		t.Fatalf("run(check) = %d, want %d", got, exitOK)
	}
	if strings.Contains(string(out), "session logs will not be shipped") {
		t.Fatalf("run(check) output = %q, want no unset-sink warning", out)
	}

}

func TestRunCheckWarnsWhenLogShipperArtifactUnset(t *testing.T) {
	t.Setenv(envAgentImage, testAgentImage)
	t.Setenv(envLogSink, "http://127.0.0.1:18081/ingest")
	t.Setenv(envLogArtifact, "")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	got := run([]string{"check"}, strings.NewReader(""), w, w)
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	if got != exitOK {
		t.Fatalf("run(check) = %d, want %d", got, exitOK)
	}
	want := "warning: log-shipper artifact source will use network default (GC_NOMAD_LOG_SHIPPER_ARTIFACT unset)"
	if !strings.Contains(string(out), want) {
		t.Fatalf("run(check) output = %q, want warning %q", out, want)
	}
}

func TestNewLifecycleReadsLogShipperArtifact(t *testing.T) {
	t.Setenv(envAgentImage, testAgentImage)
	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)

	t.Setenv(envAddr, srv.URL())
	t.Setenv(envSidecarDir, t.TempDir())
	t.Setenv(envLogSink, "http://127.0.0.1:18081/ingest")
	const artifact = "/var/lib//vector-http/vector.tar.gz"
	t.Setenv(envLogArtifact, artifact)

	l, err := newLifecycle()
	if err != nil {
		t.Fatalf("newLifecycle: %v", err)
	}
	if l.logShipper.Artifact != artifact {
		t.Fatalf("logShipper.Artifact = %q, want %q", l.logShipper.Artifact, artifact)
	}
}

// TestNewLifecycleRequiresAgentImage is the fail-closed half of ONE SHAPE:
// with the legacy exec task and its artifact-volume delivery deleted there
// is no session shape without an image, so a run naming none must stop at
// the launcher rather than register a parent whose boxes have no toolchain
// to run an agent.
func TestNewLifecycleRequiresAgentImage(t *testing.T) {
	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)
	t.Setenv(envAddr, srv.URL())
	t.Setenv(envSidecarDir, t.TempDir())
	t.Setenv(envAgentImage, "")

	if _, err := newLifecycle(); err == nil || !strings.Contains(err.Error(), envAgentImage) {
		t.Fatalf("newLifecycle with %s unset: err = %v, want one naming %s", envAgentImage, err, envAgentImage)
	}
}

func TestNewLifecycleReadsAgentImage(t *testing.T) {
	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)

	t.Setenv(envAddr, srv.URL())
	t.Setenv(envSidecarDir, t.TempDir())
	const image = "registry.example/gc-agent:2026-09-08"
	t.Setenv(envAgentImage, image)

	l, err := newLifecycle()
	if err != nil {
		t.Fatalf("newLifecycle: %v", err)
	}
	if l.agentImage != image {
		t.Fatalf("agentImage = %q, want %q", l.agentImage, image)
	}
}

// TestRunExecReadsCommandFromStdin is a regression test for the exec op's
// calling convention: docs/reference/exec-session-provider.md carries the
// command on stdin (invocation is just `exec <name>`), not extra argv. A
// prior revision read it from argv instead, which silently broke
// RPP-CONN-001's exit-code fidelity check because no real command ever
// reached the alloc.
func TestRunExecReadsCommandFromStdin(t *testing.T) {
	t.Setenv(envAgentImage, testAgentImage)
	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)

	t.Setenv(envAddr, srv.URL())
	t.Setenv(envToken, "")
	t.Setenv(envNamespace, "")
	t.Setenv(envParentJob, "gc-sessions")
	t.Setenv(envSidecarDir, t.TempDir())

	const session = "sess-exec-cli"

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if got := run([]string{"start", session}, strings.NewReader(imageModeStartPayload), w, w); got != exitOK {
		w.Close()
		out, _ := io.ReadAll(r)
		t.Fatalf("start = %d, want %d (output: %s)", got, exitOK, out)
	}
	w.Close()
	r.Close()

	r, w, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	got := run([]string{"exec", session}, strings.NewReader("echo hi-from-stdin"), w, w)
	w.Close()
	out, _ := io.ReadAll(r)
	if got != exitOK {
		t.Fatalf("exec exit = %d, want %d (output: %s)", got, exitOK, out)
	}
	if !strings.Contains(string(out), "hi-from-stdin") {
		t.Fatalf("exec stdout = %q, want it to contain the echoed command output", out)
	}
}

// TestStartRegistersParentJobWithNamespaceAndNodePool is a regression test
// for a drift this pack once had: GC_NOMAD_NAMESPACE and GC_NOMAD_NODE_POOL must
// both reach the registered parent job, or every session silently lands in
// default/default instead of a lab cluster's named namespace/pool.
func TestStartRegistersParentJobWithNamespaceAndNodePool(t *testing.T) {
	t.Setenv(envAgentImage, testAgentImage)
	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)

	t.Setenv(envAddr, srv.URL())
	t.Setenv(envToken, "")
	t.Setenv(envNamespace, "gc-lab")
	t.Setenv(envNodePool, "lab-session")
	t.Setenv(envParentJob, "gc-sessions")
	t.Setenv(envSidecarDir, t.TempDir())

	const session = "sess-namespace-nodepool"

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if got := run([]string{"start", session}, strings.NewReader(imageModeStartPayload), w, w); got != exitOK {
		w.Close()
		out, _ := io.ReadAll(r)
		t.Fatalf("start = %d, want %d (output: %s)", got, exitOK, out)
	}
	w.Close()
	r.Close()

	resp, err := http.Get(srv.URL() + "/v1/job/gc-sessions?namespace=gc-lab")
	if err != nil {
		t.Fatalf("reading registered parent job: %v", err)
	}
	defer resp.Body.Close()
	var job struct {
		Namespace string
		NodePool  string
	}
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatalf("decoding parent job: %v", err)
	}
	if job.Namespace != "gc-lab" {
		t.Fatalf("parent job Namespace = %q, want %q", job.Namespace, "gc-lab")
	}
	if job.NodePool != "lab-session" {
		t.Fatalf("parent job NodePool = %q, want %q", job.NodePool, "lab-session")
	}
}

// TestRunDrivingVerbsOverTmux exercises nudge/peek/send-keys/interrupt/
// clear-scrollback end to end against a real tmux session inside a real
// alloc (fakenomad's exec actually forks the command): proof these driving
// verbs are genuine tmux commands sent over exec, not canned replies. Skips
// if tmux isn't on PATH rather than failing, keeping the hermetic offline
// suite green on a machine without it.
func TestRunDrivingVerbsOverTmux(t *testing.T) {
	t.Setenv(envAgentImage, testAgentImage)
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)

	t.Setenv(envAddr, srv.URL())
	t.Setenv(envToken, "")
	t.Setenv(envNamespace, "")
	t.Setenv(envParentJob, "gc-sessions")
	t.Setenv(envSidecarDir, t.TempDir())

	const session = "sess-driving-verbs"

	runOp := func(args []string, stdin string) (string, int) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		got := run(args, strings.NewReader(stdin), w, w)
		w.Close()
		out, _ := io.ReadAll(r)
		r.Close()
		return string(out), got
	}

	if out, got := runOp([]string{"start", session}, imageModeStartPayload); got != exitOK {
		t.Fatalf("start = %d, want %d (output: %s)", got, exitOK, out)
	}

	if out, got := runOp([]string{"send-keys", session, "echo GC_SEND_KEYS_OK", "Enter"}, ""); got != exitOK {
		t.Fatalf("send-keys = %d, want %d (output: %s)", got, exitOK, out)
	}
	waitForPeek(t, runOp, session, "GC_SEND_KEYS_OK")

	if out, got := runOp([]string{"nudge", session}, "echo GC_NUDGE_OK"); got != exitOK {
		t.Fatalf("nudge = %d, want %d (output: %s)", got, exitOK, out)
	}
	waitForPeek(t, runOp, session, "GC_NUDGE_OK")

	if out, got := runOp([]string{"interrupt", session}, ""); got != exitOK {
		t.Fatalf("interrupt = %d, want %d (output: %s)", got, exitOK, out)
	}

	if out, got := runOp([]string{"clear-scrollback", session}, ""); got != exitOK {
		t.Fatalf("clear-scrollback = %d, want %d (output: %s)", got, exitOK, out)
	}
}

// waitForPeek polls `peek <session>` until its output contains want or a
// short timeout elapses — send-keys/nudge queue keys into the pane
// asynchronously, so the shell needs a moment to execute and echo them.
func waitForPeek(t *testing.T, runOp func([]string, string) (string, int), session, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, got := runOp([]string{"peek", session}, "")
		if got != exitOK {
			t.Fatalf("peek = %d, want %d (output: %s)", got, exitOK, out)
		}
		last = out
		if strings.Contains(out, want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("peek never showed %q, last output: %s", want, last)
}

// TestRunLifecycleOpRequiresConfig confirms a lifecycle op fails loudly
// (not silently, not exit-2) when GC_NOMAD_ADDR/GC_NOMAD_SIDECAR_DIR are
// unset, so a misconfigured pack never masquerades as "op unimplemented".
func TestRunLifecycleOpRequiresConfig(t *testing.T) {
	t.Setenv(envAddr, "")
	t.Setenv(envSidecarDir, "")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := run([]string{"is-running", "s1"}, strings.NewReader(""), w, w)
	w.Close()
	if got != exitError {
		t.Fatalf("run(is-running) with no config = %d, want %d (exitError)", got, exitError)
	}
}

// TestParseAdoptMaxAge covers the GC_NOMAD_ADOPT_MAX_AGE config surface:
// unset means the shipped 30m default, a real duration is honored, and
// anything that would silently turn the limit off — a typo, or a
// non-positive value — is a config error rather than a fallback. The
// failure mode this knob exists to prevent is precisely "nobody noticed the
// limit was not being applied".
func TestParseAdoptMaxAge(t *testing.T) {
	if got, err := parseAdoptMaxAge(""); err != nil || got != defaultAdoptMaxAge {
		t.Fatalf(`parseAdoptMaxAge("") = (%v, %v), want the %v default`, got, err, defaultAdoptMaxAge)
	}
	if got, err := parseAdoptMaxAge("2h"); err != nil || got != 2*time.Hour {
		t.Fatalf(`parseAdoptMaxAge("2h") = (%v, %v), want 2h`, got, err)
	}
	for _, bad := range []string{"not-a-duration", "0s", "-5m"} {
		if got, err := parseAdoptMaxAge(bad); err == nil {
			t.Fatalf("parseAdoptMaxAge(%q) = %v, want an error: it would disable the limit", bad, got)
		}
	}
}

// TestRunCheckWarnsWhenDNSServersUnset keeps a measured failure mode from
// being silent again: with no resolver configured the pack still registers a
// working-looking box that cannot resolve a single name.
func TestRunCheckWarnsWhenDNSServersUnset(t *testing.T) {
	t.Setenv(envAgentImage, testAgentImage)
	t.Setenv(envDNSServers, "")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	got := run([]string{"check"}, strings.NewReader(""), w, w)
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	if got != exitOK {
		t.Fatalf("run(check) = %d, want %d", got, exitOK)
	}
	if !strings.Contains(string(out), "GC_NOMAD_DNS_SERVERS unset") {
		t.Fatalf("run(check) output = %q, want the unset-resolver warning", out)
	}
}

func TestRunCheckOmitsDNSWarningWhenResolversConfigured(t *testing.T) {
	t.Setenv(envAgentImage, testAgentImage)
	t.Setenv(envDNSServers, "192.168.0.202")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	got := run([]string{"check"}, strings.NewReader(""), w, w)
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	if got != exitOK {
		t.Fatalf("run(check) = %d, want %d", got, exitOK)
	}
	if strings.Contains(string(out), "GC_NOMAD_DNS_SERVERS unset") {
		t.Fatalf("run(check) output = %q, want no unset-resolver warning", out)
	}
}

// TestNewLifecycleReadsPlacementConfig proves both knobs reach the lifecycle
// that builds the parent job — the register path this bead exists to close.
func TestNewLifecycleReadsPlacementConfig(t *testing.T) {
	t.Setenv(envAgentImage, testAgentImage)
	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)

	t.Setenv(envAddr, srv.URL())
	t.Setenv(envSidecarDir, t.TempDir())
	t.Setenv(envDNSServers, "192.168.0.202")
	t.Setenv(envNodeConstraint, `${node.unique.name} != prod-nomad`)

	l, err := newLifecycle()
	if err != nil {
		t.Fatalf("newLifecycle: %v", err)
	}
	if len(l.placement.dnsServers) != 1 || l.placement.dnsServers[0] != "192.168.0.202" {
		t.Fatalf("placement.dnsServers = %v, want [192.168.0.202]", l.placement.dnsServers)
	}
	if len(l.placement.nodeConstraints) != 1 || l.placement.nodeConstraints[0].RTarget != "prod-nomad" {
		t.Fatalf("placement.nodeConstraints = %+v, want the prod-nomad fence", l.placement.nodeConstraints)
	}
}

// TestNewLifecycleRejectsBadPlacementConfig is the fail-closed half: a typo in
// either knob must stop the op with the variable named, not register a parent
// that silently drops the placement it was configured with.
func TestNewLifecycleRejectsBadPlacementConfig(t *testing.T) {
	t.Setenv(envAgentImage, testAgentImage)
	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)
	t.Setenv(envAddr, srv.URL())
	t.Setenv(envSidecarDir, t.TempDir())

	t.Setenv(envDNSServers, "resolver.example.internal")
	if _, err := newLifecycle(); err == nil || !strings.Contains(err.Error(), envDNSServers) {
		t.Fatalf("newLifecycle with a non-IP resolver: err = %v, want one naming %s", err, envDNSServers)
	}

	t.Setenv(envDNSServers, "")
	t.Setenv(envNodeConstraint, `${node.unique.name} ~= prod-nomad`)
	if _, err := newLifecycle(); err == nil || !strings.Contains(err.Error(), envNodeConstraint) {
		t.Fatalf("newLifecycle with a bad operand: err = %v, want one naming %s", err, envNodeConstraint)
	}
}

// TestProtocolHandshakeDeclaresOnlyNativeCapabilities is the exact-set guard
// the presence-only check above cannot be: gc probes every declared env.*
// capability (internal/runtime/runtimecapability probes.go/runner.go at the
// pin), so one invented token in this list turns a SKIP row of `gc runtime
// conformance` into a FAIL this pack cannot serve. A strings.Contains loop
// passes with the invented capability present AND absent, which is how
// env.workspace survived here while nothing materialized its sentinel.
func TestProtocolHandshakeDeclaresOnlyNativeCapabilities(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := run([]string{"protocol"}, strings.NewReader(""), w, w); got != exitOK {
		t.Fatalf("run(protocol) = %d, want %d", got, exitOK)
	}
	w.Close()
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		t.Fatalf("reading protocol output: %v", err)
	}
	var hs struct {
		Version      int      `json:"version"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(line), &hs); err != nil {
		t.Fatalf("decoding protocol handshake %q: %v", line, err)
	}
	want := []string{"proc.exec", "proc.provision"}
	got := append([]string(nil), hs.Capabilities...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("declared capabilities = %v, want exactly %v", hs.Capabilities, want)
	}
	for _, c := range hs.Capabilities {
		if strings.HasPrefix(c, "env.") {
			t.Fatalf("declared capability %q is an env.* code: gc probes those, and the start wire here does not materialize the fixture they check", c)
		}
	}
}

// TestNewLifecycleForbidRegistrationParsesFailClosed drives the
// GC_NOMAD_FORBID_REGISTER env parse itself. The ops-level fence cases set the
// lifecycle struct field and never reach newLifecycle's parse, which is exactly
// how a swallowed ParseBool error survived: unset must keep the default
// (strconv.ParseBool("") errors, so a naive fail-closed would stop every
// deployment that never sets the knob), and a value that is present but not a
// boolean must stop the op naming the key instead of reading as "registration
// allowed" — the inverse of what the operator who typed it meant.
func TestNewLifecycleForbidRegistrationParsesFailClosed(t *testing.T) {
	t.Setenv(envAgentImage, testAgentImage)
	srv := fakenomad.NewServer()
	t.Cleanup(srv.Close)
	t.Setenv(envAddr, srv.URL())
	t.Setenv(envSidecarDir, t.TempDir())

	t.Setenv(envForbidReg, "")
	l, err := newLifecycle()
	if err != nil {
		t.Fatalf("newLifecycle with %s unset: %v", envForbidReg, err)
	}
	if l.forbidRegistration {
		t.Fatalf("%s unset: forbidRegistration = true, want the default false", envForbidReg)
	}

	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"true", true}, {"1", true}, {"false", false}, {"0", false},
	} {
		t.Setenv(envForbidReg, tc.raw)
		l, err := newLifecycle()
		if err != nil {
			t.Fatalf("newLifecycle with %s=%q: %v", envForbidReg, tc.raw, err)
		}
		if l.forbidRegistration != tc.want {
			t.Fatalf("%s=%q: forbidRegistration = %v, want %v", envForbidReg, tc.raw, l.forbidRegistration, tc.want)
		}
	}

	for _, junk := range []string{"yes", "True ", "maybe", "2"} {
		t.Setenv(envForbidReg, junk)
		if _, err := newLifecycle(); err == nil || !strings.Contains(err.Error(), envForbidReg) {
			t.Fatalf("newLifecycle with %s=%q: err = %v, want one naming %s", envForbidReg, junk, err, envForbidReg)
		}
	}
}
