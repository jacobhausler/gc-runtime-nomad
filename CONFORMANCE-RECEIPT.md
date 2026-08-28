# Phase-1 conformance receipt (NRT-P1-90)

Commit: `fff465de4aabe0a82c4b46d7aca62b006d4390b7`
Generated: 2026-08-28T14:26:06Z

Full offline ladder — no live Nomad cluster or network required. A row
reading FAIL means the ladder is red at this commit; re-run `./receipt.sh`
after a fix and regenerate this file. Fixing a red ladder step is out of
scope for this receipt — bounce back to the owning bead.

| Ladder step | Result | Evidence |
|---|---|---|
| check | PASS | `gc runtime check` — 14 checks: 10 passed, 0 failed, 4 skipped |
| golden suite | PASS | `gc runtime conformance` — 8 requirements: 8 passed, 0 failed, 0 skipped |
| env probes | FAIL | `go test -run TestM3StagingReceiptWorkspaceProbe ./runtime` |
| L0-L2 | FAIL | `go vet ./... && go test ./...` — runtime + fakenomad modules |
| secrets-grep | FAIL | `go test -run TestM3StagingReceiptNoCanaryLeak ./runtime` |
