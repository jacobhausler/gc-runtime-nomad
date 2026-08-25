// Command gc-runtime-nomad is a Runtime Provider Protocol (RPP) v0
// executable that runs Gas City sessions as dispatched Nomad jobs. It
// answers the `protocol` handshake and the four lifecycle ops this phase
// scopes — start, stop, is-running, list-running — over Nomad job
// dispatch/deregister/blocking reads (NRT-P1-03: 04 §3/§4/§6). Every other
// op exits 2, the RPP forward-compatibility signal a caller treats as a
// no-op success; driving verbs (exec/nudge/peek/...), staging, and the
// provision/launch split are out of scope for this phase.
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
// Configuration comes from the environment:
//
//	GC_NOMAD_ADDR         required for lifecycle ops: absolute base URL of the Nomad API (e.g. http://127.0.0.1:4646)
//	GC_NOMAD_TOKEN        optional: ACL token sent as X-Nomad-Token
//	GC_NOMAD_NAMESPACE    optional: Nomad namespace (default "default")
//	GC_NOMAD_PARENT_JOB   optional: the city's parameterized parent job ID (default "gc-sessions")
//	GC_NOMAD_SIDECAR_DIR  required for lifecycle ops: directory for the sidecar session->job-ID bindings (04 §1)
package main

import (
	"context"
	"fmt"
	"os"
)

const (
	envAddr       = "GC_NOMAD_ADDR"
	envToken      = "GC_NOMAD_TOKEN"
	envNamespace  = "GC_NOMAD_NAMESPACE"
	envParentJob  = "GC_NOMAD_PARENT_JOB"
	envSidecarDir = "GC_NOMAD_SIDECAR_DIR"

	defaultParentJob = "gc-sessions"

	exitOK      = 0
	exitError   = 1
	exitUnknown = 2
)

// protocolHandshakeJSON is the response to the `protocol` op.
const protocolHandshakeJSON = `{"version":0,"capabilities":[]}`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// lifecycleOps is the set of RPP operations this phase implements against
// the Nomad API. Ops outside it exit 2 without requiring any configuration
// — so a misconfigured or unset GC_NOMAD_ADDR never turns a handshake or an
// unimplemented-op probe into a spurious failure.
var lifecycleOps = map[string]bool{
	"start":        true,
	"stop":         true,
	"is-running":   true,
	"list-running": true,
}

// run dispatches one RPP operation. It is separated from main so tests can
// drive it with an in-memory stream and assert exit codes.
func run(args []string, stdout, stderr *os.File) int {
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
		if err := l.opStart(ctx, name); err != nil {
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
		names, err := l.opListRunning(ctx)
		if err != nil {
			return fail(err)
		}
		for _, n := range names {
			fmt.Fprintln(stdout, n)
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
	return &lifecycle{client: c, sidecar: sc, parentJobID: parentJob}, nil
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
