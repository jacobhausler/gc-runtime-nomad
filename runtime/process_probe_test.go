package main

import (
	"errors"
	"testing"
)

// TestClassifyProcessProbeSeparatesUnreachableFromUnmeasurable pins the split
// an outside review found collapsed: both failure modes returned "an agent is
// running", so a box whose image has no `ps` asserted health forever.
//
// The two are not the same failure. A transport error says nothing about the
// box and must stay silent — warning on an unreachable box is the cry-wolf
// behaviour this probe was built to end. But an exec that REACHED the box and
// exited nonzero means the probe command failed in there, and since this
// runtime requires `ps` in the image, that is a contract violation that
// disables the empty-box diagnosis for every session the deployment runs.
func TestClassifyProcessProbeSeparatesUnreachableFromUnmeasurable(t *testing.T) {
	tests := []struct {
		name               string
		err                error
		exitCode           int
		out                string
		runs, unmeasurable bool
	}{
		{
			name: "transport fault says nothing about the box",
			err:  errors.New("dial tcp: connection refused"),
			runs: true, unmeasurable: false,
		},
		{
			name:     "probe reached the box and could not run: no usable ps",
			exitCode: 127,
			out:      "sh: 1: ps: not found\n",
			runs:     true, unmeasurable: true,
		},
		{
			name: "empty box: only the furniture",
			out:  "COMMAND\nsleep\ntmux: server\nsh\n",
			runs: false, unmeasurable: false,
		},
		{
			name: "working box: an agent beside the furniture",
			out:  "COMMAND\nsleep\ntmux: server\nsh\ncodex\n",
			runs: true, unmeasurable: false,
		},
		{
			name: "wrapped pane still counts: the shell in front is furniture",
			out:  "COMMAND\nsleep\ntmux: server\nbash\nsh\nclaude\n",
			runs: true, unmeasurable: false,
		},
		{
			name: "ps itself is never the agent",
			out:  "COMMAND\nsleep\ntmux: server\nsh\nps\n",
			runs: false, unmeasurable: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runs, unmeasurable := classifyProcessProbe(tc.err, tc.exitCode, []byte(tc.out))
			if runs != tc.runs || unmeasurable != tc.unmeasurable {
				t.Errorf("classifyProcessProbe = (runs=%v, unmeasurable=%v), want (runs=%v, unmeasurable=%v)",
					runs, unmeasurable, tc.runs, tc.unmeasurable)
			}
		})
	}
}
