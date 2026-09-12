package main

import (
	"strings"
	"testing"
)

// The pack's placement diagnosis names the constraints it asked for, which is
// the right answer for a FILTERED placement and the wrong one for an empty node
// pool. The two are indistinguishable without reading the evaluation, and the
// remedies have nothing in common: one is a constraint change, the other is
// configuration — and no constraint change can ever fix an empty pool.
//
// Measured on a live cluster 2026-09-12: three ready, eligible nodes, all in
// pools `prod-services` and `prod-partial`, and a parent job registered into
// `default`. Every dispatch sat pending forever with NodesInPool=0,
// NodesEvaluated=0 and every filter counter nil. The pack's own message would
// have sent an operator to compare constraints against nodes that were never
// evaluated.

func TestEmptyNodePoolIsNotReportedAsAConstraintFault(t *testing.T) {
	p := placementFailure{NodePool: "default", NodesInPool: 0, NodesEvaluated: 0, Found: true}
	if !p.emptyNodePool() {
		t.Fatal("NodesInPool=0 with NodesEvaluated=0 must classify as an empty pool")
	}
	got := describePlacementFailure(p)

	for _, want := range []struct{ needle, why string }{
		{"NO NODES AT ALL", "the headline does not say the scheduler saw nothing"},
		{`"default"`, "the pool is not named, so the operator does not know which one is empty"},
		{"NOT a constraint", "it does not rule out the wrong remedy, which is the whole point"},
		{envNodePool, "it does not name the knob that retargets the pool"},
		{"re-registered", "it does not say the pool is a parent-job property, so a restart looks sufficient"},
	} {
		if !strings.Contains(got, want.needle) {
			t.Errorf("the empty-pool message lacks %q: %s\n%s", want.needle, want.why, got)
		}
	}
	// And it must NOT send them to the constraint layer.
	if strings.Contains(got, "nomad node status") {
		t.Errorf("the empty-pool message points at node status, which is the wrong layer here:\n%s", got)
	}
}

// A genuinely filtered placement must still get the counters, because there the
// constraint story IS the right one and the numbers say which rule bit.
func TestFilteredPlacementReportsTheCounters(t *testing.T) {
	p := placementFailure{
		NodePool: "prod-services", NodesInPool: 3, NodesEvaluated: 3, NodesFiltered: 3,
		ConstraintFilt: map[string]int{"${attr.cpu.arch} = amd64": 3},
		Found:          true,
	}
	if p.emptyNodePool() {
		t.Fatal("a pool with nodes must not classify as empty")
	}
	got := describePlacementFailure(p)
	for _, want := range []string{"evaluated 3", "prod-services", "3 filtered", "amd64", "constraints"} {
		if !strings.Contains(got, want) {
			t.Errorf("the filtered-placement message lacks %q:\n%s", want, got)
		}
	}
}

// An unreadable evaluation must add nothing rather than replace a real
// diagnosis with a transport complaint — this only ever runs to enrich a
// message the caller is already returning.
func TestUnreadableEvaluationAddsNothing(t *testing.T) {
	if got := describePlacementFailure(placementFailure{}); got != "" {
		t.Errorf("an absent placement failure produced %q, want empty", got)
	}
}
