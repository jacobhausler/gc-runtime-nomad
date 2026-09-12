package main

import (
	"strings"
	"testing"
)

// THE HOLE IN A DOCUMENTED SAFETY NET.
//
// toolchain/Dockerfile explains, carefully and honestly, that in omit-gc mode
// the ICU agreement between the fetched gc and the image "moves to launch
// time, where the pack's in-box probe runs `gc version` and reports a gc that
// cannot start. That is a real trade and not a free one: a mismatch is found on
// the first box rather than at build."
//
// It was not found on the first box, or any box. warnIfBoxGCIsStale runs
// `gc version --json --long` in the box and passes the output to
// gcCommitFromVersionJSON, which returns "unknown" for anything without a
// `"commit":"` field; staleBoxGCWarning treats unknown as "could not tell,
// never they differ" and returns no warning. A gc that cannot LOAD produces a
// dynamic-loader error, not JSON — so the one case the net was built for fell
// straight through it in silence.
//
// Measured today, on a gc built in the wrong container:
//
//	/tmp/gcverify: error while loading shared libraries: libicui18n.so.76:
//	cannot open shared object file: No such file or directory
//
// The agent image is ubuntu 24.04 (libicui18n.so.74), the builders container is
// Debian trixie (76), toolchain/Dockerfile's BUILD_IMAGE is bookworm (72). The
// binary loads on its build host every time; it is only ever wrong where it has
// to run, which is why "could not tell" is exactly the wrong answer.

func TestBoxGCLoaderFailureIsNamed(t *testing.T) {
	out := "/local/gcbin/gc: error while loading shared libraries: libicui18n.so.76: cannot open shared object file: No such file or directory"
	w := boxGCLoadFailure(out)
	if w == "" {
		t.Fatal("a gc that cannot load reports nothing, so the documented launch-time net does not exist")
	}
	// It must name the library, because that string is the entire diagnosis:
	// the number IS the ABI mismatch.
	if !strings.Contains(w, "libicui18n.so.76") {
		t.Errorf("the report does not name the missing library, which is the whole diagnosis: %q", w)
	}
	// And it must say where the remedy is. An adopter who reads "cannot load"
	// goes looking in the image; the fix is to rebuild the ARTIFACT against
	// the image's base.
	if !strings.Contains(strings.ToLower(w), "built against") {
		t.Errorf("the report does not say the artifact must be built against the image's own base: %q", w)
	}
}

// It must NOT fire on the two outputs that are genuinely undetermined, or it
// becomes noise on every healthy box and stops being read.
func TestBoxGCLoadFailureIgnoresHealthyAndUnknown(t *testing.T) {
	for name, out := range map[string]string{
		"healthy json":   `{"commit":"75fbc2cf5b","ok":true}`,
		"empty":          "",
		"unrecognised":   "some future gc subcommand output nobody has seen",
		"no such binary": "sh: 1: gc: not found",
	} {
		if w := boxGCLoadFailure(out); w != "" {
			t.Errorf("%s: reported a loader failure where there is none: %q", name, w)
		}
	}
}

// The 'gc is absent' case is a DIFFERENT cause with a different remedy, and the
// pack already has a message for it. This must not claim that one.
func TestBoxGCLoadFailureIsNotTheAbsentCase(t *testing.T) {
	w := boxGCLoadFailure("sh: 1: gc: not found")
	if w != "" {
		t.Errorf("a missing gc is not a load failure; that is the absent-gc case: %q", w)
	}
}
