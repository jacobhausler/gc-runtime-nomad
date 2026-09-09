# Phase-1 conformance receipt (NRT-P1-90)

Commit: `501fc915ef5977aec06d5128ac04e2abb9bab8f4`
gc: `/tmp/gc-pin/gc` — commit `e52f96f62`, version `dev`, sha256 `0f3b9e3a3e115ba95096ce9ea01e08d4f97b397243ddca0ec259de6e0c2b25a7`
Generated: 2026-09-09T08:21:43Z
Conformance ladder exit: 0

Full offline ladder — no live Nomad cluster or network required. A row
reading FAIL means the ladder is red at this commit; re-run `./receipt.sh`
after a fix and regenerate this file. Fixing a red ladder step is out of
scope for this receipt — bounce back to the owning bead.

The rows report what `gc runtime check` / `gc runtime conformance` said
and what the ladder's own rungs did; `receipt.sh` exits non-zero if any row
is FAIL **or** the ladder exited non-zero. Regenerate both twins after any
pin roll or pack release — a receipt whose gc commit no longer matches the
pin is stale by construction:

```
GC_BIN=$(command -v gc) ./receipt.sh
GC_BIN=$(command -v gc) ./receipt.sh --json-stdout > CONFORMANCE-RECEIPT.json
```

| Ladder step | Result | Evidence |
|---|---|---|
| check | PASS | `gc runtime check` — 14 checks: 10 passed, 0 failed, 4 skipped |
| golden suite | PASS | `gc runtime conformance` — 8 requirements: 8 passed, 0 failed, 0 skipped |
| retry idempotency | PASS | provision/start re-drive rung — retry idempotency: 1 passed, 0 failed, 0 skipped |
| env probes | PASS | `go test -run TestM3StagingReceiptWorkspaceProbe ./runtime` |
| L0-L2 | PASS | `go vet ./... && go test ./...` — runtime + fakenomad modules |
| secrets-grep | PASS | `go test -run TestM3StagingReceiptNoCanaryLeak ./runtime` |
| provenance | PASS | `gc version --json` + sha256 of the resolved GC_BIN |
