package main

import "testing"

// THE RACE, measured repeatedly on 2026-09-12 once the fetched gc artifact
// made the task's pre-start work long:
//
//	provision FAILED: staging workspace for "...": the "agent" task never
//	started (state "pending"): Downloading Artifacts: Client is downloading
//	artifacts: reading exec frame: EOF
//
// awaitExecAllocReady answered READY for an allocation carrying no task states
// at all. That was written for a stub read — "the API declining to answer is
// not an answer that the task is down" — and it is right about a stub. It is
// wrong about a REAL allocation Nomad has just created and not yet filled in,
// and a 261 MB artifact download widened that instant into a window every
// start fell into.
//
// ClientStatus separates the two, and Nomad always supplies it: an alloc whose
// ClientStatus is "pending" has no running task BY DEFINITION, whatever its
// task states say or fail to say. A stub that models no tasks reports the
// alloc running and keeps its immediate answer.
//
// Asserted as a pure predicate rather than by driving the poll loop: the
// decision is the whole of the defect, and testing it directly needs no
// test-only seam on the lifecycle type.
func TestAllocExecReadyDecision(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  allocRecord
		want bool
	}{{
		name: "running task is ready",
		rec: allocRecord{ClientStatus: "running", TaskStates: map[string]allocTaskState{
			execTaskName: {State: allocStateRunning},
		}},
		want: true,
	}, {
		name: "pending task is not ready",
		rec: allocRecord{ClientStatus: "running", TaskStates: map[string]allocTaskState{
			execTaskName: {State: "pending"},
		}},
		want: false,
	}, {
		// The stub contract: no task states AND the alloc itself running.
		name: "no task states on a running alloc stays ready",
		rec:  allocRecord{ClientStatus: "running"},
		want: true,
	}, {
		// THE DEFECT. A real fresh alloc, before Nomad fills in task states.
		name: "no task states on a PENDING alloc is not ready",
		rec:  allocRecord{ClientStatus: "pending"},
		want: false,
	}, {
		name: "no task states and no client status is not ready",
		rec:  allocRecord{},
		want: false,
	}, {
		// A task state for a DIFFERENT task tells us nothing about ours, and
		// the alloc is still pending.
		name: "other task's state on a pending alloc is not ready",
		rec: allocRecord{ClientStatus: "pending", TaskStates: map[string]allocTaskState{
			"sidecar": {State: allocStateRunning},
		}},
		want: false,
	}} {
		if got := allocExecReady(tc.rec); got != tc.want {
			t.Errorf("%s: allocExecReady = %v, want %v", tc.name, got, tc.want)
		}
	}
}
