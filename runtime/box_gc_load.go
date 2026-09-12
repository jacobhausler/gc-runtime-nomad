package main

import (
	"regexp"
	"strings"
)

// loaderFailureLibrary matches the soname in a dynamic-loader failure. The
// number is the entire diagnosis: an ICU soname mismatch is not a version
// preference, it is an ABI the binary cannot satisfy.
var loaderFailureLibrary = regexp.MustCompile(`lib[A-Za-z0-9_+.-]+\.so(?:\.[0-9]+)+`)

// boxGCLoadFailure reports the gc in the box being unable to START, as opposed
// to being absent or answering an unrecognised shape.
//
// WHY IT EXISTS. toolchain/Dockerfile documents a real trade: with no gc baked
// into the image, the ICU agreement between the FETCHED gc and the image
// "moves to launch time, where the pack's in-box probe runs `gc version` and
// reports a gc that cannot start ... a mismatch is found on the first box
// rather than at build."
//
// It was not found on any box. warnIfBoxGCIsStale hands the probe output to
// gcCommitFromVersionJSON, which answers "unknown" for anything lacking a
// `"commit":"` field, and staleBoxGCWarning deliberately treats unknown as
// "could not tell", never "they differ" — the right call for an unrecognised
// SCHEMA and the wrong one for a binary that never ran. A loader failure is
// not JSON, so the single case the net was built for passed through it in
// silence.
//
// Distinguishing the three answers is the whole job here:
//
//	JSON with a commit   -> compare them (staleBoxGCWarning)
//	loader failure       -> THIS: the binary cannot start, and why
//	anything else        -> genuinely undetermined; stay quiet
//
// An ABSENT gc is a fourth case with its own message
// (gcAbsentWorkerSurfaceError) and a different remedy, so "not found" from the
// shell must not be claimed here.
func boxGCLoadFailure(out string) string {
	o := strings.TrimSpace(out)
	if o == "" {
		return ""
	}
	// The loader's own vocabulary. Both halves are required: "not found" alone
	// is what a SHELL says about a missing command, which is the absent-gc
	// case, not this one.
	loaderSpeak := strings.Contains(o, "error while loading shared libraries") ||
		strings.Contains(o, "cannot open shared object file")
	if !loaderSpeak {
		return ""
	}
	lib := loaderFailureLibrary.FindString(o)
	if lib == "" {
		lib = "a shared library it needs"
	}
	return "THE gc IN THE BOX CANNOT START: it needs " + lib + " and the image does not have it. " +
		"This is an ABI mismatch, not a version preference — the soname number IS the " +
		"incompatibility, and a binary in this state loads cleanly on whatever host built it " +
		"and only ever fails where it has to run. The fetched gc must be BUILT AGAINST the " +
		"agent image's own base; building it in a different distribution (or a different " +
		"release of the same one) produces exactly this. Nothing in the image can fix it. " +
		"The loader said: " + strings.TrimSpace(firstLine(o))
}

// firstLine keeps the report to the loader's own sentence rather than a whole
// captured stream.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
