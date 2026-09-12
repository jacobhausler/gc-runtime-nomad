package main

import (
	"strings"
	"testing"
)

// T6 (cr-kkcvmd). Measured live 2026-09-11 with GC_NOMAD_AGENT_IMAGE pointed at a
// tag that exists nowhere: Nomad reported
//
//	Failed to pull `gc-agent:...`: Error response from daemon: pull access denied
//	for gc-agent, repository does not exist
//
// while the city's log carried only "staging workspace for %q: reading exec
// frame: EOF" — no image, no node, no pull. This guard holds the repair: when the
// allocation's own task state knows why the task never started, the provision
// error must SAY it, and must name the image when the cause is the image.
func TestAllocFailureDiagnosisNamesTheCause(t *testing.T) {
	const image = "gc-agent:3a3dda06a"
	const pullEvent = "Failed to pull `gc-agent:nope`: Error response from daemon: " +
		"pull access denied for gc-agent, repository does not exist or may require 'docker login'"

	cases := []struct {
		name     string
		image    string
		allocs   []allocRecord
		want     []string // all must appear
		wantNone bool     // true => expect "" (no answer, keep the original error)
		why      string
	}{
		{
			name:  "image pull failure names the image and the node",
			image: image,
			allocs: []allocRecord{{
				ID: "a1", ClientStatus: "failed",
				TaskStates: map[string]allocTaskState{"agent": {State: "dead", LastEvent: pullEvent}},
			}},
			want: []string{image, "is not on this node", "pull access denied"},
			why:  "this is the T6 sentence: the operator must not have to leave the city to learn the image is absent",
		},
		{
			name:  "a non-image start failure is reported verbatim, not swallowed",
			image: image,
			allocs: []allocRecord{{
				ID: "a2", ClientStatus: "failed",
				TaskStates: map[string]allocTaskState{"agent": {State: "dead", LastEvent: "OOM Killed"}},
			}},
			// Same re-point as dead_seat_reason_test.go: this row exists to
			// prove a non-image driver cause arrives with its own text, and it
			// still does — under a name of its own instead of the generic
			// start-failure wording.
			want: []string{"OOM-killed", "OOM Killed"},
			why:  "the fix is not an image special case — every driver cause must arrive with its own text",
		},
		{
			name:  "a failed alloc is preferred over a newer healthy-looking one",
			image: image,
			allocs: []allocRecord{
				{ID: "a3", ClientStatus: "failed", TaskStates: map[string]allocTaskState{"agent": {State: "dead", LastEvent: pullEvent}}},
				{ID: "a4", ClientStatus: "pending", TaskStates: map[string]allocTaskState{"agent": {State: "pending", LastEvent: "Task Setup"}}},
			},
			want: []string{"is not on this node"},
			why:  "a reschedule after the failure must not hide the failure that matters",
		},
		{
			// THE TRAP THAT FOOLED ME LIVE. Nomad's newest event on a failed pull
			// is "Policy allows no restarts"; the pull error sits earlier in the
			// list. Reading the newest event produced a diagnosis that named the
			// restart policy and never mentioned the image, which is barely better
			// than the EOF it replaced.
			name:  "the failure event wins over a newer bookkeeping event",
			image: image,
			allocs: []allocRecord{{
				ID: "a8", ClientStatus: "failed",
				TaskStates: map[string]allocTaskState{"agent": {
					State:        "dead",
					LastEvent:    "Restart Signaled: Policy allows no restarts",
					FailureEvent: "Driver Failure: " + pullEvent,
				}},
			}},
			want: []string{image, "is not on this node", "pull access denied"},
			why:  "the newest event is usually the least informative one; the cause must win",
		},
		{
			name:     "no allocations at all is NOT this function's answer",
			image:    image,
			allocs:   nil,
			wantNone: true,
			why:      "nothing placed is the placement shape, which confirmDispatchPlaced already reports",
		},
		{
			name:     "a stub read with no task states is not an answer",
			image:    image,
			allocs:   []allocRecord{{ID: "a5", ClientStatus: "failed"}},
			wantNone: true,
			why:      "a read that carried no task states must never be turned into an invented cause",
		},
		{
			name:  "an empty last event is not an answer",
			image: image,
			allocs: []allocRecord{{
				ID: "a6", ClientStatus: "failed",
				TaskStates: map[string]allocTaskState{"agent": {State: "dead", LastEvent: "   "}},
			}},
			wantNone: true,
			why:      "silence from Nomad keeps the caller's original error rather than replacing it with nothing",
		},
		{
			name:  "an unknown agent image still reports the driver text",
			image: "",
			allocs: []allocRecord{{
				ID: "a7", ClientStatus: "failed",
				TaskStates: map[string]allocTaskState{"agent": {State: "dead", LastEvent: pullEvent}},
			}},
			want: []string{"agent image is not on this node", "pull access denied"},
			why:  "a pack that cannot name its own image still must not answer EOF",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := allocFailureDiagnosis(tc.image, tc.allocs, "agent")
			if tc.wantNone {
				if got != "" {
					t.Fatalf("diagnosis = %q, want empty — %s", got, tc.why)
				}
				return
			}
			if got == "" {
				t.Fatalf("diagnosis was empty — %s", tc.why)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("diagnosis %q does not contain %q — %s", got, w, tc.why)
				}
			}
			// The whole point of T6: never ship the bare frame error as the cause.
			if strings.Contains(strings.ToLower(got), "exec frame") {
				t.Fatalf("diagnosis %q still leads with the exec-frame symptom", got)
			}
		})
	}
}
