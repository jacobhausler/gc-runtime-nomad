package main

import (
	"strings"
	"testing"
)

// Gate 4: every failure names its cause at the layer that knows.
//
// A seat whose box has died is correctly dropped from list-running — reporting a
// corpse as alive is the T5 defect. But dropping it SILENTLY leaves an operator
// with a lane that quietly shrank and no cause anywhere, which is the same
// dark-lane shape from the other side. Nomad already holds the answer in the
// allocation's task events; these are the causes it must be able to name.
func TestDeadSeatCausesAreNamed(t *testing.T) {
	const image = "gc-agent:abc123"

	for _, tc := range []struct {
		name  string
		event string
		want  []string
		why   string
	}{
		{
			name:  "OOM",
			event: "Terminated: OOM Killed",
			// OOM is its own named cause now, not a start failure. The
			// assertion is on the INTENT this row always had — the cause is
			// named and the driver's own text survives — rather than on the
			// generic wording it used to borrow. "never started" was a proxy
			// for "readable", and an OOM that says OOM is strictly more so.
			want: []string{"OOM-killed", "sizing fault", "OOM Killed"},
			why:  "an out-of-memory kill is the most common silent box death and must be readable without leaving the city",
		},
		{
			name:  "image pull denied",
			event: "Driver Failure: Failed to pull `gc-agent:abc123`: pull access denied",
			want:  []string{image, "is not on this node"},
			why:   "the T6 phrasing must hold on this path too, not only on provision",
		},
		{
			name:  "bad entrypoint",
			event: "Driver Failure: failed to start task: no such file or directory",
			want:  []string{"never started", "no such file or directory"},
			why:   "a driver cause with no special handling must still arrive verbatim",
		},
		{
			name:  "killed by the scheduler",
			event: "Killed: Task was killed by the scheduler",
			want:  []string{"never started", "killed by the scheduler"},
			why:   "a constraint eviction or drain must not read as an unexplained disappearance",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allocs := []allocRecord{{
				ID: "a1", ClientStatus: "failed",
				TaskStates: map[string]allocTaskState{
					"agent": {State: "dead", FailureEvent: tc.event},
				},
			}}
			got := allocFailureDiagnosis(image, allocs, "agent", knownArtifacts{})
			if got == "" {
				t.Fatalf("no cause named for %q — %s", tc.event, tc.why)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("cause %q does not contain %q — %s", got, want, tc.why)
				}
			}
		})
	}

	t.Run("no reason offered stays silent", func(t *testing.T) {
		allocs := []allocRecord{{ID: "a2", ClientStatus: "failed"}}
		if got := allocFailureDiagnosis(image, allocs, "agent", knownArtifacts{}); got != "" {
			t.Fatalf("invented a cause from nothing: %q — silence is correct when Nomad offers no reason", got)
		}
	})
}
