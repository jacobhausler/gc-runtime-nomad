# runtime-nomad

A Gas City **runtime pack**: it ships a runtime *executable*, not a service.
The executable `gc-runtime-nomad` speaks the Runtime Provider Protocol
(RPP v0). It answers the `protocol` handshake, the four session lifecycle
ops `start`/`stop`/`is-running`/`list-running` from `NRT-P1-03`, and — as of
`NRT-P1-08` — the provision/launch split and warm relaunch:
`provision`/`exec`/`relaunch`. Everything is implemented over Nomad job
dispatch/deregister/blocking reads against a parameterized parent job plus
the alloc-exec WebSocket, per
`research/outputs/04-proposed-architecture.md` §3/§4/§6/§7. The remaining
driving verbs (`nudge`, `peek`, ...) and staging are out of scope for this
phase and exit 2.

## Layout

```
runtime-nomad/
├── pack.toml       # declares [runtimes.nomad] -> gc-runtime-nomad
├── install.sh      # installs the executable onto PATH
├── fakenomad/      # nested Go module: in-memory fake of the Nomad API this pack calls
│   └── go.mod      # module github.com/gastownhall/gc-runtime-nomad/fakenomad
└── runtime/        # nested Go module (zero gascity imports, zero external deps in production code)
    ├── go.mod      # module github.com/gastownhall/gc-runtime-nomad (test-only require on ../fakenomad)
    ├── main.go     # RPP op dispatch + env config
    ├── client.go   # Nomad API client (register/dispatch/deregister/blocking reads/alloc-exec WS)
    ├── exec_ws.go  # client-side RFC 6455 frame codec for the alloc-exec WebSocket
    ├── jobspec.go  # parent job spec builder (04 §4 job-template invariants)
    ├── sidecar.go  # session -> child-job-ID binding store + launched marker (04 §1 sidecar state dir)
    └── ops.go      # start/stop/is-running/list-running/provision/exec/relaunch
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

## RPP operations

| Op | Notes |
|----|-------|
| `protocol` | `{"version":0,"capabilities":["proc.provision","proc.exec"]}` |
| `provision` | Registers the parent job (idempotent upsert) if needed, then dispatches a tmux-only child for the session — no agent launched, sidecar launched marker stays unset. Rejects a session with a live child with an "already exists" error (04 §6 wire-contract constant). |
| `start` | `provision` + launch: dispatches, then execs the launch command (`tmux new-session -d -s main`) into the alloc, then sets the launched marker. Same "already exists" rejection as `provision`. |
| `exec` | Runs a command inside the session's current alloc over the Nomad alloc-exec WebSocket and streams its stdout; **the op's own exit code is the remote command's exit code** (04 §3 exec row, RPP-CONN-001) — not the 0/1/2 lifecycle-op convention. Works regardless of the launched marker — a provisioned-but-not-launched box already answers exec (RPP-PROVISION-001). |
| `relaunch` | Re-execs the launch command into the SAME alloc — no fresh dispatch — then sets the launched marker (04 §7 warm relaunch, launch-only fingerprint drift). Fails if the session has no live alloc to relaunch into. |
| `stop` | Deregisters the session's child job without purge, confirms terminal via a blocking read, tombstones the sidecar binding. Idempotent — a session with no binding is a no-op success. |
| `is-running` | Prints `true`/`false`. False whenever the launched marker is unset, even if the alloc is running (RPP-PROVISION-001: "provisioned, agent never launched" reads as not-running). Once launched, the honesty split (04 §6) applies — Nomad API unavailability never flips this to `false`, it answers last-known-good instead. |
| `list-running` | Prints one running session name per line — launched sessions only, sidecar-primary (04 §2.1). Exits 1 on any lookup error rather than returning a partial list. |

Every other operation exits 2 — the RPP forward-compatibility signal the
caller treats as a no-op success. The remaining driving verbs (`nudge`,
`peek`, `interrupt`, `send-keys`, `clear-scrollback`) and staging are out of
scope for this phase — they land in later phases (see `fnrt-szx`).

## Out of scope

- The remaining driving verbs (`nudge`, `peek`, `interrupt`, `send-keys`,
  `clear-scrollback`) and staging — the parent job's task is a
  structurally-valid tmux-only placeholder (`jobspec.go`), not a real
  agent-bootstrap supervisor binary, and the launch command is a bare
  `tmux new-session`, not a real agent command line.
- Cluster-side recovery for `list-running` (04 §2.1 rule 2: listing the
  parent's children when the sidecar is missing/cold) — `fakenomad`
  implements no children-list endpoint, and this pack's `list-running` is
  sidecar-only.
- The fuller sidecar record (dispatch-attempt counter, disputed-children
  ledger, staleness datum, single-flight lock) — this pack's sidecar
  binding is scoped to exactly what start/stop/is-running/list-running/
  provision/exec/relaunch need.
