package main

import (
	"errors"
	"strings"
	"testing"
)

// A live incident is the reason this probe exists, and the reason it is a FAIL
// rather than a warning.
//
// A deployment's token kept submit-job and lost its allocation read. Every op
// that MINTS a box kept working, so nothing looked broken. is-running, whose
// only evidence is the allocation list, answered last-known-good on every
// call — 2016 warnings in twenty-seven minutes, a whole pool reporting
// healthy, its demand probe seeing no starvation, and eighty-three beads
// sitting ready against a lane that could not claim one of them.
//
// `check` was blind to it, because `check` only read environment variables. It
// could tell you GC_NOMAD_AGENT_IMAGE was unset and could not tell you the
// fleet had stopped being observable.

// TestForbiddenIsNotMistakenForAMissingJob is the distinction the probe rests
// on. It names a job that does not exist, so "gone" must read as SUCCESS — the
// read was permitted — while a refusal must not.
func TestForbiddenIsNotMistakenForAMissingJob(t *testing.T) {
	if !errJobGoneOrEmpty(errJobGone) {
		t.Error("a missing job must read as capability PRESENT: the probe names a job that is not there on purpose")
	}
	if !errJobGoneOrEmpty(nil) {
		t.Error("no error must read as capability present")
	}
	if errJobGoneOrEmpty(errJobForbidden) {
		t.Error("a 403 must NOT read as a missing job — that would turn the one definitive refusal into a pass")
	}
	// The SDK path yields a plain string, not a sentinel. That path is the one
	// is-running actually takes, and missing it was a real defect once.
	sdk := errors.New("Unexpected response code: 403 (Permission denied)")
	if errJobGoneOrEmpty(sdk) {
		t.Error("the SDK's plain-string 403 must not read as a missing job")
	}
	if !isForbiddenErr(sdk) {
		t.Error("the SDK's plain-string 403 must be recognised as forbidden")
	}
}

// TestAllocReadFailureNamesTheConsequence pins the message, because the message
// is the whole value. An operator who reads "403" alone fixes nothing; the
// actionable part is that minting still works, so NOTHING WILL LOOK BROKEN.
func TestAllocReadFailureNamesTheConsequence(t *testing.T) {
	var sb strings.Builder
	// Drive the message by making the probe take its forbidden branch through
	// the same formatting path the op uses.
	msg := allocReadForbiddenMessage("gc")
	sb.WriteString(msg)
	got := sb.String()

	for _, want := range []struct{ needle, why string }{
		{"403", "the status is not named"},
		{"report healthy", "the consequence — every session reporting healthy — is not stated"},
		{"no starvation", "the second-order effect, a pool that mints no replacement, is not stated"},
		{"read-job", "no remedy is named, so the operator cannot act on it"},
		{"gc", "the namespace is not named, so the operator does not know where to grant it"},
	} {
		if !strings.Contains(got, want.needle) {
			t.Errorf("the allocation-read refusal does not mention %q: %s\n%s", want.needle, want.why, got)
		}
	}
}
