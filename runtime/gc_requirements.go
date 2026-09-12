package main

import (
	"fmt"
	"io"
)

// THE DECLARED MINIMUM. This is the pack's answer to "what does it require of
// gc", in one machine-readable place, and it exists because a VERSION cannot
// carry that answer here: every build of the lineage this pack needs reports
// the literal string "dev" from `gc version`, so a numeric comparison is
// vacuous. What can be declared is the CAPABILITY SET.
//
// It is also an honesty mechanism. Exactly one family is probeable today, and
// the pack fails closed on it. The others cannot be detected from outside gc —
// and until this file existed they were not merely undetected, they were
// UNMENTIONED at runtime: `check` reported the one it could test and said
// nothing about the rest, so a green check read as "compatible" when it meant
// "the one thing I can test passed".
//
// Declaring an unprobeable requirement is worth more than omitting it. An
// adopter can act on "this is required and I cannot verify it for you"; they
// cannot act on silence.
type gcRequirement struct {
	// Family is the behaviour name used in docs/PIN-DEPENDENCIES.md. The two
	// lists are asserted to agree, so neither can drift alone.
	Family string
	// Upstream is where the work lives, or "unfiled".
	Upstream string
	// Probeable records whether this pack can detect the family from outside
	// gc. False is the common case and is the point of the declaration.
	Probeable bool
	// Why is what breaks without it, in one line, phrased as the symptom an
	// operator would actually see.
	Why string
}

// tier1Requirements are the families without which a seat inside an allocation
// cannot claim, stamp or close work. This is the blocking set, not everything
// the pin happens to carry.
func tier1Requirements() []gcRequirement {
	return []gcRequirement{
		{
			Family:    "remote-worker-lifecycle",
			Upstream:  "https://github.com/gastownhall/gascity/pull/6146",
			Probeable: true,
			Why: "a seat in an allocation cannot claim at all: the box comes up, the agent " +
				"launches, the pane looks healthy, and the seat drains no_work forever while the " +
				"city lists its work as ready",
		},
		{
			Family:    "remote-worker-env-city-target",
			Upstream:  "https://github.com/gastownhall/gascity/pull/5864",
			Probeable: false,
			Why:       "the in-box gc cannot resolve the city it was told to use",
		},
		{
			Family:    "remote-worker-claim-dispatch",
			Upstream:  "https://github.com/gastownhall/gascity/pull/6146",
			Probeable: false,
			Why: "the remote claim bypasses the conditional fence, so two seats can each believe " +
				"they hold the same bead and the lost update is silent",
		},
		{
			Family:    "remote-worker-session-identity",
			Upstream:  "https://github.com/gastownhall/gascity/pull/6146",
			Probeable: false,
			Why:       "a typed close of a hook-claimed bead from inside a box is refused",
		},
		{
			Family:    "policy-conditional-writer-forwarding",
			Upstream:  "https://github.com/gastownhall/gascity/pull/6146",
			Probeable: false,
			Why:       "remote claim and heartbeat writes are unfenced",
		},
		{
			Family:    "remote-step-accept-list",
			Upstream:  "unfiled",
			Probeable: false,
			Why: "a step inside a box cannot stamp its own outcome before closing, so terminal " +
				"bookkeeping is lost and the work is re-run",
		},
	}
}

// reportGCRequirements prints the declared minimum and what could be verified.
//
// The shape matters: the unverifiable ones are listed, not summarised away. A
// count ("18 families undetected") is a statistic an adopter cannot act on; a
// list is a thing they can check against their own gc.
func reportGCRequirements(w io.Writer, probed gcWorkerSurface) {
	reqs := tier1Requirements()
	var unprobeable []gcRequirement
	for _, r := range reqs {
		if !r.Probeable {
			unprobeable = append(unprobeable, r)
		}
	}
	fmt.Fprintf(w, "declared minimum: %d gc behaviour families are required (docs/PIN-DEPENDENCIES.md "+
		"Tier 1). There is no version to compare — every build of the required lineage reports the "+
		"literal string \"dev\" from `gc version`, so the declaration is a CAPABILITY set.\n", len(reqs))

	switch probed {
	case gcWorkerSurfacePresent:
		fmt.Fprintln(w, "  verified present: remote-worker-lifecycle")
	case gcWorkerSurfaceMissing:
		fmt.Fprintln(w, "  verified ABSENT: remote-worker-lifecycle (this is the FAIL below)")
	case gcWorkerSurfaceAbsent:
		fmt.Fprintln(w, "  not verified: no gc answered the probe (this is the FAIL below)")
	default:
		fmt.Fprintln(w, "  not verified: the gc on PATH answered in an unrecognised shape")
	}

	fmt.Fprintf(w, "  DECLARED BUT NOT VERIFIABLE by this pack (%d) — a green check above does NOT "+
		"mean these are present:\n", len(unprobeable))
	for _, r := range unprobeable {
		fmt.Fprintf(w, "    - %-38s %s\n      without it: %s\n", r.Family, r.Upstream, r.Why)
	}
}
