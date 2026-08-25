// Command reconcilersim-drill is the standalone half of the NRT-P2-06a L4
// harness ("the L4 harness = pack CLI + scripted reconciler-sim driver,
// standing in for GC until L5" — 08 §1.1). It wires a
// reconcilersim.Driver from the environment and runs one named scenario
// against whatever Nomad target GC_NOMAD_ADDR names — fakenomad in offline
// mode, or the real T1 lab in L4 mode (set GC_NOMAD_TLS_CACERT/CERT/KEY per
// the NRT-P2-02 baseline).
//
// This binary ships exactly one built-in scenario, "lifecycle-roundtrip"
// (reconcilersim.LifecycleRoundtripSteps) — a smoke check that the wiring
// (pack CLI subprocess, mTLS front proxy, observer client) actually reaches
// the target end to end. The 08 §1.1 per-drill beads (fnrt-o37.3 ..
// fnrt-o37.14) are expected to script their own scenario as a short Go
// program importing this same reconcilersim package (New, Step, RunScript,
// the classify.go helpers) — this command is a working example of that
// shape, not a driver for all eleven drills.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/gastownhall/gc-runtime-nomad/drills/reconcilersim"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	scenario := "lifecycle-roundtrip"
	if len(args) > 0 {
		scenario = args[0]
	}
	if scenario != "lifecycle-roundtrip" {
		fmt.Fprintf(stderr, "reconcilersim-drill: unknown scenario %q (only \"lifecycle-roundtrip\" ships in this binary)\n", scenario)
		return 2
	}

	cfg := reconcilersim.ConfigFromEnv()
	if cfg.Addr == "" {
		fmt.Fprintf(stderr, "reconcilersim-drill: %s is required\n", reconcilersim.EnvAddr)
		return 1
	}

	d, err := reconcilersim.New(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "reconcilersim-drill: %v\n", err)
		return 1
	}
	defer d.Close()

	session := fmt.Sprintf("drill-selfcheck-%d", time.Now().UnixNano())
	steps := reconcilersim.LifecycleRoundtripSteps(session, cfg.ParentJob)
	report := reconcilersim.RunScript(context.Background(), d, steps)

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		fmt.Fprintf(stderr, "reconcilersim-drill: encoding report: %v\n", err)
		return 1
	}

	if !report.AllPassed() {
		return 1
	}
	return 0
}
