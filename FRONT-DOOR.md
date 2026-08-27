---
kind: front-door
version: 1.0
ratified-by: pending
ratified-date: pending
ledger-bead-id: to-be-assigned
owner-unit: runtime-nomad
---

# Front door: runtime-nomad

## Identity and scope

- `runtime-nomad` is a Gas City runtime pack: it ships the `gc-runtime-nomad` executable (not a service) and speaks Runtime Provider Protocol v0 (RPP v0) against a Nomad cluster.
- Registry name: `jacobhausler/runtime-nomad` (the `runtime-nomad/` pack in the `jacobhausler/gascity-packs` fork of `gastownhall/gascity-packs`).
- Co-maintainers: Jacob Hausler (fork owner) and Fable (Westlands executive).
- Scope: one session = one child job under a single parameterized parent job; lifecycle `provision`/launch/`exec`/`stop` plus `is-running`/`list-running`, all over the Nomad API.

## Interfaces

- Ops: `protocol` (RPP v0 handshake), `start`, `stop`, `is-running`, `list-running [prefix]`, `provision`, `relaunch`, `exec`, `nudge`, `peek`, `interrupt`, `send-keys`, `clear-scrollback`, `check`. Exit codes: 0 = success, 1 = error, 2 = unknown op — the RPP forward-compat signal callers treat as no-op success; `exec` is the exception (it exits with the remote command's own code).
- Config keys (environment of the runtime process):

| Key | Required | Meaning |
|----|----|----|
| `GC_NOMAD_ADDR` | yes | Nomad API base URL, e.g. `http://127.0.0.1:4646` |
| `GC_NOMAD_SIDECAR_DIR` | yes | sidecar dir: session→child-job-ID bindings, launched markers |
| `GC_NOMAD_TOKEN` | no | ACL token, sent as `X-Nomad-Token` |
| `GC_NOMAD_NAMESPACE` | no | Nomad namespace (default `default`) |
| `GC_NOMAD_NODE_POOL` | no | parent job's node pool (empty = Nomad's default pool) |
| `GC_NOMAD_PARENT_JOB` | no | parent job ID (default `gc-sessions`) |
| `GC_NOMAD_EGRESS_DIR` | no | stop-path transcript/evidence dir; unset disables egress |
| `GC_NOMAD_FORBID_REGISTER` | no | `true` = never attempt a parent registration; fail closed |
| `GC_NOMAD_LOG_SINK` | no | HTTP JSON-lines sink; the one key that adds the `log-shipper` task |
| `GC_NOMAD_LOG_SINK_TOKEN_FILE` | no | in-box path the shipper reads its bearer token from |
| `GC_NOMAD_LOG_LABELS` | no | `k=v,k=v` labels merged onto the fixed `session_name`/`alloc_id`/`node`/`runtime=nomad` set |

- Token model: the management token (holds `submit-job`) registers the parent job exactly once; every session-path op (dispatch, launch, peek, stop, is-running, list-running) runs on the narrowed runtime token and never registers — a matching parent is a read-only no-op, a missing or drifted one fails the op with a re-registration error.

## Health and SLO signals

- `is-running` prints `true`/`false` with an honesty split: provisioned-but-never-launched reads `false` even while the alloc runs; once launched, an in-box probe (tmux session + pane pid) answers, so an agent killed inside a live alloc reads `false`; transport faults along the path answer last-known-good `true`, never a fabricated `false`.
- `list-running` exits 0 with one launched session name per line (prefix-filterable); exits 1 on any lookup error — never a partial list.
- `check` exits 0 with no cluster configuration; with the sink unset it prints `warning: session logs will not be shipped (GC_NOMAD_LOG_SINK unset)` to stderr.
- Shipper metrics: with the sink set, each session group carries a pinned vector `log-shipper` task exposing vector's own Prometheus metrics via its built-in `prometheus_exporter` on a group-local port.

## How to integrate

1. `city.toml`:

   ```toml
   [imports.runtime-nomad]
   source = "jacobhausler/runtime-nomad"

   [session]
   provider = "nomad"
   ```

2. `./install.sh` (puts `gc-runtime-nomad` on PATH), then `gc doctor`.
3. On the runtime host export `GC_NOMAD_ADDR`, `GC_NOMAD_SIDECAR_DIR`, and the runtime token in `GC_NOMAD_TOKEN`, sourced from its custody file path — never typed in or pasted.
4. Register the parent once with the management token: `GC_NOMAD_TOKEN=<management token> gc-runtime-nomad provision <session-name>` (or the first `start <session-name>` — the session name is the first positional argument of every named op); it registers `gc-sessions`, stamped with the `gc_jobspec_hash` fingerprint.
5. To ship session logs set `GC_NOMAD_LOG_SINK` (plus `GC_NOMAD_LOG_SINK_TOKEN_FILE` / `GC_NOMAD_LOG_LABELS`); confirm `gc-runtime-nomad check` prints no warning.
6. Start sessions; `stop` deregisters the child and copies transcript/evidence to `GC_NOMAD_EGRESS_DIR` when it is set.

Token custody is by path: token values live in custody files (mode 0600, owner root) at fixed host paths; the city config and every document reference the path, never the value — the shipper's bearer token likewise reads from the file named by `GC_NOMAD_LOG_SINK_TOKEN_FILE`.

## How to upgrade safely

- Any jobspec change — task spec or node pool, i.e. a different `gc_jobspec_hash` — means re-registering the parent with the management token before dispatching; until then the pack refuses to dispatch with `parent job "gc-sessions" is stale — re-register with a management token`.
- The conformance ladder is hermetic (no cluster, no network) and is the pack's CI gate: `GC_BIN=$(command -v gc) ./conformance.sh` runs `gc runtime check` and `gc runtime conformance` through the full RPP lifecycle against an in-memory fake Nomad; `./receipt.sh` runs the full ladder (check, golden suite, staging env probe, L0–L2 `go vet`/`go test`, secrets-grep) and records each item's pass/fail result, linked to the commit it ran at.
- For a Nomad cluster upgrade (N-1 to N), rehearse the transition on a fresh cluster before applying it to the running one.

## Invariants / what breaks it

- Never re-register the parent with the runtime token — it lacks `submit-job`, so it cannot fix drift in place; only the management token can.
- Never cat a secret-bearing config — custody files (token values) and env ledgers are read by the machinery, never printed into documents, transcripts, or logs; reference the path, never the value.
- A snapshot revert on a cloud-init image wipes the SSH host keys (first boot regenerates the keypair) — re-baseline host keys after any revert before trusting SSH to that node.
- Gossip key uniqueness is per cluster: a cluster's gossip encryption key is never copied into another cluster.

## Ownership

- Repo: `github.com/jacobhausler/gascity-packs` (fork of `gastownhall/gascity-packs`); the pack lives at `runtime-nomad/`.
- Issue tracker: that repository's issues.
- The `nomad-runtime-debug` skill, once it exists, is the entry point for debugging the pack and the cluster.
