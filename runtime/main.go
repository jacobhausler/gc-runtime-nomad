// Command gc-runtime-nomad is a Runtime Provider Protocol (RPP) v0
// executable that runs Gas City sessions as dispatched Nomad jobs. It
// answers the `protocol` handshake, the four lifecycle ops from NRT-P1-03
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
// op exits 2, the RPP forward-compatibility signal a caller treats as a
// no-op success.
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
//	GC_NOMAD_NAMESPACE    optional: Nomad namespace (default "default")
//	GC_NOMAD_NODE_POOL    optional: Nomad node pool for the parent job (default "" — Nomad's own "default" pool)
//	GC_NOMAD_PARENT_JOB   optional: the city's parameterized parent job ID (default "gc-sessions")
//	GC_NOMAD_SIDECAR_DIR  required for lifecycle ops: directory for the sidecar session->job-ID bindings (04 §1)
//	GC_NOMAD_EGRESS_DIR   optional: local directory for stop-path transcript/evidence egress (NRT-P1-07); unset disables egress
//	GC_NOMAD_FORBID_REGISTER  optional: "true" forbids ever registering the parent job (04 §4 lab ACL model) — start/provision fail closed instead of attempting a register call that needs the submit-job capability
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
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

	defaultParentJob = "gc-sessions"

	exitOK      = 0
	exitError   = 1
	exitUnknown = 2
)

// protocolHandshakeJSON is the response to the `protocol` op. proc.provision
// and proc.exec are declared now that both are implemented (04 §3);
// env.workspace joins them as of NRT-P1-06 (workspace/secret staging over
// exec-stdin). The remaining v0-target capabilities (env.tooling/
// env.identity/env.transcripts) stay undeclared — out of scope here.
const protocolHandshakeJSON = `{"version":0,"capabilities":["proc.provision","proc.exec","env.workspace"]}`

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
	forbidRegistration, _ := strconv.ParseBool(os.Getenv(envForbidReg))
	return &lifecycle{
		client:             c,
		sidecar:            sc,
		parentJobID:        parentJob,
		nodePool:           os.Getenv(envNodePool),
		egressDir:          os.Getenv(envEgressDir),
		forbidRegistration: forbidRegistration,
	}, nil
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
