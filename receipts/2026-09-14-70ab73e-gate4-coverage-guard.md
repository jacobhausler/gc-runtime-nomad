# Gate-4 coverage guard receipt: the guard can still fail

Re-runnable check: `receipts/2026-09-14-70ab73e-gate4-coverage-guard.md` records
that `TestGate4CausesAreEachNamed` in `runtime/gate4_coverage_test.go` passes at
HEAD **and** goes red when one anchor is removed from the code while its doc
comment stays. Taken 2026-09-14, pack HEAD
`70ab73e5771384b42f7f7a8ad82f4e961d85dab3` (`fix/t5-t6-liveness-and-placement`).

Redaction: the two `node=` values below, and the node name inside the first capture's
filename, are replaced by `<builder-node>`. A machine wrote those fields into the runner
capture on its own and a human copy-quoted them here, which is exactly what
`TestNoEstateLeakageInShippedFiles` refuses to ship: an adopter reading this receipt is
sent to a host that does not exist for them. Nothing else in this receipt is altered; the
alloc ids, timestamps, exit codes and test output are verbatim.

## Why this receipt exists

651a334 fixed a guard that could not fail: `TestGate4CausesAreEachNamed` read
whole `.go` files, comments included, and this package quotes its own error text
in doc comments — so an anchor deleted from the code was still "found" in the
comment describing it. The fix strips comments before matching. That fix has no
receipt of its own, and HEAD has moved 31 commits since it landed; a guard
that quietly regressed to vacuous would still report every cause as named.

## Green — the guard passes at HEAD

Never run Go on the Mac; both runs went through `ops/bin/nomad-run` on the
builders pool.

```
ops/bin/nomad-run --repo <pack> --rev 70ab73e5771384b42f7f7a8ad82f4e961d85dab3 -- \
  go -C runtime test -count=1 -v -run TestGate4CausesAreEachNamed ./...

=== RUN   TestGate4CausesAreEachNamed
--- PASS: TestGate4CausesAreEachNamed (0.04s)
PASS
ok  	github.com/gastownhall/gc-runtime-nomad	0.052s
```

`node=<builder-node> alloc=32aff5c5-1cca-32f7-5e80-dbe24be0a16f exit_code=0`
builder log: `ops/state/nomad-run/20260914T101849Z-builders-lane-<builder-node>-dispatch-1789381130-a99fd9ab.log` (the node name also sits inside that capture's filename, so it is redacted with the field)

## Red — and red with the comment left in place

The green row alone proves nothing: a vacuous guard is green too. So 651a334's
own mutation was re-run against this HEAD, in a throwaway `git worktree` at
70ab73e so the live tree was never touched, and removed afterwards.

`runtime/prime.go:538` (code) had its anchor replaced; `runtime/prime.go:566`
(the doc comment quoting the same sentence, the thing that used to rescue the
mutation) was deliberately left intact:

```
-			"died. The box's own exit record says: " + execExcerpt([]byte(lastLine(rec)))
+			"died. MUTATED exit record reads: " + execExcerpt([]byte(lastLine(rec)))
```

```
ops/bin/nomad-run --repo <worktree> -- \
  go -C runtime test -count=1 -run TestGate4CausesAreEachNamed ./...

--- FAIL: TestGate4CausesAreEachNamed (0.01s)
    gate4_coverage_test.go:140: gate 4 cause "the box's own exit record is quoted" is no longer named: expected a message containing "The box's own exit record says"
FAIL
FAIL	github.com/gastownhall/gc-runtime-nomad	0.014s
FAIL
```

`node=<builder-node> alloc=fe92e54d-309a-e92d-2bee-a0e439b7a510 exit_code=1`
builder log: `ops/state/nomad-run/20260914T101937Z-builders-dispatch-1789381177-873d4f61.log`

The failure names the cause it noticed, which is the second half of the point:
the guard has to say *which* cause lost its message, not merely be red.

## What is and is not covered

* Covered: comment-stripping is live, so the twelve-plus anchors are asserted on
  code, not documentation. `TestConcatNormalisationActuallyWorks` in the same
  file guards the concatenation join, without which every anchor would read as
  present or all as absent.
* Not covered, and left as-is deliberately: `stripComments` falls back to the
  raw body when a file fails to parse (`gate4_coverage_test.go:43-48`), so an
  unparseable file re-opens the vacuity this receipt just closed. It is a real
  hole, it is not reachable while `go vet` and the build pass — `check.sh` runs
  both — and it is not this bead's "cannot fail" condition, so it is recorded
  here rather than fixed under this receipt.
* No skip path: the test has no `t.Skip`, and `normalisedPackageSource` fails
  closed (`t.Fatal`) if it reads no package source. A missing anchor cannot be
  reported as a pass by not running.

Verdict: 651a334 still holds at HEAD. No guard change was needed.
