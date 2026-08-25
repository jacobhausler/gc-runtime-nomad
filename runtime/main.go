// Command gc-runtime-nomad is a phase-1 scaffold for a Runtime Provider
// Protocol (RPP) v0 executable. It answers only the `protocol` handshake op;
// every other op exits 2, the RPP forward-compatibility signal a caller
// treats as a no-op success. It makes no Nomad API call and implements no
// lifecycle op (NRT-P1-01: install + handshake against a stub only).
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
package main

import (
	"fmt"
	"os"
)

const (
	exitOK      = 0
	exitError   = 1
	exitUnknown = 2
)

// protocolHandshakeJSON is the response to the `protocol` op. This scaffold
// advertises no capabilities and implements no lifecycle op.
const protocolHandshakeJSON = `{"version":0,"capabilities":[]}`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one RPP operation. It is separated from main so tests can
// drive it with an in-memory stream and assert exit codes.
func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "gc-runtime-nomad: missing operation")
		return exitError
	}
	if args[0] != "protocol" {
		// Every op besides the handshake is unimplemented in this scaffold.
		return exitUnknown
	}
	fmt.Fprintln(stdout, protocolHandshakeJSON)
	return exitOK
}
