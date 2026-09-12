package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The four states below all describe a box that comes up, looks healthy, and
// cannot do work. An outside reviewer of this pack found that every one of
// them was written to os.Stderr alone — the stderr this pack's own
// documentation says gc discards — while fail() wrote durably. So the durable
// record held the failures and was silent on the dark lanes, which is the
// wrong way round: a failure announces itself, a dark lane does not.
//
// This guard is a source assertion rather than a behavioural one because
// three of the four need a live alloc to reach. A source assertion is weaker,
// so it is written to fail on the SPECIFIC regression — a call site handing
// these warnings os.Stderr again — rather than on prose.

// darkLaneWarnCallSites are the exact spellings that must not come back.
var darkLaneWarnCallSites = []struct{ call, why string }{
	{"warnAbsentSecretKeys(os.Stderr", "a seat whose credentials are absent launches and cannot work"},
	{"warnIncompleteCityTarget(os.Stderr", "a half-configured city target looks healthy and fails every in-box gc call"},
	{"warnUnknownLiveness(os.Stderr", "an UNKNOWN liveness answer reads as running; a revoked token makes the whole fleet permanently green"},
}

func TestDarkLaneWarningsAreDurable(t *testing.T) {
	src, err := os.ReadFile("ops.go")
	if err != nil {
		t.Fatalf("reading ops.go: %v", err)
	}
	s := string(src)

	for _, c := range darkLaneWarnCallSites {
		if strings.Contains(s, c.call) {
			t.Errorf("%s writes to os.Stderr only — it must use l.durableWarn(). Why it matters: %s",
				c.call, c.why)
		}
	}

	// The in-box gc probe is the fourth, and it has two halves: the warning
	// itself, and the case where the probe could not run at all. A probe that
	// cannot run must not be indistinguishable from one that passed.
	probe := s[strings.Index(s, "func (l *lifecycle) warnIfBoxGCCannotClaim"):]
	if end := strings.Index(probe, "\nfunc "); end > 0 {
		probe = probe[:end]
	}
	if probe == "" {
		t.Fatal("warnIfBoxGCCannotClaim not found — this guard is pinned to a function that moved")
	}
	if strings.Contains(probe, "os.Stderr") {
		t.Error("the in-box gc capability warning writes to os.Stderr only; use l.logDecision")
	}
	if strings.Count(probe, "l.logDecision") < 2 {
		t.Error("the in-box gc probe must record BOTH a gc that cannot claim AND a probe that " +
			"could not run; a probe that cannot run is not a probe that passed")
	}
}

// TestDurableWarnWriterTeesToTheSink is the behavioural half: the writer must
// reach the file, not merely exist.
func TestDurableWarnWriterTeesToTheSink(t *testing.T) {
	dir := t.TempDir()
	var captured strings.Builder
	w := durableWarnWriter{stderr: &captured, sink: newDiagnosticsSink(dir), op: "provision"}

	if _, err := w.Write([]byte("warning: session \"s1\" looks healthy and cannot claim\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if !strings.Contains(captured.String(), "cannot claim") {
		t.Errorf("the warning did not reach stderr: %q", captured.String())
	}
	body, err := os.ReadFile(filepath.Join(dir, diagnosticsFile))
	if err != nil {
		t.Fatalf("reading %s: %v — the warning was not durable", diagnosticsFile, err)
	}
	if !strings.Contains(string(body), "cannot claim") {
		t.Errorf("%s does not carry the warning:\n%s", diagnosticsFile, body)
	}
	if !strings.Contains(string(body), "provision") {
		t.Errorf("%s does not attribute the warning to its op:\n%s", diagnosticsFile, body)
	}
	// A trailing newline from the writer must not become a blank record.
	if strings.Contains(string(body), "\n\n") {
		t.Errorf("%s has a blank record between entries:\n%q", diagnosticsFile, body)
	}
}
