# drills — the L4 reconciler-sim harness (NRT-P2-06a)

08's own words for what an L4 drill is built from: "the L4 harness = pack
CLI + scripted reconciler-sim driver, standing in for GC until L5" (08 §1.1
"controller-restart mid-exec" row). This module is that driver. It never
reimplements a lifecycle op — every mutation goes through the real,
unmodified `gc-runtime-nomad` binary (`../runtime`) as a subprocess, exactly
how GC itself execs it (`main.go`'s "gc execs the binary directly" calling
convention). It only adds two things a real reconciler drill needs beyond
what RPP ops expose: independent read-only Nomad observation
(`reconcilersim.Client`), and the classification/scripting scaffolding to
turn "kill something, then look" into a pass/fail verdict
(`reconcilersim.ClassifyDeath` and friends in `classify.go`).

## Layout

```
drills/
├── go.mod                          # nested Go module; test-only require on ../fakenomad
├── reconcilersim/                  # the harness package
│   ├── client.go                    # read-only Nomad API client, native mTLS support
│   ├── classify.go                  # agent-death vs box-death, honesty split, staleness, replacement-alloc count, egress ordering
│   ├── config.go                    # env var contract (GC_NOMAD_*, GC_NOMAD_TLS_*, GC_RUNTIME_NOMAD_BIN)
│   ├── tlsproxy.go                  # local mTLS-terminating reverse proxy fronting the (TLS-less) pack CLI subprocess
│   ├── driver.go                    # Driver (execs the pack CLI), Step/RunScript scripted-drill scaffolding
│   ├── scenarios.go                 # LifecycleRoundtripSteps — the worked example scenario
│   ├── classify_test.go             # pure unit tests of the classification decision table
│   └── driver_test.go               # offline: fakenomad + the real built pack CLI binary
└── cmd/reconcilersim-drill/
    └── main.go                      # CLI: `reconcilersim-drill lifecycle-roundtrip`
```

## Two ways to reach a target

- **Offline (fakenomad)**: point `GC_NOMAD_ADDR` at a running `fakenomad`
  instance (see `../conformance.sh` for how to start one), leave the
  `GC_NOMAD_TLS_*` vars unset. `driver_test.go` runs this way — no cluster,
  fake or real, is a live dependency.
- **L4 (real T1 lab)**: set `GC_NOMAD_ADDR` to the lab's real
  `https://...` API address, plus all three `GC_NOMAD_TLS_*` vars (the
  NRT-P2-02 mTLS baseline: CA cert, client cert, client key). `Driver.New`
  then starts a local plain-HTTP proxy that terminates real mTLS to the lab
  on the pack CLI subprocess's behalf — the CLI itself still has zero TLS
  config (an intentional pack boundary, NRT-P1-04/NRT-P2-05); this
  harness's own `reconcilersim.Client` (used for every direct observation
  read) talks mTLS to the lab directly, no proxy involved.

## Env vars

| Variable | Required | Notes |
|---|---|---|
| `GC_NOMAD_ADDR` | yes | Nomad API base URL — fakenomad's URL offline, or the real lab's `https://` address at L4 |
| `GC_NOMAD_TOKEN` | no | ACL token |
| `GC_NOMAD_NAMESPACE` | no | Nomad namespace (default `default`) |
| `GC_NOMAD_PARENT_JOB` | no | Parent job ID (default `gc-sessions`) |
| `GC_NOMAD_SIDECAR_DIR` | yes (pack CLI's own requirement) | passed straight through to the pack CLI subprocess |
| `GC_NOMAD_TLS_CACERT` / `GC_NOMAD_TLS_CERT` / `GC_NOMAD_TLS_KEY` | L4 only | mTLS materials (07-provider-implementation-plan.md §3's planned names); all three or none |
| `GC_RUNTIME_NOMAD_BIN` | no | path to the `gc-runtime-nomad` binary (default: `gc-runtime-nomad` on `PATH`) |

## Running the shipped self-check

```bash
go build -o /tmp/gc-runtime-nomad ../runtime
GC_NOMAD_ADDR=<target> GC_NOMAD_SIDECAR_DIR=$(mktemp -d) \
  GC_RUNTIME_NOMAD_BIN=/tmp/gc-runtime-nomad \
  go run ./cmd/reconcilersim-drill lifecycle-roundtrip
```

Prints a JSON `Report` (one entry per scripted step) to stdout and exits
non-zero if any step failed.

## Writing a per-drill scenario

Each 08 §1.1 L4 drill bead (`fnrt-o37.3` .. `fnrt-o37.14`) is expected to
script its own scenario as a short Go program (or `_test.go`, offline)
importing this package directly — `reconcilersim.New`, `[]reconcilersim.Step`,
`reconcilersim.RunScript`, and whichever `classify.go` helper matches its
drill row's pass criterion. `scenarios.go`'s `LifecycleRoundtripSteps` is a
complete worked example of that shape. The one thing this harness
deliberately has no opinion on is how a real box/agent/link actually gets
killed at L4 — that's lab-access-specific and belongs in each drill bead's
own `Step.Inject`, not in this shared package.

## Out of scope (this bead, NRT-P2-06a)

- Executing any of the eleven §1.1 drills themselves against the real T1
  lab — that's the twelve blocked `fnrt-o37.3`..`fnrt-o37.14` beads' own
  scope, each producing its own runbook receipt.
- Adding TLS/mTLS support to the production `../runtime/client.go` — that
  boundary was deliberately drawn at NRT-P1-04 and reaffirmed at NRT-P2-05;
  this harness reaches the lab by fronting the unmodified pack CLI with its
  own local proxy instead (`tlsproxy.go`).
- `GC_NOMAD_NODE_POOL` / the T1 lab's `lab-session` node-pool mismatch
  (recorded, not fixed, in `ops/receipts/nrt-p2-05-fakenomad-reconcile.md`
  in the westlands city repo) — a real drill run against the current lab
  topology will need that resolved first; this harness's job is being
  ready the moment it is.
