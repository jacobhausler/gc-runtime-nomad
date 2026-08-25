# runtime-nomad

A Gas City **runtime pack**: it ships a runtime *executable*, not a service.
The executable `gc-runtime-nomad` speaks the Runtime Provider Protocol
(RPP v0). It answers the `protocol` handshake and, as of `NRT-P1-03`, the
four session lifecycle ops `start`/`stop`/`is-running`/`list-running` —
implemented over Nomad job dispatch/deregister/blocking reads against a
parameterized parent job, per `research/outputs/04-proposed-architecture.md`
§3/§4/§6. Driving verbs (`exec`, `nudge`, `peek`, ...), staging, and the
provision/launch split are out of scope for this phase and exit 2.

## Layout

```
runtime-nomad/
├── pack.toml         # declares [runtimes.nomad] -> gc-runtime-nomad
├── install.sh        # installs the executable onto PATH
├── conformance.sh    # gc runtime check + gc runtime conformance against an in-memory fake Nomad API
├── fakenomad/        # nested Go module: in-memory fake of the Nomad API this pack calls
│   ├── go.mod        # module github.com/gastownhall/gc-runtime-nomad/fakenomad
│   └── cmd/fakenomad/ # standalone process wrapping the fake server, for conformance.sh + CI
└── runtime/          # nested Go module (zero gascity imports, zero external deps in production code)
    ├── go.mod        # module github.com/gastownhall/gc-runtime-nomad (test-only require on ../fakenomad)
    ├── main.go       # RPP op dispatch + env config
    ├── client.go     # Nomad API client (register/dispatch/deregister/blocking reads)
    ├── jobspec.go    # parent job spec builder (04 §4 job-template invariants)
    ├── sidecar.go    # session -> child-job-ID binding store (04 §1 sidecar state dir)
    └── ops.go        # start/stop/is-running/list-running
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
| `protocol` | `{"version":0,"capabilities":[]}` |
| `start` | Registers the parent job (idempotent upsert) if needed, then dispatches a child for the session. Rejects a session with a live child with an "already exists" error (04 §6 wire-contract constant). |
| `stop` | Deregisters the session's child job without purge, confirms terminal via a blocking read, tombstones the sidecar binding. Idempotent — a session with no binding is a no-op success. |
| `is-running` | Prints `true`/`false`. Per the honesty split (04 §6), Nomad API unavailability never flips this to `false` — it answers last-known-good instead. |
| `list-running` | Prints one running session name per line, sidecar-primary (04 §2.1). Exits 1 on any lookup error rather than returning a partial list. |

Every other operation exits 2 — the RPP forward-compatibility signal the
caller treats as a no-op success. Driving verbs, staging, and the
provision/launch split are out of scope for this phase (`NRT-P1-03`
out-of-scope note) — they land in later phases (see `fnrt-szx`, `fnrt-8yh`).

## Out of scope

- Driving verbs (`exec`, `nudge`, `peek`, `interrupt`, `send-keys`,
  `clear-scrollback`), staging, and the provision/launch split — the parent
  job's task is a structurally-valid placeholder (`jobspec.go`), not a real
  agent bootstrap.
- Cluster-side recovery for `list-running` (04 §2.1 rule 2: listing the
  parent's children when the sidecar is missing/cold) — `fakenomad`
  implements no children-list endpoint, and this pack's `list-running` is
  sidecar-only.
- The fuller sidecar record (launched marker, dispatch-attempt counter,
  disputed-children ledger, staleness datum, single-flight lock) — this
  phase's sidecar binding is scoped to exactly what start/stop/is-running/
  list-running need.
