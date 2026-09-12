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
// THIS TEST WAS DEAD. It anchored on the literal "/data/agent.exit" and
// t.Skip'd when it could not find it — "covered by
// TestCaptureIsWrittenWhereTheBoxCanWrite", which was false: that test proves
// the record's PATH, and nothing at all proved these two properties. The
// anchor had moved (the body holds the directory in $__gc_cap, exactly as the
// comment above records for the sibling test), so from then on this test
// skipped on every run and would have kept skipping if the stderr redirect
// came back. A guard that answers "skip" when its subject disappears is not a
// guard; the absence of the write is now a failure, which is the only reading
// under which this test means anything.
func TestCaptureFailureIsNotSwallowed(t *testing.T) {
	body := paneBody(t)
	const groupStart = `{ printf 'exit=`
	i := strings.Index(body, groupStart)
	if i < 0 {
		t.Fatalf("the pane body does not write an exit record at all, so this test's subject is gone "+
			"— TestCaptureIsWrittenWhereTheBoxCanWrite proves the record's PATH and does NOT prove "+
			"either property asserted here:\n%s", body)
	}
	// Scoped to the WHOLE write group — from the `{` that opens it to the
	// `|| true` that closes it — and not merely to the part after the `>`
	// redirection. A first cut of this scoped from the redirect onward, and a
	// mutation that inserted `2>/dev/null` just BEFORE the redirect passed it:
	// the swallow applies to the whole command wherever in it it is written.
	//
	// Command substitutions are removed before the check, because the group
	// legitimately contains `2>/dev/null` on two date(1) calls whose failure
	// genuinely does not matter (they fall back to "unknown"). Without that
	// removal the test reports a defect against a correct writer, and a test
	// that cannot tell the two apart gets ignored or deleted.
	end := strings.Index(body[i:], "|| true")
	if end < 0 {
		t.Fatalf("the record write is not closed by `|| true`, so a failed write fails the launch "+
			"— the box then dies for want of a diagnostic:\n%s", body[i:])
	}
	region := stripCommandSubstitutions(body[i : i+end])
	if strings.Contains(region, "2>") {
		t.Errorf("the record write redirects its own stderr away, so a failure to write is silent "+
			"and reads as a pane that never started — the exact ambiguity this record exists to "+
			"remove (it made a read-only alloc/logs indistinguishable from a pane that never ran):\n%s", region)
	}
}

// stripCommandSubstitutions removes every $( … ) span so a redirect INSIDE a
// substitution is not read as a redirect on the command that contains it. The
// spans here hold no nested parentheses; an unbalanced `$(` swallows the rest,
// which fails closed for this use — the checks that read the result are looking
// for something that must NOT be present.
func stripCommandSubstitutions(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "$(")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		rest := s[i+2:]
		j := strings.Index(rest, ")")
		if j < 0 {
			return b.String()
		}
		s = rest[j+1:]
	}
}
