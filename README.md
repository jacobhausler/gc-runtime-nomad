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
The remaining driving verbs (`nudge`, `peek`, ...) and staging are out of
scope for this phase and exit 2.

## Layout

```
runtime-nomad/
├── pack.toml       # declares [runtimes.nomad] -> gc-runtime-nomad
├── install.sh      # installs the executable onto PATH
├── conformance.sh  # gc runtime check + gc runtime conformance against an in-memory fake Nomad API
├── fakenomad/      # nested Go module: in-memory fake of the Nomad API this pack calls
│   ├── go.mod      # module github.com/gastownhall/gc-runtime-nomad/fakenomad
│   └── cmd/fakenomad/ # standalone process wrapping the fake server, for conformance.sh + CI
└── runtime/        # nested Go module (zero gascity imports, zero external deps in production code)
    ├── go.mod      # module github.com/gastownhall/gc-runtime-nomad (test-only require on ../fakenomad)
    ├── main.go     # RPP op dispatch + env config
    ├── client.go   # Nomad API client (register/dispatch/deregister/blocking reads/alloc-exec WS)
    ├── exec_ws.go  # client-side RFC 6455 frame codec for the alloc-exec WebSocket
    ├── jobspec.go  # parent job spec builder (04 §4 job-template invariants)
    ├── sidecar.go  # session -> child-job-ID binding store + launched marker (04 §1 sidecar state dir)
    └── ops.go      # start/stop/is-running/list-running/provision/exec/relaunch, stop-path fs egress (NRT-P1-07)
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

## RPP operations

| Op | Notes |
|----|-------|
| `protocol` | `{"version":0,"capabilities":["proc.provision","proc.exec"]}` |
| `provision` | Registers the parent job (idempotent upsert) if needed, then dispatches a tmux-only child for the session — no agent launched, sidecar launched marker stays unset. Rejects a session with a live child with an "already exists" error (04 §6 wire-contract constant). |
| `start` | `provision` + launch: dispatches, then execs the launch command (`tmux new-session -d -s main`) into the alloc, then sets the launched marker. Same "already exists" rejection as `provision`. |
| `exec` | Runs a command inside the session's current alloc over the Nomad alloc-exec WebSocket and streams its stdout; **the op's own exit code is the remote command's exit code** (04 §3 exec row, RPP-CONN-001) — not the 0/1/2 lifecycle-op convention. Works regardless of the launched marker — a provisioned-but-not-launched box already answers exec (RPP-PROVISION-001). |
| `relaunch` | Re-execs the launch command into the SAME alloc — no fresh dispatch — then sets the launched marker (04 §7 warm relaunch, launch-only fingerprint drift). Fails if the session has no live alloc to relaunch into. |
| `stop` | If `GC_NOMAD_EGRESS_DIR` is set, first copies the session's transcript/evidence files (via the Nomad client fs API) into it, retrying up to 3 times before giving up and marking the tombstone `evidence_lost` rather than wedging (R2b-04); a successful egress receipts completion in the sidecar instead. Then deregisters the session's child job without purge, confirms terminal via a blocking read, and tombstones the sidecar binding. Idempotent — a session with no binding is a no-op success, and a stop that fails after egress but before deregister does not re-attempt egress on retry. |
| `is-running` | Prints `true`/`false`. False whenever the launched marker is unset, even if the alloc is running (RPP-PROVISION-001: "provisioned, agent never launched" reads as not-running). Once launched, the honesty split (04 §6) applies — Nomad API unavailability never flips this to `false`, it answers last-known-good instead. |
| `list-running [prefix]` | Prints one running session name per line — launched sessions only. Enumerates the children-of-parent jobs list (`GET /v1/jobs?meta=true`, 04 §2.1 rule 2/3) rather than trusting the sidecar as the existence source, decodes each non-terminal child's `gc_session` Meta key, and — when a prefix argument is given — filters the decoded names to it (`ListRunning(prefix)`, E2a amendment A-1). The sidecar is still consulted for the launched marker, which the children list alone cannot answer. Exits 1 on any lookup error rather than returning a partial list. |

Every other operation exits 2 — the RPP forward-compatibility signal the
caller treats as a no-op success. The remaining driving verbs (`nudge`,
`peek`, `interrupt`, `send-keys`, `clear-scrollback`) and staging are out of
scope for this phase — they land in later phases (see `fnrt-szx`).

## Out of scope

- Retention policy for egressed transcript/evidence files (owner decision)
  and any egress sink beyond a local directory (`NRT-P1-07`) — no
  directory listing/discovery either, just the two well-known files
  (`ops.go`'s `egressFiles`).
- The remaining driving verbs (`nudge`, `peek`, `interrupt`, `send-keys`,
  `clear-scrollback`) and staging — the parent job's task is a
  structurally-valid tmux-only placeholder (`jobspec.go`), not a real
  agent-bootstrap supervisor binary, and the launch command is a bare
  `tmux new-session`, not a real agent command line.
- The fuller sidecar record (dispatch-attempt counter, disputed-children
  ledger, staleness datum) — this pack's sidecar binding is scoped to
  exactly what start/stop/is-running/list-running/provision/exec/relaunch
  need. The single-flight lock concurrent same-name `start`/`provision`
  needs (R2b-03) is a per-session `flock` on a file in the sidecar
  directory rather than a sidecar-record field — it has to survive across
  provider processes, since `gc` execs this binary fresh per op (a race is
  between two OS processes, never two goroutines in one).
