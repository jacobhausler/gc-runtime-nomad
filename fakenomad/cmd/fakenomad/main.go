// Command fakenomad is a standalone process wrapping the in-memory fake
// Nomad API (package fakenomad) so `gc runtime check`/`gc runtime
// conformance` and this pack's CI conformance gate (conformance.sh) can
// drive gc-runtime-nomad against a live loopback server with no live Nomad
// cluster. It is not linked into the shipped gc-runtime-nomad binary — the
// fakenomad package stays a test-only dependency of the runtime module (see
// runtime/go.mod); this binary is just another consumer of that package,
// built and run as its own process for the harness.
//
// Usage:
//
//	fakenomad
//
// It prints the server's base URL (scheme+host) as the first stdout line so
// a harness can capture it, then serves until killed. Point the runtime at
// it with GC_NOMAD_ADDR=<printed URL>.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gastownhall/gc-runtime-nomad/fakenomad"
)

func main() {
	srv := fakenomad.NewServer()
	defer srv.Close()

	// Announce the base URL on the first line so a harness can capture it.
	fmt.Println(srv.URL())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}
