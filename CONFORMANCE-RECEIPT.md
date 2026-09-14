# Phase-1 conformance receipt

Commit: `fcdff87a427519fe431590bd84563e18f3c7b5fa`
gc: `/tmp/gc-receipt-75fbc2cf5b` — commit `75fbc2cf5b`, version `dev`, sha256 `0b9fd8ca11613857d613fefd964d5dc99923da4e5a8a0497bcceaa2294d48d51`
Generated: 2026-09-14T09:09:32Z
Conformance ladder exit: 0
Runner log: `toolchain/logs/2026-09-14-fcdff87-clean-machine.log`

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
| check | PASS | `gc runtime check` — 14 checks: 11 passed, 0 failed, 3 skipped |
| golden suite | PASS | `gc runtime conformance` — 8 requirements: 8 passed, 0 failed, 0 skipped |
| retry idempotency | PASS | provision/start re-drive rung — retry idempotency: 1 passed, 0 failed, 0 skipped |
| env probes | PASS | `go test -run TestM3StagingReceiptWorkspaceProbe ./runtime` |
| L0-L2 | PASS | `go vet ./... && go test ./...` — runtime + fakenomad + drills modules, plus a check that every go.mod has a leg |
| secrets-grep | PASS | `go test -run TestM3StagingReceiptNoCanaryLeak ./runtime` |
| provenance | PASS | `gc version --json` + sha256 of the resolved GC_BIN |
