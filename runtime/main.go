// Command gc-runtime-nomad is a Runtime Provider Protocol (RPP) v0
// executable that runs Gas City sessions as dispatched Nomad jobs. It
// answers the `protocol` handshake, the pack-local `check` diagnostic, the
// four lifecycle ops from NRT-P1-03
// (start, stop, is-running, list-running), the provision/launch split plus
// warm relaunch from NRT-P1-08 (provision, exec, relaunch), and — from
// fnrt-szx — the driving verbs (nudge, peek, interrupt, send-keys,
// clear-scrollback) realized as tmux commands sent into the session's tmux
// pane over the same exec mechanism, and — from NRT-P1-06 — workspace and
// secret staging: `start`/`provision` read an optional stageConfig (JSON) on
// stdin and materialize its workspace files and secret env vars into the
// alloc over tar-over-exec-stdin, the latter routed by the envArgvSafe
// classification into $NOMAD_SECRETS_DIR rather than argv or the job spec
// (04 §5, E1a §4.5). Everything runs over Nomad job dispatch/deregister/
// blocking-reads and the alloc-exec WebSocket (04 §3/§4/§6/§7). Every other
// op besides the pack-local `check` diagnostic exits 2, the RPP
// forward-compatibility signal a caller treats as a no-op success.
//
// Calling convention (no shell wrapping — gc execs the binary directly):
//
//	gc-runtime-nomad <op> [<session-name>] [args...]
//
// Exit codes:
//
//	0  success
//	1  failure (message on stderr)
//	2  unknown / unimplemented op (forward-compatible no-op for the caller)
//
// exec is the one exception: its own exit code IS the remote command's exit
// code (04 §3 exec row, RPP-CONN-001), not this 0/1/2 convention — a
// transport-level exec failure (can't reach the alloc at all) still exits 1.
//
// Configuration comes from the environment:
//
//	GC_NOMAD_ADDR         required for lifecycle ops: absolute base URL of the Nomad API (e.g. http://127.0.0.1:4646)
//	GC_NOMAD_TOKEN        optional: ACL token sent as X-Nomad-Token
//	GC_NOMAD_CACERT       optional: CA certificate path for the Nomad API; alias of NOMAD_CACERT
//	GC_NOMAD_CLIENT_CERT  optional: client certificate path for Nomad mTLS; alias of NOMAD_CLIENT_CERT
//	GC_NOMAD_CLIENT_KEY   optional: client private-key path for Nomad mTLS; alias of NOMAD_CLIENT_KEY
//	GC_NOMAD_TLS_SERVER_NAME optional: TLS server name/SNI; alias of NOMAD_TLS_SERVER_NAME
//	GC_NOMAD_NAMESPACE    optional: Nomad namespace (default "default")
//	GC_NOMAD_NODE_POOL    optional: Nomad node pool for the parent job (default "" — Nomad's own "default" pool)
//	GC_NOMAD_PARENT_JOB   optional: the city's parameterized parent job ID (default "gc-sessions")
//	GC_NOMAD_SIDECAR_DIR  required for lifecycle ops: directory for the sidecar session->job-ID bindings (04 §1)
//	GC_NOMAD_EGRESS_DIR   optional: local directory for stop-path transcript/evidence egress (NRT-P1-07); unset disables egress
//	GC_NOMAD_FORBID_REGISTER  optional: "true" forbids ever registering the parent job (04 §4 lab ACL model) — start/provision fail closed instead of attempting a register call that needs the submit-job capability; unset means the default (registration allowed), and a value that is not a boolean is a config error that stops the op rather than reading as "allowed"
//	GC_NOMAD_LOG_SINK     optional: HTTP JSON-lines endpoint for the session task group's log-shipper task (fnrt-t4l.13); unset disables the task entirely — session logs are not shipped
//	GC_NOMAD_LOG_SINK_TOKEN_FILE  optional: in-box path the log-shipper task reads its Authorization bearer token's value from; unset means an unauthenticated sink
//	GC_NOMAD_LOG_LABELS   optional: "k=v,k=v" labels merged onto every shipped log line alongside the fixed session_name/alloc_id/node/runtime=nomad set
//	GC_NOMAD_LOG_SHIPPER_ARTIFACT  optional: URL or local path for the pinned Vector archive; unset uses the upstream release URL
//	GC_NOMAD_ADOPT_MAX_AGE  optional: Go duration (e.g. "30m", "2h") capping how old a live-but-unlaunched session box may be before provision/start reap it (stop + tombstone) and dispatch a fresh one instead of adopting it; unset means the 30m default, an unparseable value is a config error (cr-gdeav.6.1)
//	GC_NOMAD_AGENT_IMAGE  required: Docker image for the session agent task, normally produced by gc build-image; unset fails every lifecycle op closed at startup (cr-3vvnb5.2 deletes the legacy exec task and the GC_NOMAD_ARTIFACT_VOLUME host-volume shape that used to carry its toolchain). The prebaked image owns the agent toolchain: the launched pane runs gc's rendered launch command (cr-hcgaxt), GC_NOMAD_AGENT_LAUNCH_SCRIPT is not consulted, an empty rendered command fails the launch closed, and staged runtime secrets go to the mount point the box reports for $NOMAD_SECRETS_DIR; because no bootstrap survives to do it, the pack then owns the agent home: CODEX_HOME is exported inside that mount, overriding the orchestrator value the provider env staged, the Codex provider config is rendered from the seat's staged environment (GC_CODEX_MODEL_PROVIDER / GC_CODEX_MODEL_PROVIDER_BASE_URL), and auth arrives from the staged CODEX_AUTH_JSON (codex_home.go, cr-usnvaw); the box owns its filesystem too, so PATH, HOME, TMP/TMPDIR/TEMP, the XDG_* family, GC_HOME and any staged value naming the orchestrator's own HOME or TMPDIR are stripped from env.sh and the launch `-e` pairs rather than shadowed (image_env.go, cr-jr2kw7)
//	GC_NOMAD_DNS_SERVERS  optional: comma-separated resolver IP addresses the session group's bridge network writes into the alloc's /etc/resolv.conf (e.g. "192.168.0.202"); unset means Nomad declares no dns stanza and the alloc inherits the CLIENT HOST's resolv.conf — on a systemd-resolved host that is the unreachable 127.0.0.53 stub, so nothing in the box can resolve a name (cr-tzsxk.18)
//	GC_NOMAD_NODE_CONSTRAINT  optional: Nomad-syntax node constraints appended to the parent job, e.g. "${node.unique.name} != prod-nomad"; several separated by ";" (Nomad list operands already use commas). Unset adds only the pack's own linux-kernel fatal rule
//	GC_NOMAD_AGENT_LAUNCH_SCRIPT  optional: POSIX shell script the launch command execs inside the tmux pane after sourcing the staged environment (ops.go's buildLaunchCommand) — deployment-owned, so PATH prepends and HOME-style secrets-dir wiring for a specific agent (e.g. Codex) live in configuration, never in this pack; unset preserves the legacy bare-tmux placeholder session. IGNORED once GC_NOMAD_AGENT_IMAGE is set — the image owns the toolchain and image shape mounts no artifact volume for a bootstrap to live on (cr-hcgaxt); `check` warns when both are set
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

const (
	envAddr           = "GC_NOMAD_ADDR"
	envToken          = "GC_NOMAD_TOKEN"
	envNamespace      = "GC_NOMAD_NAMESPACE"
	envNodePool       = "GC_NOMAD_NODE_POOL"
	envParentJob      = "GC_NOMAD_PARENT_JOB"
	envSidecarDir     = "GC_NOMAD_SIDECAR_DIR"
	envEgressDir      = "GC_NOMAD_EGRESS_DIR"
	envForbidReg      = "GC_NOMAD_FORBID_REGISTER"
	envLogSink        = "GC_NOMAD_LOG_SINK"
	envLogTokFile     = "GC_NOMAD_LOG_SINK_TOKEN_FILE"
	envLogLabels      = "GC_NOMAD_LOG_LABELS"
	envLogArtifact    = "GC_NOMAD_LOG_SHIPPER_ARTIFACT"
	envAgentImage     = "GC_NOMAD_AGENT_IMAGE"
	envAgentLaunch    = "GC_NOMAD_AGENT_LAUNCH_SCRIPT"
	envAdoptMaxAge    = "GC_NOMAD_ADOPT_MAX_AGE"
	envDNSServers     = "GC_NOMAD_DNS_SERVERS"
	envNodeConstraint = "GC_NOMAD_NODE_CONSTRAINT"

	defaultParentJob = "gc-sessions"

	// defaultAdoptMaxAge is the adoption age limit when
	// GC_NOMAD_ADOPT_MAX_AGE is unset: well past any legitimate
	// provision-then-launch gap (staging + launch is seconds), and far short
	// of the hours-long orphan the canary reproduced.
	defaultAdoptMaxAge = 30 * time.Minute

	exitOK      = 0
	exitError   = 1
	exitUnknown = 2
)

// protocolHandshakeJSON is the response to the `protocol` op: the capability
// set gc actually consumes — proc.provision and proc.exec, both implemented
// (04 §3). Nothing else is declared, env.workspace included. gc does not
// ignore that code the way it ignores an unknown token: the capability runner
// probes every declared env.* entry (internal/runtime/runtimecapability/
// probes.go probeWorkspace execs `cat .gc-cb-sentinel` inside the session),
// and the conformance start payload carries only work_dir/command/env, so the
// declaration converts a SKIP row in `gc runtime conformance` into a FAIL the
// pack cannot serve — the work_dir sentinel is never materialized in the
// alloc. The workspace/secret staging itself stays (see README: that path is
// real, it is the claim that was invented); re-declare env.workspace only once
// the start wire materializes work_dir in the session. env.tooling/
// env.identity/env.transcripts stay undeclared — out of scope here.
const protocolHandshakeJSON = `{"version":0,"capabilities":["proc.provision","proc.exec"]}`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// lifecycleOps is the set of RPP operations this phase implements against
// the Nomad API. Ops outside it exit 2 without requiring any configuration
// — so a misconfigured or unset GC_NOMAD_ADDR never turns a handshake or an
// unimplemented-op probe into a spurious failure.
var lifecycleOps = map[string]bool{
	"start":            true,
	"stop":             true,
	"is-running":       true,
	"list-running":     true,
	"provision":        true,
	"relaunch":         true,
	"exec":             true,
	"nudge":            true,
	"peek":             true,
	"interrupt":        true,
	"send-keys":        true,
	"clear-scrollback": true,
}

// run dispatches one RPP operation. It is separated from main so tests can
// drive it with an in-memory stream and assert exit codes.
func run(args []string, stdin io.Reader, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "gc-runtime-nomad: missing operation")
		return exitError
	}
	op := args[0]
	rest := args[1:]

	if op == "protocol" {
		fmt.Fprintln(stdout, protocolHandshakeJSON)
		return exitOK
	}
	if op == "check" {
		if os.Getenv(envLogSink) == "" {
			fmt.Fprintln(stderr, "warning: session logs will not be shipped (GC_NOMAD_LOG_SINK unset)")
		} else if os.Getenv(envLogArtifact) == "" {
			fmt.Fprintln(stderr, "warning: log-shipper artifact source will use network default (GC_NOMAD_LOG_SHIPPER_ARTIFACT unset)")
		}
		if os.Getenv(envAgentImage) == "" {
			fmt.Fprintln(stderr, "warning: GC_NOMAD_AGENT_IMAGE unset — the session agent task is a Docker task and there is no other session shape, so every lifecycle op will fail closed (cr-3vvnb5.2)")
		} else if os.Getenv(envAgentLaunch) != "" {
			fmt.Fprintln(stderr, "warning: GC_NOMAD_AGENT_LAUNCH_SCRIPT is set but the docker-driver session task ignores it — the prebaked image owns the agent toolchain and its pane runs gc's rendered command (cr-hcgaxt)")
		}
		if os.Getenv(envDNSServers) == "" {
			fmt.Fprintln(stderr, "warning: session boxes will declare no dns stanza and inherit the client host's resolv.conf (GC_NOMAD_DNS_SERVERS unset) — on a systemd-resolved host that stub resolver is unreachable from a bridge alloc (cr-tzsxk.18)")
		}
		return exitOK
	}
	if !lifecycleOps[op] {
		return exitUnknown
	}

	l, err := newLifecycle()
	if err != nil {
		fmt.Fprintf(stderr, "gc-runtime-nomad: %v\n", err)
		return exitError
	}

	fail := func(err error) int {
		fmt.Fprintf(stderr, "gc-runtime-nomad %s: %v\n", op, err)
		return exitError
	}
	needName := func() (string, bool) {
		if len(rest) == 0 {
			fmt.Fprintf(stderr, "gc-runtime-nomad %s: missing session name\n", op)
			return "", false
		}
		return rest[0], true
	}

	ctx := context.Background()
	switch op {
	case "start":
		name, ok := needName()
		if !ok {
			return exitError
		}
		// The wire start config rides on stdin (staging.go's stageConfig,
		// NRT-P1-06) — empty stdin decodes to the zero value, so a caller
		// that never sends one gets exactly the pre-staging behavior.
		cfg, err := readStageConfig(stdin)
		if err != nil {
			return fail(err)
		}
		if err := l.opStartWithConfig(ctx, name, cfg); err != nil {
			return fail(err)
		}
		return exitOK

	case "stop":
		name, ok := needName()
		if !ok {
			return exitError
		}
		if err := l.opStop(ctx, name); err != nil {
			return fail(err)
		}
		return exitOK

	case "is-running":
		name, ok := needName()
		if !ok {
			return exitError
		}
		running, err := l.opIsRunning(ctx, name)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, boolText(running))
		return exitOK

	case "list-running":
		prefix := ""
		if len(rest) > 0 {
			prefix = rest[0]
		}
		names, err := l.opListRunning(ctx, prefix)
		if err != nil {
			return fail(err)
		}
		for _, n := range names {
			fmt.Fprintln(stdout, n)
		}
		return exitOK

	case "provision":
		name, ok := needName()
		if !ok {
			return exitError
		}
		cfg, err := readStageConfig(stdin)
		if err != nil {
			return fail(err)
		}
		if err := l.opProvisionWithConfig(ctx, name, cfg); err != nil {
			return fail(err)
		}
		return exitOK

	case "relaunch":
		name, ok := needName()
		if !ok {
			return exitError
		}
		if err := l.opRelaunch(ctx, name); err != nil {
			return fail(err)
		}
		return exitOK

	case "exec":
		name, ok := needName()
		if !ok {
			return exitError
		}
		// The command rides on stdin (docs/reference/exec-session-provider.md
		// exec row), not argv — a single shell command line, run inside the
		// box via /bin/sh -c so it gets the same quoting/pipeline semantics a
		// caller typing it into a shell would expect.
		cmdBytes, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "gc-runtime-nomad %s: reading command: %v\n", op, err)
			return exitError
		}
		if len(cmdBytes) == 0 {
			fmt.Fprintf(stderr, "gc-runtime-nomad %s: missing command\n", op)
			return exitError
		}
		exitCode, out, err := l.opExec(ctx, name, []string{"/bin/sh", "-c", string(cmdBytes)})
		if err != nil {
			return fail(err)
		}
		if _, err := stdout.Write(out); err != nil {
			fmt.Fprintf(stderr, "gc-runtime-nomad %s: writing stdout: %v\n", op, err)
			return exitError
		}
		// Unlike the other lifecycle ops, exec's own exit code IS the
		// remote command's exit code (04 §3 exec row: "op exit = command
		// exit", RPP-CONN-001) — not the 0/1/2 lifecycle-op convention.
		return exitCode

	case "nudge":
		name, ok := needName()
		if !ok {
			return exitError
		}
		// Text rides on stdin (mirrors exec's calling convention), not
		// argv — a nudge's turn-start text can be arbitrarily long/shaped.
		text, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "gc-runtime-nomad %s: reading nudge text: %v\n", op, err)
			return exitError
		}
		if err := l.opNudge(ctx, name, string(text)); err != nil {
			return fail(err)
		}
		return exitOK

	case "peek":
		name, ok := needName()
		if !ok {
			return exitError
		}
		lines := 0
		if len(rest) > 1 {
			n, err := strconv.Atoi(rest[1])
			if err != nil {
				fmt.Fprintf(stderr, "gc-runtime-nomad %s: invalid line count %q: %v\n", op, rest[1], err)
				return exitError
			}
			lines = n
		}
		out, err := l.opPeek(ctx, name, lines)
		if err != nil {
			return fail(err)
		}
		if _, err := stdout.Write(out); err != nil {
			fmt.Fprintf(stderr, "gc-runtime-nomad %s: writing stdout: %v\n", op, err)
			return exitError
		}
		return exitOK

	case "interrupt":
		name, ok := needName()
		if !ok {
			return exitError
		}
		if err := l.opInterrupt(ctx, name); err != nil {
			return fail(err)
		}
		return exitOK

	case "send-keys":
		name, ok := needName()
		if !ok {
			return exitError
		}
		if err := l.opSendKeys(ctx, name, rest[1:]); err != nil {
			return fail(err)
		}
		return exitOK

	case "clear-scrollback":
		name, ok := needName()
		if !ok {
			return exitError
		}
		if err := l.opClearScrollback(ctx, name); err != nil {
			return fail(err)
		}
		return exitOK

	default:
		// Unreachable: lifecycleOps gates entry to this switch. Kept as a
		// defensive exit-2 so an op added to lifecycleOps without a case
		// here degrades to the forward-compatible no-op rather than
		// panicking.
		return exitUnknown
	}
}

// newLifecycle builds the Nomad client + sidecar from the environment.
func newLifecycle() (*lifecycle, error) {
	c, err := newClient(os.Getenv(envAddr), os.Getenv(envToken), os.Getenv(envNamespace))
	if err != nil {
		return nil, err
	}
	sc, err := newSidecar(os.Getenv(envSidecarDir))
	if err != nil {
		return nil, err
	}
	parentJob := os.Getenv(envParentJob)
	if parentJob == "" {
		parentJob = defaultParentJob
	}
	forbidRegistration, err := parseForbidRegistration(os.Getenv(envForbidReg))
	if err != nil {
		return nil, err
	}
	adoptMaxAge, err := parseAdoptMaxAge(os.Getenv(envAdoptMaxAge))
	if err != nil {
		return nil, err
	}
	dnsServers, err := parseDNSServers(os.Getenv(envDNSServers))
	if err != nil {
		return nil, err
	}
	nodeConstraints, err := parseNodeConstraints(os.Getenv(envNodeConstraint))
	if err != nil {
		return nil, err
	}
	// The Docker task is the only session shape — cr-3vvnb5.2 removes the legacy
	// exec task and the host-volume artifact delivery that fed it — so an image
	// is required. Fail closed at the launcher, before any op can register a
	// parent or dispatch a box, rather than land a session in a task with no
	// toolchain to run an agent.
	agentImage := os.Getenv(envAgentImage)
	if agentImage == "" {
		return nil, fmt.Errorf("%s is required: the session agent task is a docker task and there is no other session shape to fall back to (cr-3vvnb5.2)", envAgentImage)
	}
	return &lifecycle{
		client:             c,
		sidecar:            sc,
		parentJobID:        parentJob,
		nodePool:           os.Getenv(envNodePool),
		egressDir:          os.Getenv(envEgressDir),
		forbidRegistration: forbidRegistration,
		logShipper: logShipperConfig{
			Sink:      os.Getenv(envLogSink),
			TokenFile: os.Getenv(envLogTokFile),
			Labels:    os.Getenv(envLogLabels),
			Artifact:  os.Getenv(envLogArtifact),
		},
		agentImage:  agentImage,
		adoptMaxAge: adoptMaxAge,
		placement: placementConfig{
			dnsServers:      dnsServers,
			nodeConstraints: nodeConstraints,
		},
		logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "gc-runtime-nomad: "+format+"\n", args...)
		},
	}, nil
}

// parseForbidRegistration reads GC_NOMAD_FORBID_REGISTER. Empty means the
// default — registration allowed — which has to be an explicit branch
// because strconv.ParseBool("") errors. A value that is present but not a
// boolean is a config error rather than a silent false: the flag exists so a
// token-limited deployment gets a start failure with a precise cause instead of
// a register probe across a capability boundary it cannot cross, and reading
// "yes" or "True " as false would flip that decision in exactly the direction
// the operator was guarding against. Same empty-means-default shape as
// parseAdoptMaxAge below.
func parseForbidRegistration(raw string) (bool, error) {
	if raw == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("parsing %s=%q: %w (want true or false; omit the variable to leave parent registration allowed)", envForbidReg, raw, err)
	}
	return b, nil
}

// parseAdoptMaxAge reads GC_NOMAD_ADOPT_MAX_AGE. Empty means the default; a
// value that is not a Go duration is a config error rather than a silent
// fallback, because the failure mode this knob exists to prevent is exactly
// "nobody noticed the limit was not being applied" — and a non-positive
// duration is rejected for the same reason (it would turn the limit off while
// looking like it was set).
func parseAdoptMaxAge(raw string) (time.Duration, error) {
	if raw == "" {
		return defaultAdoptMaxAge, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parsing %s=%q: %w (want a Go duration such as 30m)", envAdoptMaxAge, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s=%s disables the adoption age limit; omit the variable to get the %s default instead", envAdoptMaxAge, d, defaultAdoptMaxAge)
	}
	return d, nil
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
