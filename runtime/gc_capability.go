package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The pack depends on behaviour families that are not all upstream in gc. The
// families are enumerated in docs/PIN-DEPENDENCIES.md; the one that decides
// whether a session can do any work at all is remote-worker-lifecycle, the
// `/v0/city/{city}/worker/*` surface a seat inside an alloc claims through.
//
// Without it the pack does not break in a way anyone can see. The box starts,
// the agent launches, the pane is healthy, and the seat drains with
// {"action":"drain","reason":"no_work"} while the city lists its work as
// ready. That is the dark lane this gate exists to convert into a refusal.
//
// THE PROBE. `gc bd claim <id> --city-url <url> --city-name <name>` is
// refused by BOTH lineages, at the target gate, BEFORE any network dial — so
// this costs a local subprocess and nothing else, needs no city, no token and
// no reachable host. What differs is the refusal:
//
//	EDGE: "claim is not supported against a remote city; the remote worker
//	       subset is `update <id> --claim`, `heartbeat <id>`, ..."
//	base: "this command does not support a remote city (--city-url/--context)
//	       yet; remote support is being enabled incrementally"
//
// Measured 2026-09-12 against the live pinned binary and against a worktree at
// 6ba434271 (the upstream base of the pin lineage, which is also the commit
// this pack's CI gates on). The marker string occurs 3 times in the live gc
// binary and zero times anywhere in the base tree.
type gcWorkerSurface int

const (
	// gcWorkerSurfaceUnknown: the probe did not produce a recognisable
	// answer. Never block on this — an unrecognised gc is not evidence of a
	// missing family, and refusing a session over an unparsed string would
	// be a worse failure than the one being prevented.
	gcWorkerSurfaceUnknown gcWorkerSurface = iota
	// gcWorkerSurfacePresent: the remote worker subset is there.
	gcWorkerSurfacePresent
	// gcWorkerSurfaceMissing: this gc answers with the upstream capability
	// gate. A seat driven by it cannot claim.
	gcWorkerSurfaceMissing
	// gcWorkerSurfaceAbsent: the probe could not run AT ALL — there is no
	// `gc` on PATH, or the one there answered with nothing.
	//
	// This is split out of Unknown because the two are opposite cases and
	// were being given the same fail-open answer. Unknown means "a gc
	// answered in a shape I do not recognise", and proceeding is right:
	// refusing a session over an unparsed string is worse than the fault.
	// Absent means "no gc answered". The pack shells out to `gc` for
	// everything a seat does, so a deployment with no gc on PATH cannot
	// work under any interpretation — there is no future gc whose wording
	// this might be. Failing open here handed the exact stranger case the
	// gate exists for (installing from public sources, gc not yet
	// installed) a green box and a dark seat.
	gcWorkerSurfaceAbsent
)

const (
	edgeWorkerSurfaceMarker = "remote worker subset"
	baseCapabilityGateMark  = "remote support is being enabled incrementally"
)

// classifyWorkerSurface reads the probe's combined output. Split from the
// subprocess so the three outcomes are testable without a gc on PATH.
func classifyWorkerSurface(output string) gcWorkerSurface {
	switch {
	case strings.Contains(output, edgeWorkerSurfaceMarker):
		return gcWorkerSurfacePresent
	case strings.Contains(output, baseCapabilityGateMark):
		return gcWorkerSurfaceMissing
	default:
		return gcWorkerSurfaceUnknown
	}
}

// gcMissingWorkerSurfaceError is the refusal. It names the family, what the
// operator would otherwise have seen, and where the full dependency table is —
// because "incompatible gc" alone sends someone hunting in the wrong layer,
// which is the specific cost this whole gate exists to avoid.
func gcMissingWorkerSurfaceError() error {
	return fmt.Errorf(
		"refusing to start: the `gc` on PATH does not carry the remote-worker-lifecycle "+
			"family, so a seat in an allocation cannot claim work through "+
			"/v0/city/{city}/worker/*. This pack cannot function on it. Starting anyway "+
			"would produce the failure this check exists to prevent: the box would come up, "+
			"the agent would launch, the pane would look healthy, and the seat would drain "+
			"with {\"action\":\"drain\",\"reason\":\"no_work\"} while the city listed its work "+
			"as ready. See docs/PIN-DEPENDENCIES.md for the full family table and "+
			"COMPATIBILITY.md for what this pack requires of gc. (Probe: %q absent from "+
			"`gc bd claim` under a remote target.)", edgeWorkerSurfaceMarker)
}

// gcAbsentWorkerSurfaceError is the refusal for "no gc answered the probe".
//
// It is a DIFFERENT message from the missing-family refusal on purpose. That
// one says "your gc is the wrong build"; this one says "there is no gc here",
// and sending an operator to the dependency table when the real fix is
// installing gc is the wrong-layer diagnosis this pack exists to stop making.
func gcAbsentWorkerSurfaceError() error {
	return fmt.Errorf(
		"refusing to start: no `gc` on PATH answered the capability probe, so this pack "+
			"cannot tell whether a seat in an allocation could claim work — and it shells "+
			"out to `gc` for everything such a seat does, so an absent gc cannot work "+
			"regardless. Install gc and put it on the PATH of the process running this pack "+
			"(the SUPERVISOR's PATH, not an interactive shell's), or set %s=1 if you are "+
			"running an offline harness that mints no real sessions. See COMPATIBILITY.md "+
			"for the gc this pack requires.", envSkipCapabilityGate)
}

// envSkipCapabilityGate lets a harness that is NOT running real sessions opt
// out of the gate.
//
// The offline conformance ladder is the case this exists for: it drives the
// pack's RPP surface against an in-memory Nomad fake, with no city, no claim
// and no work — so "the gc on PATH cannot claim work from inside an
// allocation" is true and irrelevant there. Without an opt-out the protocol
// suite becomes unrunnable by anyone who does not already have the pinned gc,
// which is precisely the contributor this pack most needs to keep.
//
// It is deliberately NOT quiet. Skipping prints a line every time, because an
// env var that silently disables a dark-lane guard is a way to recreate the
// dark lane — and this one would do it on the exact ops that mint boxes.
const envSkipCapabilityGate = "GC_NOMAD_SKIP_GC_CAPABILITY_GATE"

// capabilityGateSkipped reports whether the gate is opted out of, and warns
// when it is.
func capabilityGateSkipped(stderr io.Writer) bool {
	if strings.TrimSpace(os.Getenv(envSkipCapabilityGate)) == "" {
		return false
	}
	fmt.Fprintf(stderr, "warning: the gc capability gate is disabled (%s is set). "+
		"This pack is NOT checking whether the gc on PATH can claim work from inside "+
		"an allocation. Correct for an offline harness; in a real deployment it "+
		"restores the failure the gate exists to prevent — a box that comes up, looks "+
		"healthy, and drains no_work forever.\n", envSkipCapabilityGate)
	return true
}

// probeGCWorkerSurface runs the probe against the gc on PATH.
func probeGCWorkerSurface(ctx context.Context) gcWorkerSurface {
	gc, err := exec.LookPath("gc")
	if err != nil {
		return gcWorkerSurfaceAbsent
	}
	ctx, cancel := context.WithTimeout(ctx, gcProbeTimeout)
	defer cancel()
	// A bogus bead id under an unroutable target. Both lineages refuse at the
	// target gate before dialing, so nothing here reaches a network or a city.
	cmd := exec.CommandContext(ctx, gc, "bd", "claim", "gc-runtime-nomad-capability-probe",
		"--city-url", "https://127.0.0.1:1", "--city-name", "gc-runtime-nomad-probe")
	out, _ := cmd.CombinedOutput()
	// Empty output is not an unrecognised answer, it is no answer: the probe
	// never reached the target gate both lineages refuse at. Classifying it
	// Unknown let a gc that could not execute at all read as "proceed".
	if strings.TrimSpace(string(out)) == "" {
		return gcWorkerSurfaceAbsent
	}
	return classifyWorkerSurface(string(out))
}

// gcProbeTimeout bounds the probe. It is refused locally and returns in
// milliseconds; this only exists so a pathological gc cannot hang a start.
const gcProbeTimeout = 10 * time.Second

// gcWorkerSurfaceForCheck is the `check` door's view: the opt-out suppresses
// the FAIL the same way it suppresses the refusal, so a harness that can run
// the lifecycle can also run the diagnostic.
func gcWorkerSurfaceForCheck(stderr io.Writer) gcWorkerSurface {
	if capabilityGateSkipped(stderr) {
		return gcWorkerSurfacePresent
	}
	return probeGCWorkerSurface(context.Background())
}

// inBoxProbeCommand is the same capability probe, spelled for a box.
//
// THE HOST GATE IS NOT ENOUGH, and this was found by giving the shipped skill
// and a staged fault to an operator with no context: they diagnosed it in one
// command and then pointed out the hole. The gate probes only the `gc` on the
// SUPERVISOR's PATH. A deployment with a compliant host `gc` and a
// base-lineage `gc` baked into the agent image gets green boxes and dark
// seats, and the gate is blind to it — which is the same dark lane it exists
// to prevent, one layer in.
//
// The in-box `gc` is the one that actually claims, so it is the one that
// matters most. It can only be probed once a box exists, so this is a launch
// observation rather than a precondition.
func inBoxProbeCommand() []string {
	return []string{"/bin/sh", "-c",
		"gc bd claim gc-runtime-nomad-capability-probe " +
			"--city-url https://127.0.0.1:1 --city-name gc-runtime-nomad-probe 2>&1"}
}

// inBoxWorkerSurfaceWarning reports the warning owed for a box whose `gc`
// cannot claim, or "" when it can or when the answer is unrecognisable.
//
// Unknown never warns, for the same reason the host gate never blocks on it:
// a `gc` this probe does not recognise is not evidence of a missing family,
// and a launch-time false alarm about the deepest dependency in the stack is
// worse than silence.
func inBoxWorkerSurfaceWarning(output string) string {
	if classifyWorkerSurface(output) != gcWorkerSurfaceMissing {
		return ""
	}
	return "warning: the `gc` INSIDE this box does not carry the remote-worker-lifecycle " +
		"family. The box is healthy and the agent will start, but the seat cannot claim " +
		"work through /v0/city/{city}/worker/* and will drain no_work while the city lists " +
		"its work as ready. The host gate passed, so this is the AGENT IMAGE, not the " +
		"supervisor: rebuild it against the same gc lineage (COMPATIBILITY.md, \"The agent image\")."
}
