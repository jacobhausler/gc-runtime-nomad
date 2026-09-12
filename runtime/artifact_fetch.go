package main

import (
	"fmt"
	"strings"
)

// knownArtifacts names the artifact URLs this pack declares for the agent task,
// so a fetch failure can say WHICH artifact failed and, more usefully, where
// its URL came from. That provenance is the whole diagnosis: one of these URLs
// is configured by a human and the other is DERIVED from the running gc, so the
// same 404 has two completely different remedies.
type knownArtifacts struct {
	// gc is the per-box gc download, derived from the commit of the gc driving
	// the pack. Nobody configures it per box, so nobody can point at a
	// jobspec to fix it.
	gc string
	// imageTar is the agent image tarball, which IS configured by hand.
	imageTar string
}

// isArtifactFetchFailure reports whether a task event describes Nomad failing
// to stage an artifact. Matched on the client's own vocabulary rather than one
// exact string, the same way isImagePullFailure and isOOMFailure are.
//
// This has to be its own cause because the generic sentence for it is actively
// misleading. Nomad fetches and checksums every artifact BEFORE the task runs,
// so an allocation that dies here has executed nothing at all: no image, no
// shell, no gc, no agent. Reported as `the "agent" task never started (state
// "pending")` it reads as a start fault and sends the operator into the box to
// look for something that was never there.
func isArtifactFetchFailure(event string) bool {
	e := strings.ToLower(event)
	switch {
	case strings.Contains(e, "failed artifact download"),
		strings.Contains(e, "failed to download artifact"),
		strings.Contains(e, "failed to parse artifact"),
		strings.Contains(e, "checksums did not match"),
		strings.Contains(e, "checksum mismatch"):
		return true
	}
	return false
}

// isChecksumMismatch separates a fetch that ARRIVED from one that 404'd. The
// bytes downloading but failing verification is not a missing publish: it is a
// binary and a sidecar that disagree, which means something republished one of
// the two without the other. Sending that operator to "publish for this commit"
// would have them republish the very thing that is already there.
func isChecksumMismatch(event string) bool {
	e := strings.ToLower(event)
	return strings.Contains(e, "checksums did not match") || strings.Contains(e, "checksum mismatch")
}

// describeArtifactFetchFailure states the cause at the layer that knows the one
// thing the event itself cannot: which of these URLs is a human's to fix and
// which is a function of the live pin.
//
// THE 2026-09-12 FLEET OUTAGE, from the consuming side. A pin roll installed a
// new gc, the swap door's artifact publish was skipped on its way out of an
// inconclusive readiness probe, and every box in the fleet then fetched a URL
// derived from the new commit, took a 404, and died before its task started.
// Nothing in the box was wrong and nothing in the jobspec was wrong; the
// remedy was entirely on the publishing side, for one specific commit. An
// operator reading "never started" has no way to reach that.
func describeArtifactFetchFailure(taskName, event string, known knownArtifacts) string {
	which, provenance := classifyArtifact(event, known)
	head := fmt.Sprintf("the %q task never ran: Nomad could not stage %s, and artifacts are "+
		"fetched and checksummed BEFORE the task starts, so nothing in the box has executed — "+
		"this is a FETCH fault, not a start fault and not a fault in the image, the agent or gc",
		taskName, which)
	if isChecksumMismatch(event) {
		// The provenance clause is deliberately NOT appended here. It ends in
		// "publish for this exact commit", which on a mismatch is the wrong
		// instruction — the commit IS published; what is wrong is that the
		// binary and its sidecar do not agree. Appending both produced one
		// sentence carrying two contradictory remedies, which a test caught
		// before this shipped.
		return fmt.Sprintf("%s. The bytes ARRIVED and failed verification, so this is not a "+
			"missing publish: the artifact and its sha256 sidecar disagree, which means one of "+
			"the two was republished without the other. Republish BOTH from the same bytes: %s",
			head, event)
	}
	return fmt.Sprintf("%s. %s: %s", head, provenance, event)
}

// classifyArtifact names the failing artifact and the sentence that says whose
// job it is to fix. Falls back to the honest non-answer when the event names a
// URL this pack did not declare, rather than guessing at the gc remedy and
// sending the operator to publish something unrelated.
func classifyArtifact(event string, known knownArtifacts) (which, provenance string) {
	if s := strings.TrimSpace(known.gc); s != "" && strings.Contains(event, s) {
		return fmt.Sprintf("the per-box gc (%s)", s),
			"THAT URL IS NOT CONFIGURED ANYWHERE — it is derived from the commit of the gc " +
				"driving this pack, so it changes on every pin roll and the fix is always on the " +
				"PUBLISHING side, for this exact commit. Either publish the linux/amd64 gc and its " +
				".sha256 sidecar for that commit, or unset " + envGCArtifactTemplate +
				" and let the box run the gc baked into the image (which will then be whatever the " +
				"image carries, and may be several rolls stale)"
	}
	if s := strings.TrimSpace(known.imageTar); s != "" && strings.Contains(event, s) {
		return fmt.Sprintf("the agent image tarball (%s)", s),
			"That URL comes from " + envAgentImageTar + ", so it IS yours to fix: make it " +
				"fetchable from the client, or unset it and let the driver resolve the image by " +
				"name instead"
	}
	return "one of its artifacts",
		"This pack did not declare that URL, so it is neither the derived gc download nor " +
			envAgentImageTar + " — read the URL in the event below and fix it where it is set"
}
