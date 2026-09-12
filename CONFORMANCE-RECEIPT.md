# Phase-1 conformance receipt

Commit: `6e280bf6a5ee08a8db5e137f4c90a3b845232a4c`
gc: `/cache/gc-receipt-0d3256df86` — commit `0d3256df86c5ff8d8355ca5b905c78b0cb6816da`, version `dev`, sha256 `4be57a822f1e5c48497f68f11a786cc247ad4a2bee051117dab63fb75b957fad`
Generated: 2026-09-10T04:49:53Z
Conformance ladder exit: 0

Full offline ladder — no live Nomad cluster or network required. A row
reading FAIL means the ladder is red at this commit; re-run `./receipt.sh`
after a fix and regenerate this file. Fixing a red ladder step is out of
scope for this receipt — bounce it back to whoever owns the failing rung.

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
