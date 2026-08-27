# Phase-1 conformance receipt (NRT-P1-90)

Commit: `2f74be3770f96d4323e70f762fbb4f01fd04059b`
Generated: 2026-08-27T21:02:17Z

Full offline ladder — no live Nomad cluster or network required. A row
reading FAIL means the ladder is red at this commit; re-run `./receipt.sh`
after a fix and regenerate this file. Fixing a red ladder step is out of
scope for this receipt — bounce back to the owning bead.

| Ladder step | Result | Evidence |
|---|---|---|
| check | PASS | `gc runtime check` — 14 checks: 10 passed, 0 failed, 4 skipped |
| golden suite | PASS | `gc runtime conformance` — 8 requirements: 8 passed, 0 failed, 0 skipped |
| env probes | PASS | `go test -run TestM3StagingReceiptWorkspaceProbe ./runtime` |
| L0-L2 | PASS | `go vet ./... && go test ./...` — runtime + fakenomad modules |
| secrets-grep | PASS | `go test -run TestM3StagingReceiptNoCanaryLeak ./runtime` |
