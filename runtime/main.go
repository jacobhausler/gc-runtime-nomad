// Command gc-runtime-nomad is a Runtime Provider Protocol (RPP) v0
// executable that runs Gas City sessions as dispatched Nomad jobs. It
// answers the `protocol` handshake, the pack-local `check` diagnostic, the
// four lifecycle ops
// (start, stop, is-running, list-running), the provision/launch split plus
// warm relaunch (provision, exec, relaunch), the driving verbs (nudge, peek,
// interrupt, send-keys, clear-scrollback) realized as tmux commands sent into
// the session's tmux pane over the same exec mechanism, and workspace and
// secret staging: `start`/`provision` read an optional stageConfig (JSON) on
// stdin and materialize its workspace files and secret env vars into the
// alloc over tar-over-exec-stdin, the latter routed by the envArgvSafe
// classification into $NOMAD_SECRETS_DIR rather than argv or the job spec.
// Everything runs over Nomad job dispatch/deregister/
// blocking-reads and the alloc-exec WebSocket. Every other
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
// code, not this 0/1/2 convention — a
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
//	GC_NOMAD_SIDECAR_DIR  required for lifecycle ops: directory for the sidecar session->job-ID bindings
//	GC_NOMAD_EGRESS_DIR   optional: local directory for stop-path transcript/evidence egress; unset disables egress
//	GC_NOMAD_FORBID_REGISTER  optional: "true" forbids ever registering the parent job (the lab ACL model) — start/provision fail closed instead of attempting a register call that needs the submit-job capability; unset means the default (registration allowed), and a value that is not a boolean is a config error that stops the op rather than reading as "allowed"
//	GC_NOMAD_LOG_SINK     optional: HTTP JSON-lines endpoint for the session task group's log-shipper task; unset disables the task entirely — session logs are not shipped
//	GC_NOMAD_LOG_SINK_TOKEN_FILE  optional: in-box path the log-shipper task reads its Authorization bearer token's value from; unset means an unauthenticated sink
//	GC_NOMAD_LOG_LABELS   optional: "k=v,k=v" labels merged onto every shipped log line alongside the fixed session_name/alloc_id/node/runtime=nomad set
//	GC_NOMAD_LOG_SHIPPER_ARTIFACT  optional: URL or local path for the pinned Vector archive; unset uses the upstream release URL
//	GC_NOMAD_ADOPT_MAX_AGE  optional: Go duration (e.g. "30m", "2h") capping how old a live-but-unlaunched session box may be before provision/start reap it (stop + tombstone) and dispatch a fresh one instead of adopting it; unset means the 30m default, an unparseable value is a config error
//	GC_NOMAD_AGENT_IMAGE  required: Docker image for the session agent task, normally produced by gc build-image; unset fails every lifecycle op closed at startup (the legacy exec task, and the GC_NOMAD_ARTIFACT_VOLUME host-volume shape that used to carry its toolchain, have been deleted). The prebaked image owns the agent toolchain: the launched pane runs gc's rendered launch command, there is no deployment-bootstrap seam left to consult, and an empty rendered command fails the launch closed, and staged runtime secrets go to the mount point the box reports for $NOMAD_SECRETS_DIR; because no bootstrap survives to do it, the pack then owns the agent home: CODEX_HOME is exported inside that mount, overriding the orchestrator value the provider env staged, the Codex provider config is rendered from the seat's staged environment (GC_CODEX_MODEL_PROVIDER / GC_CODEX_MODEL_PROVIDER_BASE_URL), and auth arrives from the staged CODEX_AUTH_JSON (codex_home.go); the box owns its filesystem too, so PATH, HOME, TMP/TMPDIR/TEMP, the XDG_* family, GC_HOME and any staged value naming the orchestrator's own HOME or TMPDIR are stripped from env.sh and the launch `-e` pairs rather than shadowed (image_env.go)
//	GC_NOMAD_DNS_SERVERS  optional: comma-separated resolver IP addresses the session group's bridge network writes into the alloc's /etc/resolv.conf (e.g. "192.168.0.202"); unset means Nomad declares no dns stanza and the alloc inherits the CLIENT HOST's resolv.conf — on a systemd-resolved host that is the unreachable 127.0.0.53 stub, so nothing in the box can resolve a name
//	GC_NOMAD_NODE_CONSTRAINT  optional: Nomad-syntax node constraints appended to the parent job, e.g. "${node.unique.name} != prod-nomad"; several separated by ";" (Nomad list operands already use commas). Unset adds only the pack's own linux-kernel fatal rule
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	envAddr       = "GC_NOMAD_ADDR"
	envToken      = "GC_NOMAD_TOKEN"
	envNamespace  = "GC_NOMAD_NAMESPACE"
	envNodePool   = "GC_NOMAD_NODE_POOL"
	envParentJob  = "GC_NOMAD_PARENT_JOB"
	envSidecarDir = "GC_NOMAD_SIDECAR_DIR"
	envEgressDir  = "GC_NOMAD_EGRESS_DIR"
	envForbidReg  = "GC_NOMAD_FORBID_REGISTER"
	envLogSink    = "GC_NOMAD_LOG_SINK"
	envLogTokFile = "GC_NOMAD_LOG_SINK_TOKEN_FILE"
	// envACLFile names a FILE holding the Nomad ACL token, and its name is
	// chosen the way it is on purpose. See resolveNomadToken.
	envACLFile        = "GC_NOMAD_ACL_FILE"
	envLogLabels      = "GC_NOMAD_LOG_LABELS"
	envLogArtifact    = "GC_NOMAD_LOG_SHIPPER_ARTIFACT"
	envAgentImage     = "GC_NOMAD_AGENT_IMAGE"
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
// set gc actually consumes — proc.provision and proc.exec, both implemented.
// Nothing else is declared, env.workspace included. gc does not
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

// mintsABox is the subset of lifecycleOps that brings a new allocation into
// existence. The gc capability gate applies to exactly these: a box that can
// never claim should not be created, but a box that already exists must stay
// stoppable and readable.
var mintsABox = map[string]bool{
	"start":     true,
	"provision": true,
	"relaunch":  true,
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
	// Recorded for the diagnostics sink: every line it writes is attributed to
	// the operation that produced it, which is the first thing an operator
	// needs and the first thing a bare exit code loses.
	currentOp = op

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
		if w := registrylessImageWarning(os.Getenv(envAgentImage)); w != "" {
			fmt.Fprintln(stderr, w)
		}
		if os.Getenv(envAgentImage) == "" {
			fmt.Fprintln(stderr, "warning: GC_NOMAD_AGENT_IMAGE unset — the session agent task is a Docker task and there is no other session shape, so every lifecycle op will fail closed")
		}
		if skipVerifyRequested() {
			fmt.Fprintln(stderr, tlsVerificationDisabledWarning)
		}
		if code := checkAllocListCapability(stderr); code != exitOK {
			return code
		}
		// Where this pack's own diagnostics live. An operator who needs
		// diagnostics.log needs the directory first, and that directory is a
		// deployment choice carried in the supervisor's environment — which
		// is exactly where someone debugging a failure cannot look, because
		// it is also where the tokens are. So the runtime says it out loud
		// rather than making anyone go find it.
		if dir := strings.TrimSpace(os.Getenv(envSidecarDir)); dir == "" {
			fmt.Fprintf(stderr, "warning: %s is unset, so there is no sidecar directory and no "+
				"durable %s — this pack's account of a failure will exist only on a stderr gc "+
				"discards\n", envSidecarDir, diagnosticsFile)
		} else {
			path := filepath.Join(dir, diagnosticsFile)
			if _, err := os.Stat(path); err == nil {
				fmt.Fprintf(stderr, "diagnostics: %s\n", path)
			} else {
				fmt.Fprintf(stderr, "diagnostics: %s (nothing recorded yet)\n", path)
			}
		}
		switch gcWorkerSurfaceForCheck(stderr) {
		case gcWorkerSurfaceMissing:
			fmt.Fprintf(stderr, "FAIL: %v\n", gcMissingWorkerSurfaceError())
			return exitError
		case gcWorkerSurfaceAbsent:
			fmt.Fprintf(stderr, "FAIL: %v\n", gcAbsentWorkerSurfaceError())
			return exitError
		case gcWorkerSurfaceUnknown:
			fmt.Fprintln(stderr, "warning: the `gc` on PATH answered the capability probe in a "+
				"shape this pack does not recognise, so whether it carries the "+
				"remote-worker-lifecycle family is undetermined. Proceeding: an unrecognised "+
				"answer is not evidence of a missing family. (An ABSENT gc is a different "+
				"case and is refused, not warned.) See docs/PIN-DEPENDENCIES.md.")
		}
		if os.Getenv(envDNSServers) == "" {
			fmt.Fprintln(stderr, "warning: session boxes will declare no dns stanza and inherit the client host's resolv.conf (GC_NOMAD_DNS_SERVERS unset) — on a systemd-resolved host that stub resolver is unreachable from a bridge alloc")
		}
		return exitOK
	}
	if !lifecycleOps[op] {
		return exitUnknown
	}

	// The capability gate runs BEFORE the lifecycle is built, and only for the
	// ops that MINT a box.
	//
	// Before, because "the gc driving this pack cannot claim work" is a deeper
	// and more actionable fault than "GC_NOMAD_ADDR is unset", and an operator
	// who fixes the address first only gets to the real problem later, by way
	// of a box that looks healthy and does nothing.
	//
	// Only box-minting ops, because stop/is-running/list-running must keep
	// working against boxes that already exist. Refusing those would strand
	// allocations on the very gc that cannot drive them — converting a startup
	// refusal into an orphaned-box problem, which is strictly worse.
	if mintsABox[op] && !capabilityGateSkipped(stderr) {
		// Two refusals, not one. Missing = the wrong gc build; Absent = no gc
		// answered at all. Both are fail-closed on the ops that mint a box;
		// Unknown still proceeds, because an unrecognised answer is not
		// evidence of a missing family.
		switch probeGCWorkerSurface(context.Background()) {
		case gcWorkerSurfaceMissing:
			fmt.Fprintf(stderr, "gc-runtime-nomad %s: %v\n", op, gcMissingWorkerSurfaceError())
			return exitError
		case gcWorkerSurfaceAbsent:
			fmt.Fprintf(stderr, "gc-runtime-nomad %s: %v\n", op, gcAbsentWorkerSurfaceError())
			return exitError
		}
	}

	l, err := newLifecycle()
	if err != nil {
		fmt.Fprintf(stderr, "gc-runtime-nomad: %v\n", err)
		return exitError
	}

	fail := func(err error) int {
		fmt.Fprintf(stderr, "gc-runtime-nomad %s: %v\n", op, err)
		// THE LINE THAT MATTERS. gc's exec transport turns a nonzero exit into
		// "tmux exited N" and discards this stderr, so without the durable
		// copy the operator is left with an exit code and no cause — which is
		// exactly the shape this pack exists to stop producing, produced by
		// the pack itself.
		l.diagnostics.write(op, "FAILED: "+err.Error())
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
		// The wire start config rides on stdin (staging.go's stageConfig)
		// — empty stdin decodes to the zero value, so a caller
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
		// remote command's exit code ("op exit = command exit") — not the
		// 0/1/2 lifecycle-op convention.
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
	token, err := resolveNomadToken()
	if err != nil {
		return nil, err
	}
	c, err := newClient(os.Getenv(envAddr), token, os.Getenv(envNamespace))
	if err != nil {
		return nil, err
	}
	diag := newDiagnosticsSink(os.Getenv(envSidecarDir))
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
	// The Docker task is the only session shape — the legacy exec task and the
	// host-volume artifact delivery that fed it are gone — so an image
	// is required. Fail closed at the launcher, before any op can register a
	// parent or dispatch a box, rather than land a session in a task with no
	// toolchain to run an agent.
	agentImage := os.Getenv(envAgentImage)
	if agentImage == "" {
		return nil, fmt.Errorf("%s is required: the session agent task is a docker task and there is no other session shape to fall back to", envAgentImage)
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
		diagnostics: diag,
		logf: func(format string, args ...any) {
			line := fmt.Sprintf(format, args...)
			fmt.Fprintf(os.Stderr, "gc-runtime-nomad: %s\n", line)
			// And to a file the caller cannot discard. See diagnostics.go.
			diag.write(currentOp, line)
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

// checkAllocListCapability asks Nomad whether this deployment's token can still
// LIST ALLOCATIONS, and fails the check when it cannot.
//
// This probe exists because of a live incident that `check` was blind to. A
// token kept its submit-job capability and lost its allocation read. Every
// lifecycle op that MINTS a box kept working, so nothing looked broken — while
// is-running, whose only evidence is the allocation list, started answering
// last-known-good on every call. The pack warned correctly and durably, and the
// warning said the quiet part outright: an authorisation failure "will answer
// the same way on every call until a credential changes, so every session will
// keep reporting healthy until then."
//
// It did. 2016 of those warnings in twenty-seven minutes, an entire pool
// reporting healthy, its demand probe seeing no starvation, and eighty-three
// beads sitting ready for a lane that could not claim any of them. That is the
// dark lane this pack exists to make impossible, reached through a capability
// nobody thought to check.
//
// So it is a FAIL, not a warning. A deployment that cannot observe its own
// boxes cannot be trusted to report on them, and the one thing worse than a
// broken lane is a broken lane that reports healthy.
//
// Unreachable is deliberately NOT a failure: `check` must stay useful for
// someone configuring the pack before their cluster is up, and "I could not
// reach Nomad" is a different, self-announcing problem. Only a definitive
// refusal fails.
func checkAllocListCapability(stderr io.Writer) int {
	if strings.TrimSpace(os.Getenv(envAddr)) == "" {
		fmt.Fprintf(stderr, "warning: cannot check whether this deployment may list allocations (%s unset); "+
			"a token that can dispatch but not list allocations makes every session report healthy forever\n",
			envAddr)
		return exitOK
	}
	l, err := newLifecycle()
	if err != nil {
		fmt.Fprintf(stderr, "warning: could not build a client to check the allocation-read capability: %v\n", err)
		return exitOK
	}
	ctx, cancel := context.WithTimeout(context.Background(), allocListProbeTimeout)
	defer cancel()
	// A job id that does not exist. A 404/empty answer proves the capability as
	// well as a real job would, and the probe reads nothing it is not entitled
	// to — the question is only whether the read is PERMITTED.
	_, _, err = l.client.listAllocsForJob(ctx, allocListProbeJob, 0, 0)
	switch {
	case err == nil || errJobGoneOrEmpty(err):
		return exitOK
	case isForbiddenErr(err):
		fmt.Fprint(stderr, allocReadForbiddenMessage(l.client.namespace))
		return exitError
	default:
		fmt.Fprintf(stderr, "warning: could not confirm the allocation-read capability (%v); "+
			"this check fails only on a definitive refusal, not on an unreachable cluster\n", err)
		return exitOK
	}
}

// allocReadForbiddenMessage is split out so a test can assert what it says
// without a Nomad server. What it says is the point: "403" alone sends an
// operator to the wrong layer.
func allocReadForbiddenMessage(namespace string) string {
	return fmt.Sprintf("FAIL: this deployment's Nomad credential may not list allocations (403). "+
		"Minting a box would still work, so nothing would look broken — but is-running has no "+
		"evidence except the allocation list, so it would answer last-known-good on every call and "+
		"EVERY SESSION WOULD REPORT HEALTHY until the credential changes. A pool whose seats all "+
		"report healthy sees no starvation and mints no replacement, so routed work sits unclaimed "+
		"against a lane that looks alive. Grant this token read access to the job and allocation "+
		"endpoints in namespace %q (Nomad policy: namespace \"%s\" { policy = \"read\" }, or "+
		"capabilities including \"read-job\"), or supply a token that has it.\n",
		namespace, namespace)
}

const (
	allocListProbeTimeout = 10 * time.Second
	// A name no real session produces, so the probe can never collide with one.
	allocListProbeJob = "gc-runtime-nomad-capability-probe-no-such-job"
)

// errJobGoneOrEmpty treats "that job does not exist" as proof the read was
// ALLOWED. The probe deliberately names a job that is not there.
func errJobGoneOrEmpty(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, errJobGone) || strings.Contains(err.Error(), "404")
}

// resolveNomadToken reads the Nomad ACL token from GC_NOMAD_TOKEN, or from the
// FILE named by GC_NOMAD_ACL_FILE.
//
// WHY A FILE SOURCE EXISTS, and why its variable is not called *_TOKEN_FILE.
//
// gc filters its own environment before handing it to child processes:
// internal/execenv.IsSensitiveKey treats any key CONTAINING "TOKEN" (or
// PASSWORD, SECRET, CREDENTIAL, OAUTH, ...) as sensitive and drops it. That is
// correct behaviour — a credential should not be inherited by every process a
// supervisor spawns — but it has a consequence this pack has to live with:
// GC_NOMAD_TOKEN can NEVER reach this binary by inheritance, because its own
// name matches the filter. There is also no config path for it: PackRuntimeEntry
// carries command, protocol and prompt-delivery, and no env map.
//
// Measured on a live deployment: GC_NOMAD_AGENT_IMAGE, exported by the same
// wrapper script in the same way, arrives here fine. GC_NOMAD_TOKEN does not.
// The only difference between them is the name.
//
// The symptom that produces is the worst one this pack has. Ops that MINT a box
// keep working, so nothing looks broken, while is-running cannot read the
// allocation list and answers last-known-good on every call — every session
// reports healthy, the pool sees no starvation, and routed work sits against a
// lane that looks alive.
//
// So the token travels as a PATH instead. A path is not a credential, so
// GC_NOMAD_ACL_FILE survives the filter honestly rather than by disguising a
// secret under an innocuous name — and it is the better shape anyway: the value
// never enters any environment, never appears in a process listing, and the file
// can be 0600. It is the same convention this pack already uses for _PEM keys
// and the log shipper's token file.
//
// GC_NOMAD_TOKEN still wins when both are set, so no existing deployment that
// manages to deliver it changes behaviour.
func resolveNomadToken() (string, error) {
	if t := strings.TrimSpace(os.Getenv(envToken)); t != "" {
		return t, nil
	}
	path := strings.TrimSpace(os.Getenv(envACLFile))
	if path == "" {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		// Named but unusable is an error, not a silent fall-through to no
		// token: falling through is exactly how a deployment ends up
		// unauthenticated while believing it is configured.
		return "", fmt.Errorf("reading the Nomad ACL token from %s=%q: %w "+
			"(the file must exist and be readable by the process running this pack; "+
			"an unreadable one is refused rather than quietly running with no token, "+
			"because no token means every session reports healthy while claiming nothing)",
			envACLFile, path, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("the Nomad ACL token file %s=%q is empty", envACLFile, path)
	}
	return token, nil
}
