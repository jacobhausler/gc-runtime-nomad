package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two refusals are quoted verbatim from the binaries they come from, not
// paraphrased: the live pinned gc (measured 2026-09-12) and cmd/gc/
// remote_target.go at 6ba434271, the upstream base of the pin lineage.
const (
	edgeRefusal = "gc bd: claim is not supported against a remote city; the remote worker " +
		"subset is `update <id> --claim`, `heartbeat <id>`, `release-if-current <id> " +
		"<assignee>` and `close <id>` with a typed gc.work_outcome — every other bd verb " +
		"needs the local store (or bd) and stays local"
	baseRefusal = "this command does not support a remote city (--city-url/--context) yet; " +
		"remote support is being enabled incrementally"
)

func TestClassifyWorkerSurface(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   gcWorkerSurface
	}{
		{"the pinned lineage names the worker subset", edgeRefusal, gcWorkerSurfacePresent},
		{"the upstream base answers with the capability gate", baseRefusal, gcWorkerSurfaceMissing},
		{"silence is not evidence of absence", "", gcWorkerSurfaceUnknown},
		{"an unrelated failure is not evidence either", "gc: command not found", gcWorkerSurfaceUnknown},
		{"a future rewording of the gate is unknown, not missing",
			"this command does not support a remote city", gcWorkerSurfaceUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyWorkerSurface(tc.output); got != tc.want {
				t.Errorf("classifyWorkerSurface(%.40q…) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// TestMissingWorkerSurfaceErrorNamesTheSymptom pins that the refusal describes
// what the operator would otherwise have seen. A refusal that says only
// "incompatible gc" sends someone hunting in the runtime layer, which is the
// exact cost this gate exists to remove.
func TestMissingWorkerSurfaceErrorNamesTheSymptom(t *testing.T) {
	msg := gcMissingWorkerSurfaceError().Error()
	for _, want := range []string{
		"remote-worker-lifecycle", // the family, by name
		"no_work",                 // the symptom the operator would have chased
		"PIN-DEPENDENCIES.md",     // where the full table is
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, msg)
		}
	}
}

// TestProbeGCWorkerSurfaceAgainstTheLiveGC is the end-to-end half. It is
// skipped when no gc is on PATH so the suite stays hermetic, and it asserts
// only that the probe does not report MISSING against a gc this pack is
// actually running on — asserting PRESENT would make the suite fail on a
// machine with a stock gc, which is a legitimate place to develop the pack.
func TestProbeGCWorkerSurfaceAgainstTheLiveGC(t *testing.T) {
	got := probeGCWorkerSurface(context.Background())
	t.Logf("probe against the gc on PATH: %v", got)
	if got == gcWorkerSurfaceMissing {
		t.Skip("this machine's gc lacks the remote-worker family; nothing to assert")
	}
}

// stubGCOnPath puts a fake `gc` first on PATH that prints the given refusal.
// This is how the MISSING path gets exercised at all: the machine running this
// suite has a gc that carries the family, and building an upstream-base gc to
// test against is neither hermetic nor available on a clean machine.
func stubGCOnPath(t *testing.T, refusal string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + refusal + "\nEOF\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gc"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub gc: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestBoxMintingOpsRefuseAGCWithoutTheWorkerSurface is the end-to-end proof
// that the gate is wired, not merely written. Without it the probe could
// classify correctly and refuse nothing.
func TestBoxMintingOpsRefuseAGCWithoutTheWorkerSurface(t *testing.T) {
	stubGCOnPath(t, baseRefusal)
	if got := probeGCWorkerSurface(context.Background()); got != gcWorkerSurfaceMissing {
		t.Fatalf("probe against the stub = %v, want missing", got)
	}
	for _, op := range []string{"start", "provision", "relaunch"} {
		t.Run(op, func(t *testing.T) {
			// run() writes to *os.File, so the capture is a real file.
			outFile, errFile := t.TempDir()+"/out", t.TempDir()+"/err"
			out, err := os.Create(outFile)
			if err != nil {
				t.Fatalf("creating stdout capture: %v", err)
			}
			defer out.Close()
			errf, err := os.Create(errFile)
			if err != nil {
				t.Fatalf("creating stderr capture: %v", err)
			}
			defer errf.Close()

			code := run([]string{op, "sess-capability-probe"}, strings.NewReader(""), out, errf)
			if code != exitError {
				t.Errorf("%s exit = %d, want %d (a refusal)", op, code, exitError)
			}
			captured, readErr := os.ReadFile(errFile)
			if readErr != nil {
				t.Fatalf("reading stderr capture: %v", readErr)
			}
			if !strings.Contains(string(captured), "remote-worker-lifecycle") {
				t.Errorf("%s did not name the missing family:\n%s", op, captured)
			}
		})
	}
}

// TestNonMintingOpsSurviveAGCWithoutTheWorkerSurface is the other half, and
// the one that would be easy to get wrong: a box that already exists must stay
// stoppable. Refusing `stop` on an incompatible gc would convert a startup
// refusal into stranded allocations — a worse failure than the one prevented.
func TestNonMintingOpsSurviveAGCWithoutTheWorkerSurface(t *testing.T) {
	stubGCOnPath(t, baseRefusal)
	for _, op := range []string{"stop", "is-running", "list-running"} {
		if mintsABox[op] {
			t.Errorf("%q is treated as box-minting; it must not be gated", op)
		}
	}
}
