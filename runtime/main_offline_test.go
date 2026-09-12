package main

import (
	"os"
	"testing"
)

// TestMain declares what this suite IS: an offline harness driving a fake
// Nomad API, minting no real sessions.
//
// MEASURED on a clean machine (a golang container with tmux installed and no
// `gc` on PATH): a dozen tests failed with the pack's own capability-gate
// refusal — "no `gc` on PATH answered the capability probe" — which names this
// variable as the correct answer for exactly this case: "set
// GC_NOMAD_SKIP_GC_CAPABILITY_GATE=1 if you are running an offline harness
// that mints no real sessions."
//
// Set here rather than in each test because the property belongs to the SUITE,
// not to any one case, and because a dozen individual t.Setenv calls are a
// dozen chances to miss one — which is how a clean machine ends up with a wall
// of failures a reader has to learn to ignore.
//
// An operator-set value still wins, and any test that needs the gate ACTIVE
// sets it back with t.Setenv, which overrides this for that test alone. That is
// what keeps the gate's own tests from going vacuous.
func TestMain(m *testing.M) {
	if os.Getenv("GC_NOMAD_SKIP_GC_CAPABILITY_GATE") == "" {
		os.Setenv("GC_NOMAD_SKIP_GC_CAPABILITY_GATE", "1")
	}
	os.Exit(m.Run())
}
