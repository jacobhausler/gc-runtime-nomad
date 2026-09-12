package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// THE PACK'S OWN ACCOUNT OF WHAT IT DID, somewhere the caller cannot throw
// away.
//
// gc's exec transport turns any nonzero op exit into "tmux exited N" and drops
// this process's stdout and stderr on the floor. Everything the pack says
// about itself therefore vanishes at exactly the moment it matters: the
// decision log that records whether a box was adopted, reaped-and-replaced or
// refused, and the error that explains why an op failed, both arrive at the
// operator as a bare exit code.
//
// Measured today, twice: a config change took a lane down and every failure
// read "spawning prime window: tmux new-window exited 1:" with EMPTY output.
// The pack knew more than that and had already said it — to a stderr nobody
// keeps. Two diagnosis attempts were spent on a question the runtime could
// have answered itself.
//
// So this appends the same lines to a file under GC_NOMAD_SIDECAR_DIR, which
// the pack already owns and which outlives the process. stderr is unchanged:
// a caller that DOES keep it loses nothing.
const diagnosticsFile = "diagnostics.log"

// diagnosticsCap bounds the file so an unattended deployment cannot fill a
// disk with its own commentary. On exceeding it the file is truncated and
// restarted with a marker, rather than rotated — a runtime's diagnostic log is
// a tail, and a half-rotated one is worse than a short one.
const diagnosticsCap = 4 << 20 // 4 MiB

// currentOp is the RPP operation this process was invoked for, set once in
// run(). It is a package variable rather than plumbing because every sink
// write wants it and nothing else in the process ever changes it.
var currentOp string

// diagnosticsSink appends one line to the sidecar diagnostics file. Every
// failure to write is swallowed: a diagnostic that can break the op it is
// describing is worse than no diagnostic at all.
type diagnosticsSink struct{ dir string }

func newDiagnosticsSink(sidecarDir string) diagnosticsSink {
	return diagnosticsSink{dir: strings.TrimSpace(sidecarDir)}
}

func (d diagnosticsSink) write(op, line string) {
	if d.dir == "" {
		return
	}
	path := filepath.Join(d.dir, diagnosticsFile)
	if fi, err := os.Stat(path); err == nil && fi.Size() > diagnosticsCap {
		_ = os.WriteFile(path, []byte("-- diagnostics truncated at cap --\n"), 0o600)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck
	stamp := time.Now().UTC().Format(time.RFC3339)
	if op = strings.TrimSpace(op); op == "" {
		op = "-"
	}
	_, _ = fmt.Fprintf(f, "%s %s %s\n", stamp, op, strings.TrimRight(line, "\n"))
}

// durableWarnWriter tees a warning to stderr AND to diagnostics.log.
//
// WHY THIS EXISTS. An outside review of this pack observed that the durable
// log recorded failures — fail() writes every one — but NOT the warnings, and
// that the warnings are the half describing a lane that is alive and dark:
// absent secret keys, a half-configured city target, an in-box gc that cannot
// claim, and a liveness answer that is UNKNOWN rather than observed. Each of
// those went to os.Stderr alone, which is the exact stderr this pack documents
// gc as discarding. So the pack applied its own thesis everywhere except where
// it mattered most: at 3am the session fails, and at 9am the file is silent on
// every one of the four states that would have explained it.
//
// A failure that is loud and a dark lane that is quiet is the wrong way round.
// The dark lane is the one nobody can reconstruct after the fact.
//
// It is a writer rather than a logf call so the warning functions keep taking
// an io.Writer and stay unit-testable against a bytes.Buffer.
type durableWarnWriter struct {
	stderr io.Writer
	sink   diagnosticsSink
	op     string
}

func (d durableWarnWriter) Write(p []byte) (int, error) {
	n, err := d.stderr.Write(p)
	// The sink is line-oriented and timestamps each entry, so hand it trimmed
	// text: the writer's own trailing newline would otherwise land as a blank
	// line between every pair of records.
	if line := strings.TrimRight(string(p), "\n"); line != "" {
		d.sink.write(d.op, line)
	}
	return n, err
}

// durableWarn is the writer every warning in this package should use.
func (l *lifecycle) durableWarn() io.Writer {
	return durableWarnWriter{stderr: os.Stderr, sink: l.diagnostics, op: currentOp}
}
