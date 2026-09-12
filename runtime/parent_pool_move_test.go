package main

import (
	"strings"
	"testing"
)

// ensureParentRegistered compares the LIVE parent's node pool against the pool
// the CALLING PROCESS computed from its own environment, and used to
// re-register on any difference — silently. A pool change is not a task-spec
// change: picking up a new task spec is routine, and moving the pool repoints
// where every future session is placed.
//
// This cost a live outage on 2026-09-12. A hand-run `start`, for diagnosis,
// with GC_NOMAD_NODE_POOL simply absent from that shell, moved the parent from
// "prod-services" into Nomad's "default" pool, which held zero nodes. Every
// dispatch then pended forever, nothing on the session said why, and the
// supervisor silently restored it on its next pass — so the window looked like
// an intermittent fault rather than a configuration change.

func TestForgottenNodePoolIsRefusedNotApplied(t *testing.T) {
	err := parentPoolMoveRefusal(true, "prod-services", "", "gc-sessions")
	if err == nil {
		t.Fatal("a registered pool plus an unset variable must be REFUSED: an absent variable is a " +
			"forgotten one, not a decision, and applying it moves the whole fleet's placement")
	}
	got := err.Error()
	for _, want := range []struct{ needle, why string }{
		{"prod-services", "the pool it would have moved AWAY from is not named"},
		{envNodePool, "the variable whose absence caused this is not named"},
		{"pends forever", "the consequence is not stated, so the refusal looks like pedantry"},
		{"forgotten", "it does not say why this is refused rather than warned"},
	} {
		if !strings.Contains(got, want.needle) {
			t.Errorf("the refusal lacks %q: %s\n%s", want.needle, want.why, got)
		}
	}
}

// The routine cases must NOT be refused, or a pack upgrade cannot land.
func TestOrdinaryRegistrationIsNotRefused(t *testing.T) {
	cases := []struct {
		name                 string
		exists               bool
		registered, computed string
	}{
		{"parent absent entirely", false, "", "prod-services"},
		{"registered pool empty, computed empty", true, "", ""},
		{"registered pool empty, computed set", true, "", "prod-services"},
		{"both set and identical", true, "prod-services", "prod-services"},
		{"both set and different — a decision", true, "prod-services", "prod-partial"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := parentPoolMoveRefusal(c.exists, c.registered, c.computed, "gc-sessions"); err != nil {
				t.Errorf("refused a legitimate registration: %v", err)
			}
		})
	}
}

// A deliberate move proceeds, but must announce itself: the dispatches it
// breaks carry no error of their own.
func TestDeliberatePoolMoveIsAnnounced(t *testing.T) {
	w := parentPoolMoveWarning(true, "prod-services", "prod-partial", "gc-sessions")
	if w == "" {
		t.Fatal("a pool move with both pools expressed must be announced")
	}
	for _, want := range []string{"prod-services", "prod-partial", "pending"} {
		if !strings.Contains(w, want) {
			t.Errorf("the move warning lacks %q:\n%s", want, w)
		}
	}
	// And the non-moves must stay silent, or the warning becomes noise that is
	// filtered out and stops being read.
	for _, c := range [][3]string{
		{"prod-services", "prod-services", "identical"},
		{"", "prod-services", "no prior pool"},
		{"prod-services", "", "unset — that is the refusal's job, not a warning"},
	} {
		if got := parentPoolMoveWarning(true, c[0], c[1], "gc-sessions"); got != "" {
			t.Errorf("%s should not warn, got: %s", c[2], got)
		}
	}
}
