package reconcilersim_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gc-runtime-nomad/drills/reconcilersim"
)

// TestClassifyDeath is a pure unit test of the classification decision
// table (classify.go) against synthetic before/after AllocRecord pairs —
// no cluster, fake or real, needed to prove the logic itself is right;
// driver_test.go additionally proves the box-death path through the real
// pack CLI + fakenomad boundary.
func TestClassifyDeath(t *testing.T) {
	cases := []struct {
		name           string
		before         reconcilersim.AllocRecord
		after          reconcilersim.AllocRecord
		isRunningAfter bool
		want           reconcilersim.DeathClass
	}{
		{
			name:           "box survives, is-running flips false: agent-death",
			before:         reconcilersim.AllocRecord{ID: "a1", ClientStatus: "running"},
			after:          reconcilersim.AllocRecord{ID: "a1", ClientStatus: "running"},
			isRunningAfter: false,
			want:           reconcilersim.DeathAgent,
		},
		{
			name:           "alloc goes terminal: box-death",
			before:         reconcilersim.AllocRecord{ID: "a1", ClientStatus: "running"},
			after:          reconcilersim.AllocRecord{ID: "a1", ClientStatus: "failed"},
			isRunningAfter: false,
			want:           reconcilersim.DeathBox,
		},
		{
			name:           "nothing moved: unknown",
			before:         reconcilersim.AllocRecord{ID: "a1", ClientStatus: "running"},
			after:          reconcilersim.AllocRecord{ID: "a1", ClientStatus: "running"},
			isRunningAfter: true,
			want:           reconcilersim.DeathUnknown,
		},
		{
			name:           "box was never alive: unknown",
			before:         reconcilersim.AllocRecord{ID: "a1", ClientStatus: "failed"},
			after:          reconcilersim.AllocRecord{ID: "a1", ClientStatus: "failed"},
			isRunningAfter: false,
			want:           reconcilersim.DeathUnknown,
		},
		{
			name:           "pending counts as alive",
			before:         reconcilersim.AllocRecord{ID: "a1", ClientStatus: "pending"},
			after:          reconcilersim.AllocRecord{ID: "a1", ClientStatus: "lost"},
			isRunningAfter: false,
			want:           reconcilersim.DeathBox,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcilersim.ClassifyDeath(tc.before, tc.after, tc.isRunningAfter)
			if got != tc.want {
				t.Fatalf("ClassifyDeath(%+v, %+v, %v) = %v, want %v", tc.before, tc.after, tc.isRunningAfter, got, tc.want)
			}
		})
	}
}

// TestCheckIsRunningHonesty exercises the 04 §6 observation-honesty split
// (NRT-OBS-001..004): is-running must never flip to a false negative on
// mere API unavailability for a session last known running.
func TestCheckIsRunningHonesty(t *testing.T) {
	errUnavailable := errors.New("nomad: unavailable")

	cases := []struct {
		name             string
		wasRunningBefore bool
		observedRunning  bool
		lookupErr        error
		wantHonest       bool
	}{
		{"running before, error now: last-known-good required", true, false, errUnavailable, false},
		{"never running, error now: honest", false, false, errUnavailable, true},
		{"running before, flips false with no error: dishonest", true, false, nil, false},
		{"running before, stays true: honest", true, true, nil, true},
		{"never running, stays false: honest", false, false, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcilersim.CheckIsRunningHonesty(tc.wasRunningBefore, tc.observedRunning, tc.lookupErr)
			if got.Honest != tc.wantHonest {
				t.Fatalf("CheckIsRunningHonesty(%v, %v, %v) = %+v, want Honest=%v", tc.wasRunningBefore, tc.observedRunning, tc.lookupErr, got, tc.wantHonest)
			}
		})
	}
}

func TestCountReplacementAllocs(t *testing.T) {
	cases := []struct {
		name   string
		allocs []reconcilersim.AllocRecord
		want   int
	}{
		{"none", nil, 0},
		{"one", []reconcilersim.AllocRecord{{ID: "a1"}}, 0},
		{"two: one replacement", []reconcilersim.AllocRecord{{ID: "a1"}, {ID: "a2"}}, 1},
		{"four: three replacements", []reconcilersim.AllocRecord{{ID: "a1"}, {ID: "a2"}, {ID: "a3"}, {ID: "a4"}}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reconcilersim.CountReplacementAllocs(tc.allocs); got != tc.want {
				t.Fatalf("CountReplacementAllocs(%v) = %d, want %d", tc.allocs, got, tc.want)
			}
		})
	}
}

func TestEgressOrderingOrdered(t *testing.T) {
	t0 := time.Unix(1000, 0)
	t1 := t0.Add(time.Second)

	cases := []struct {
		name string
		o    reconcilersim.EgressOrdering
		want bool
	}{
		{
			name: "egress completed strictly before deregister: ordered",
			o:    reconcilersim.EgressOrdering{EgressObservedAt: t0, DeregisterObservedAt: t1, EgressCompleted: true},
			want: true,
		},
		{
			name: "deregister observed before egress completed: not ordered",
			o:    reconcilersim.EgressOrdering{EgressObservedAt: t1, DeregisterObservedAt: t0, EgressCompleted: true},
			want: false,
		},
		{
			name: "egress never completed, no evidence_lost marker: not ordered (silent loss)",
			o:    reconcilersim.EgressOrdering{DeregisterObservedAt: t1, EgressCompleted: false},
			want: false,
		},
		{
			name: "egress never completed but evidence_lost marked: ordered (priced fallback)",
			o:    reconcilersim.EgressOrdering{DeregisterObservedAt: t1, EgressCompleted: false, EvidenceLostMarked: true},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.o.Ordered(); got != tc.want {
				t.Fatalf("Ordered() = %v, want %v", got, tc.want)
			}
		})
	}
}
