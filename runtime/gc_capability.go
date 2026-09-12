package main

import (
	"context"
	"fmt"
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

// probeGCWorkerSurface runs the probe against the gc on PATH.
func probeGCWorkerSurface(ctx context.Context) gcWorkerSurface {
	gc, err := exec.LookPath("gc")
	if err != nil {
		return gcWorkerSurfaceUnknown
	}
	ctx, cancel := context.WithTimeout(ctx, gcProbeTimeout)
	defer cancel()
	// A bogus bead id under an unroutable target. Both lineages refuse at the
	// target gate before dialing, so nothing here reaches a network or a city.
	cmd := exec.CommandContext(ctx, gc, "bd", "claim", "gc-runtime-nomad-capability-probe",
		"--city-url", "https://127.0.0.1:1", "--city-name", "gc-runtime-nomad-probe")
	out, _ := cmd.CombinedOutput()
	return classifyWorkerSurface(string(out))
}

// gcProbeTimeout bounds the probe. It is refused locally and returns in
// milliseconds; this only exists so a pathological gc cannot hang a start.
const gcProbeTimeout = 10 * time.Second
