package main

import (
	"strings"
	"testing"
)

// MEASURED IN A LIVE BOX, 2026-09-12, with stderr made visible:
//
//	$ printf "exit=%s\n" 7 > "$NOMAD_ALLOC_DIR/logs/agent.exit.$$"
//	/bin/sh: 4: cannot create /alloc/logs/agent.exit.243: Read-only file system
//
//	WRITABLE   /alloc          read-only  /alloc/logs
//	WRITABLE   /alloc/data     WRITABLE   /local, /secrets, /tmp
//
// Nomad bind-mounts alloc/logs READ-ONLY into the task — it owns the files in
// there and its own log rotation writes them. The directory reports 0777,
// which is why this survived inspection: the permission bits say yes and the
// mount says no.
//
// So alloc/logs/agent.exit could never be written by anything running in the
// box — not by this pack, and not by the "deployment bootstrap
// ops/nomad/agent-launch.sh" the egress doc credits, which is why the record
// has never existed in this estate. Four places named that path: the pane
// writer, egressFiles, prime.go's reader, and the vector source.
//
// alloc/data is writable and is where this pack ALREADY reads a box-written
// file from (alloc/data/evidence.json in egressFiles), so it is the path with
// precedent, not a new invention.

func TestCaptureIsWrittenWhereTheBoxCanWrite(t *testing.T) {
	body := paneBody(t)
	if strings.Contains(body, "/logs/agent.exit") || strings.Contains(body, `logs"`) {
		t.Errorf("the pane writes its record under alloc/logs, which Nomad mounts read-only: the write fails and no record is ever produced:\n%s", body)
	}
	// Asserted as two properties, not as one literal path: the body holds the
	// directory in a variable, so a substring test for "/data/agent.exit" went
	// red against a CORRECT writer. Same mistake as the ${PATH} guard — check
	// what the shell will do, not how the string happens to be spelled.
	if !strings.Contains(body, `${NOMAD_ALLOC_DIR:-/tmp}/data`) {
		t.Errorf("the pane's capture directory is not the writable alloc/data:\n%s", body)
	}
	if !strings.Contains(body, "/agent.exit") {
		t.Errorf("the pane does not write an agent.exit at all:\n%s", body)
	}
}

// Every reader must agree with the writer. A writer moved without its readers
// produces a record nobody looks at, which is the same as no record.
func TestEveryCaptureReaderAgreesWithTheWriter(t *testing.T) {
	for _, f := range egressFiles {
		if strings.Contains(f.allocPath, "agent.") && strings.HasPrefix(f.allocPath, "alloc/logs/") {
			t.Errorf("egressFiles reads %q, under the read-only alloc/logs; nothing can have written it", f.allocPath)
		}
	}
	var sawExit bool
	for _, f := range egressFiles {
		if f.allocPath == "alloc/data/agent.exit" {
			sawExit = true
		}
	}
	if !sawExit {
		t.Error("egressFiles does not copy alloc/data/agent.exit, so the record dies with the box")
	}
}

// The pane body must not hide its own write failure. Swallowing stderr here is
// what made a Read-only file system error indistinguishable from "the pane
// never ran" — the exact ambiguity this record exists to remove. The pane's
// own output is where an operator already looks (peek, capture-pane), so the
// error belongs there; `|| true` still keeps a failed write from failing the
// launch.
func TestCaptureFailureIsNotSwallowed(t *testing.T) {
	body := paneBody(t)
	i := strings.Index(body, "/data/agent.exit")
	if i < 0 {
		t.Skip("covered by TestCaptureIsWrittenWhereTheBoxCanWrite")
	}
	tail := body[i:]
	if strings.Contains(tail, "2>/dev/null") {
		t.Errorf("the record write discards its own stderr, so a failure to write is silent and reads as a pane that never started:\n%s", tail)
	}
	if !strings.Contains(tail, "|| true") {
		t.Errorf("a failed record write must not fail the launch:\n%s", tail)
	}
}
