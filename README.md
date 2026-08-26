# runtime-nomad

A Gas City **runtime pack**: it ships a runtime *executable*, not a service.
The executable `gc-runtime-nomad` speaks the Runtime Provider Protocol
(RPP v0). It answers the `protocol` handshake, the four session lifecycle
ops `start`/`stop`/`is-running`/`list-running` from `NRT-P1-03`, and — as of
`NRT-P1-08` — the provision/launch split and warm relaunch:
`provision`/`exec`/`relaunch`. Everything is implemented over Nomad job
dispatch/deregister/blocking reads against a parameterized parent job plus
the alloc-exec WebSocket, per
`research/outputs/04-proposed-architecture.md` §3/§4/§6/§7. As of
`NRT-P1-07`, `stop` also egresses each session's transcript/evidence files
via the Nomad client fs API before deregister, when egress is configured.
As of `NRT-P1-05`, `exec` plus the driving verbs (`nudge`, `peek`,
`interrupt`, `send-keys`, `clear-scrollback`) are realized as tmux commands
over the same exec mechanism. As of `NRT-P1-06`, `start`/`provision` accept
an optional workspace/secrets staging config on stdin (04 §5), materialized
into the alloc over tar-over-exec-stdin.

## Layout

```
runtime-nomad/
├── pack.toml       # declares [runtimes.nomad] -> gc-runtime-nomad
├── install.sh      # installs the executable onto PATH
├── conformance.sh  # gc runtime check + gc runtime conformance against an in-memory fake Nomad API
├── fakenomad/      # nested Go module: in-memory fake of the Nomad API this pack calls
│   ├── go.mod      # module github.com/gastownhall/gc-runtime-nomad/fakenomad
│   └── cmd/fakenomad/ # standalone process wrapping the fake server, for conformance.sh + CI
├── runtime/        # nested Go module (zero gascity imports, zero external deps in production code)
│   ├── go.mod      # module github.com/gastownhall/gc-runtime-nomad (test-only require on ../fakenomad)
│   ├── main.go     # RPP op dispatch + env config
│   ├── client.go   # Nomad API client (register/dispatch/deregister/blocking reads/alloc-exec WS)
│   ├── exec_ws.go  # client-side RFC 6455 frame codec for the alloc-exec WebSocket
│   ├── jobspec.go  # parent job spec builder (04 §4 job-template invariants)
│   ├── sidecar.go  # session -> child-job-ID binding store + launched marker (04 §1 sidecar state dir)
│   ├── staging.go  # wire start config, envArgvSafe classification, tar-over-exec-stdin builder (NRT-P1-06)
│   └── ops.go      # start/stop/is-running/list-running/provision/exec/relaunch/stage, stop-path fs egress (NRT-P1-07)
└── drills/         # nested Go module: L4 reconciler-sim harness (NRT-P2-06a, 08 §1.1) — see drills/README.md
```

## Use

```toml
# city.toml
[imports.runtime-nomad]
source = "<path-or-registry>/runtime-nomad"

[session]
provider = "nomad"
```

```bash
./install.sh   # put gc-runtime-nomad on PATH
gc doctor      # the pack-runtimes check verifies install + handshake
```

Lifecycle ops need Nomad API configuration on the environment:

| Variable | Required | Notes |
|----|----|----|
| `GC_NOMAD_ADDR` | yes | Absolute base URL of the Nomad API, e.g. `http://127.0.0.1:4646` |
| `GC_NOMAD_SIDECAR_DIR` | yes | Directory for the sidecar session→child-job-ID bindings (04 §1) |
| `GC_NOMAD_TOKEN` | no | ACL token, sent as `X-Nomad-Token` |
| `GC_NOMAD_NAMESPACE` | no | Nomad namespace (default `default`) |
| `GC_NOMAD_PARENT_JOB` | no | The city's parameterized parent job ID (default `gc-sessions`) |
| `GC_NOMAD_EGRESS_DIR` | no | Local directory for stop-path transcript/evidence egress (`NRT-P1-07`); unset disables egress |

## Conformance

```bash
GC_BIN=$(command -v gc) ./conformance.sh
```

`conformance.sh` builds the executable and an in-memory fake Nomad API
server (`fakenomad`), then runs `gc runtime check` and `gc runtime
conformance` through the full RPP lifecycle round-trip — no live Nomad
cluster or network required. This is the pack's CI gate (NRT-P1-04); `gc`
itself is a tool dependency installed with a pinned
`go install github.com/gastownhall/gascity/cmd/gc@<pin>`, never imported, so
the pack keeps zero gascity Go dependencies. Optional-op requirements
(`exec`, `provision`, ...) report SKIP since this phase only implements the
four lifecycle ops — later beads make them pass; this harness only runs
them.

### Conformance receipt

```bash
GC_BIN=$(command -v gc) ./receipt.sh
```

`receipt.sh` runs the full offline ladder in one hermetic pass — `check`
(`gc runtime check`), the golden suite (`gc runtime conformance`), the
env probe (`TestM3StagingReceiptWorkspaceProbe`), L0-L2
(`go vet`/`go test` across the `runtime` and `fakenomad` modules), and
secrets-grep (`TestM3StagingReceiptNoCanaryLeak`, zero planted-canary hits)
— then writes the per-item pass/fail result, linked to the commit it ran
at, to `CONFORMANCE-RECEIPT.md` (NRT-P1-90; 08 §1 pack-tier conformance
standard). Exits non-zero if any ladder item is red. Fixing a red ladder
step is out of scope for the receipt itself — bounce back to the owning
bead.

## RPP operations

| Op | Notes |
|----|-------|
| `protocol` | `{"version":0,"capabilities":["proc.provision","proc.exec","env.workspace"]}` |
| `provision` | Registers the parent job (idempotent upsert) if needed, then dispatches a tmux-only child for the session — no agent launched, sidecar launched marker stays unset. Rejects a session with a live child with an "already exists" error (04 §6 wire-contract constant). Reads an optional staging config (JSON) on stdin and materializes it (see `start`'s staging row) before returning. |
| `start` | `provision` + launch: dispatches, stages an optional workspace/secrets config read as JSON from stdin (`staging.go`'s `stageConfig`, NRT-P1-06 — workspace files land under `WorkDir` and non-`envArgvSafe` env entries land as files under `$NOMAD_SECRETS_DIR`, both via tar-over-exec-stdin, 04 §5), then execs the launch command (`tmux new-session -d -s main`, plus `-e KEY=VALUE` for any `envArgvSafe`-classified env entries) into the alloc, then sets the launched marker. Empty/absent stdin is a no-op config — pre-staging callers are unaffected. Same "already exists" rejection as `provision`. |
| `exec` | Runs a command inside the session's current alloc over the Nomad alloc-exec WebSocket and streams its stdout; **the op's own exit code is the remote command's exit code** (04 §3 exec row, RPP-CONN-001) — not the 0/1/2 lifecycle-op convention. Works regardless of the launched marker — a provisioned-but-not-launched box already answers exec (RPP-PROVISION-001). |
| `relaunch` | Re-execs the launch command into the SAME alloc — no fresh dispatch, no fresh env — then sets the launched marker (04 §7 warm relaunch, launch-only fingerprint drift). Fails if the session has no live alloc to relaunch into. |
| `nudge` / `peek` / `interrupt` / `send-keys` / `clear-scrollback` | The driving verbs (NRT-P1-05), realized as tmux commands sent into the session's tmux pane over the same exec mechanism. `nudge`/`interrupt` are best-effort — a not-found session answers success rather than an error, per the RPP's best-effort convention. |
| `stop` | Best-effort `tmux kill-session` for the agent's session before anything else (NRT-P2-06.1) — the task's own exit is independent of this (its supervisor script traps and exits on Nomad's own SIGTERM), but tmux's server is a detached daemon nothing else reaps. If `GC_NOMAD_EGRESS_DIR` is set, then copies the session's transcript/evidence files (via the Nomad client fs API) into it, retrying up to 3 times before giving up and marking the tombstone `evidence_lost` rather than wedging (R2b-04); a successful egress receipts completion in the sidecar instead. Then deregisters the session's child job without purge, confirms terminal via a blocking read, and tombstones the sidecar binding. Idempotent — a session with no binding is a no-op success, and a stop that fails after egress but before deregister does not re-attempt egress on retry. |
| `is-running` | Prints `true`/`false`. False whenever the launched marker is unset, even if the alloc is running (RPP-PROVISION-001: "provisioned, agent never launched" reads as not-running). Once launched, the honesty split (04 §6) applies — Nomad API unavailability never flips this to `false`, it answers last-known-good instead. |
| `list-running [prefix]` | Prints one running session name per line — launched sessions only. Enumerates the children-of-parent jobs list (`GET /v1/jobs?meta=true`, 04 §2.1 rule 2/3) rather than trusting the sidecar as the existence source, decodes each non-terminal child's `gc_session` Meta key, and — when a prefix argument is given — filters the decoded names to it (`ListRunning(prefix)`, E2a amendment A-1). The sidecar is still consulted for the launched marker, which the children list alone cannot answer. Exits 1 on any lookup error rather than returning a partial list. |

Every other operation exits 2 — the RPP forward-compatibility signal the
caller treats as a no-op success.

## Out of scope

- Retention policy for egressed transcript/evidence files (owner decision)
  and any egress sink beyond a local directory (`NRT-P1-07`) — no
  directory listing/discovery either, just the two well-known files
  (`ops.go`'s `egressFiles`).
- Remote artifact blocks and the env.ledger tunnel (`NRT-P1-06`) — staging
  only materializes what a caller sends inline on stdin; the launch command
  is still a bare `tmux new-session` (plus `-e` flags), not a real agent
  command line. The session task's own command (`jobspec.go`'s
  `sessionSupervisorScript`, NRT-P2-06.1) is a real long-lived
  trap-and-loop that keeps the alloc alive until stop — no longer the
  `/bin/true` placeholder that used to exit immediately on a real client —
  but it carries no agent itself; the agent is still launched afterward as
  a separate detached tmux-client command over alloc-exec.
- `PackOverlayDirs`/`OverlayDir` merge semantics (04 §5's overlay-precedence
  rules) — `stageConfig.Files` is this pack's own flat CopyFiles-analog
  channel; provider-side overlay resolution is not reimplemented.
- The fuller sidecar record (dispatch-attempt counter, disputed-children
  ledger, staleness datum) — this pack's sidecar binding is scoped to
  exactly what start/stop/is-running/list-running/provision/exec/relaunch
  need. The single-flight lock concurrent same-name `start`/`provision`
  needs (R2b-03) is a per-session `flock` on a file in the sidecar
  directory rather than a sidecar-record field — it has to survive across
  provider processes, since `gc` execs this binary fresh per op (a race is
  between two OS processes, never two goroutines in one).
