# runtime-nomad

> Extracted from jacobhausler/gascity-packs fork with history on 2026-08-26;
> registry name jacobhausler/runtime-nomad; co-maintainers @jacobhausler
> @atbrace.

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
into the alloc over tar-over-exec-stdin. As of `fnrt-t4l.13`, setting
`GC_NOMAD_LOG_SINK` adds a second `log-shipper` task (a pinned+checksummed
`vector` binary) to the session task group, tailing the agent's session
JSONL and captured stdout to that HTTP JSON-lines sink and exposing
vector's own metrics via `prometheus_exporter` on a group-local port —
unset, this task is never added at all.

## Layout

```
runtime-nomad/
├── pack.toml       # declares [runtimes.nomad] -> gc-runtime-nomad
├── install.sh      # installs the executable onto PATH
├── conformance.sh  # gc runtime check + gc runtime conformance against an in-memory fake Nomad API
├── toolchain/      # checksum-pinned session-toolchain build input (TOOLCHAIN.lock + Dockerfile + build-image.sh + BUILD-RECEIPT.md) — see §Part 2
├── fakenomad/      # nested Go module: in-memory fake of the Nomad API this pack calls
│   ├── go.mod      # module github.com/gastownhall/gc-runtime-nomad/fakenomad
│   └── cmd/fakenomad/ # standalone process wrapping the fake server, for conformance.sh + CI
├── runtime/        # nested Go module (zero gascity imports, zero external deps in production code)
│   ├── go.mod      # module github.com/gastownhall/gc-runtime-nomad (test-only require on ../fakenomad)
│   ├── main.go     # RPP op dispatch + env config
│   ├── client.go   # Nomad API client (register/dispatch/deregister/blocking reads/alloc-exec WS)
│   ├── exec_ws.go  # client-side RFC 6455 frame codec for the alloc-exec WebSocket
│   ├── jobspec.go  # parent job spec builder (04 §4 job-template invariants) + optional log-shipper task (fnrt-t4l.13)
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
gc-runtime-nomad check  # warn when session-log shipping or artifact delivery is not configured
```

Lifecycle ops need Nomad API configuration on the environment. There is **one
key table in this pack and it lives in Install -> part 1 -> step 7** - the copy
kept in sync with `runtime/*.go`. This section deliberately does not repeat it:
every hand-copied subset has drifted (the last was missing 7 of its 20 keys,
`GC_NOMAD_AGENT_IMAGE` among them). Two keys are worth naming here because they
fail silently: with `GC_NOMAD_DNS_SERVERS` unset the alloc inherits the *client
host's* resolv.conf, and `GC_NOMAD_NODE_CONSTRAINT` only ever appends to the
pack's linux-kernel fatal rule.

### Allocation environment contract

Each session allocation receives its runtime environment in three explicit
layers:

| Layer | Delivery contract |
|---|---|
| Workspace/secrets | `stageConfig.Env` is staged through the pack's tar-over-exec-stdin channel into `$NOMAD_SECRETS_DIR`, including the sourceable `env.sh` file. This is the pack's own transport, not an RPP claim: the pack declares no `env.workspace` capability, because the start wire does not materialize `work_dir` inside the alloc and gc probes every declared `env.*` code. |
| Toolchain | With `GC_NOMAD_AGENT_IMAGE`, the configured Docker image carries the session toolchain and the agent task has no host-volume mount; without it, `gc`, Codex, tmux, `sh`, and `tar` are expected in the allocation toolchain and `GC_NOMAD_ARTIFACT_VOLUME` may mount the pinned Linux `gc` artifact read-only at `/mnt/nomad`. |
| Identity/config | The city target arrives through the secrets environment channel as **exactly one** of a complete remote context (`GC_HOME` + `GC_CITY_CONTEXT`) or `GC_CITY_URL`; both at once is the fail-closed case, not a redundancy - see Install part 1 step 8. Session identity values are added by the lifecycle. |

The Cloudflare pack's bundle is the environment boundary for pack-provided
tools; the exec driver otherwise retains its host-binary assumptions. The
runtime sources `$NOMAD_SECRETS_DIR/env.sh` before starting tmux, while local
mode remains valid when no environment script is staged.

Enable live session-log continuity with the recommended one-line setup:

```bash
export GC_NOMAD_LOG_SINK="https://<firehose>/ingest"
```

Add `GC_NOMAD_LOG_SINK_TOKEN_FILE` when the firehose requires bearer auth and
`GC_NOMAD_LOG_LABELS` for deployment-specific labels. If the sink is unset,
the pack prints a warning during its check and session logs will not be
shipped; disabling shipping is therefore explicit and visible.

## Install

D4 has ruled: this pack is now the estate default for every lane except the
executive. Everything below is provider-agnostic; the values one estate actually
chose are not this pack's to carry and live in its admitted deployment record
(end of part 1).

The runtime and the agent **provider** are separate abstractions: this pack
schedules a box and feeds it; it does not know, name, or ship what runs inside
it. Part 1 is the runtime and part 2 the contract its provider side must
satisfy; what a given estate fills in is recorded outside this pack.

### Part 1 — Runtime requirements

Provider-agnostic: what any deployment of this pack needs.

1. **`gc`, installed at a pin that carries the remote-worker families.**
   `go install github.com/gastownhall/gascity/cmd/gc@<pin>`. `gc` is a tool
   dependency of this pack, never a Go import — the pack keeps zero gascity
   Go dependencies. The families the pin must carry, their upstream twins, and
   the first pin that carried each are the checklist in
   [`docs/PIN-DEPENDENCIES.md`](docs/PIN-DEPENDENCIES.md). Pin choice is the
   gate that silently decides whether a box can do work: the identities the
   allocation sends must be a superset of the identities the claim gate
   matches, or every session on a lane whose work arrives as a step bead
   assigned to the pool template answers `drain/no_work` and looks healthy
   while doing nothing. A fix for such a gate that has shipped on a branch
   **is not in effect until the pin moves onto a commit that is an ancestor
   of it** — verify with `git merge-base --is-ancestor <fix-sha> <pin>`, and
   treat the result as a pin-roll action, not a defect to re-investigate.
2. **A Go toolchain where `go install` may run.** On an estate that forbids
   Mac-side Go runs, build off-Mac and install the artifact from there;
   `./install.sh agent-launch` remains runnable from the operator seat because
   that half is pure ssh+scp.
3. **A reachable Nomad API, plus two tokens:** a management token holding
   `submit-job` for the one-time parent-job registration (step 6), and the
   narrowed runtime token every session-path op runs on.
4. **Install the executable.** `./install.sh` runs `go install .` in
   `runtime/` and confirms `gc-runtime-nomad` landed on `PATH` (adding
   `$(go env GOBIN)` if it did not). This file *is* the pack's install step —
   the `pack-runtimes` `gc doctor` check then verifies the binary is installed
   and answers the RPP handshake (RUNTIME-SEL-011).
5. **Deploy the agent launcher.** `install.sh` also deploys the
   deployment-owned bootstrap the launched tmux pane execs — but only when
   `GC_NOMAD_AGENT_LAUNCH_SOURCE` is set (unset ⇒ the deploy prints a skip line
   and does nothing else, which is also the rollback path). Host and path come
   from `GC_NOMAD_AGENT_LAUNCH_HOST` / `GC_NOMAD_AGENT_LAUNCH_TARGET`; both are
   deployment-supplied pointers whose defaults `install.sh` fills in for one
   deployment - adopt them only after reading that deployment's record.
   `./install.sh agent-launch` runs this half alone. The deploy verifies by
   sha256 (a matching live copy is left alone), takes a dated rollback copy
   before overwriting, preserves the live file's owner/group/mode, and fails
   closed on any mismatch. Point
   `GC_NOMAD_AGENT_LAUNCH_SCRIPT` at the deployed path in the provider config.
   **A launcher edit does not take effect via `go install`** — the live copy on
   the shared volume is what the launch path execs.
6. **Register the parent job once, with the management token.** Run
   `provision` or `start` once with `GC_NOMAD_TOKEN` set to that token; the
   registration is idempotent and stamps the jobspec fingerprint. After **any**
   jobspec change — task spec, node pool, DNS, constraints — re-register it the
   same way, or the pack reports the parent stale and refuses to dispatch.
   `GC_NOMAD_FORBID_REGISTER=true` makes an absent or drifted parent a
   fail-closed error instead of an attempted registration.
7. **Configure the runtime's environment.** These keys are read by the
   `gc-runtime-nomad` process, i.e. the **supervisor's** environment — none of
   them belong in the allocation. Landing a change here means changing the
   environment the **supervisor process itself** starts with - however the
   deployment composes it (a service manager's env, or a wrapper that sources a
   file) - and then cycling the supervisor. `gc reload` cannot revoke an env the
   supervisor already composed, and a wrapper sourcing a file wholesale can keep
   supplying a key the service definition no longer names.

   | Key | Required | Notes |
   |---|---|---|
   | `GC_NOMAD_ADDR` | yes | Absolute Nomad API base URL |
   | `GC_NOMAD_SIDECAR_DIR` | yes | Session→child-job-ID bindings + launched markers |
   | `GC_NOMAD_TOKEN` | no | ACL token (`X-Nomad-Token`); the narrowed runtime token in normal use |
   | `GC_NOMAD_NAMESPACE` | no | Nomad namespace (default `default`) |
   | `GC_NOMAD_PARENT_JOB` | no | Parameterized parent job ID (default `gc-sessions`) |
   | `GC_NOMAD_NODE_POOL` | no | Parent job's node pool (empty = Nomad's own default pool) |
   | `GC_NOMAD_NODE_CONSTRAINT` | no | `;`-separated Nomad-syntax constraints appended *after* the pack's linux-kernel fatal rule, e.g. `${node.unique.name} != prod-nomad` |
   | `GC_NOMAD_DNS_SERVERS` | no | Resolver addresses the session group's bridge network writes into the alloc's `/etc/resolv.conf` — see step 10 |
   | `GC_NOMAD_ARTIFACT_VOLUME` | no | Client host-volume name mounted read-only at `/mnt/nomad` for the pinned Linux `gc` artifact |
   | `GC_NOMAD_AGENT_IMAGE` | no | Docker image carrying the session agent and its toolchain, used instead of the legacy `exec` task (see the image-content note below); unset preserves the legacy `exec` task and the artifact-volume rollback shape |
   | `GC_NOMAD_AGENT_LAUNCH_SCRIPT` | no | The bootstrap from step 5; unset preserves the bare-tmux placeholder pane |
   | `GC_NOMAD_EXEC_TASK` | no | Task name alloc-exec targets, when it is not the pack's session task |
   | `GC_NOMAD_EXTRA_SECRET_KEYS` | no | Comma-separated names to bridge from the supervisor's own process env into `$NOMAD_SECRETS_DIR` — the value-blind channel for a secret `gc`'s provider env map cannot interpolate. Naming a key is what stages it; an unnamed key is never staged, and an extra key is always routed to the secrets dir, never to the launch argv. The names themselves belong to the deployment (part 2 works one Codex name through it) |
   | `GC_NOMAD_EGRESS_DIR` | no | Stop-path transcript/evidence directory; unset disables egress |
   | `GC_NOMAD_FORBID_REGISTER` | no | `true` = never register the parent; fail closed instead. Unset = the default (registration allowed); a value that is not a boolean is a config error and the op stops naming this variable rather than defaulting to allowed |
   | `GC_NOMAD_ADOPT_MAX_AGE` | no | Go duration capping how old a live-but-unlaunched box may be before it is reaped and replaced instead of adopted (default `30m`; unparseable is a config error) |
   | `GC_NOMAD_LOG_SINK` | no | The one key that adds the `log-shipper` task; unset ⇒ session logs are not shipped |
   | `GC_NOMAD_LOG_SINK_TOKEN_FILE` | no | In-box path the shipper reads its bearer token from |
   | `GC_NOMAD_LOG_LABELS` | no | `k=v,k=v` labels merged onto the fixed `session_name`/`alloc_id`/`node`/`runtime=nomad` set |
   | `GC_NOMAD_LOG_SHIPPER_ARTIFACT` | no | URL or local path for the pinned Vector archive; unset uses the upstream release URL |

   Image *content* is not this pack's contract. `gc build-image` can produce such
   an image, but at the pinned gc its own contract is K8s-shaped
   (`cmd/gc/cmd_build_image.go:34-41` - `[session.k8s]`, Pods, K8s Secret) and
   names no Nomad at all; the prebake and release work (`cr-u7b0s.45`,
   `cr-u7b0s.50`) owns what goes into the image this key points at.

   The identity/config side of the allocation is *not* configured here:
   `GC_CITY_URL`, `GC_CITY_NAME`, and whatever transport credentials the
   endpoint needs arrive through the provider env / secrets channel, and session
   identity (`GC_SESSION_ID`/`GC_AGENT`/`GC_SESSION_NAME`) is added by the
   lifecycle.
8. **Give the allocations a city endpoint they can actually reach — named
   exactly once.** An allocation cannot reach the supervisor's loopback
   listeners, so a remote session needs either a complete **remote context**
   (`GC_HOME` + `GC_CITY_CONTEXT`, staged by the pack as `gc-home/contexts.toml`
   — an mTLS context on this estate) or `GC_CITY_URL`. Both at once is a
   failure, not a redundancy: when the provider env carries a complete remote
   context, the pack stages `gc-home/contexts.toml`, exports `GC_HOME` +
   `GC_CITY_CONTEXT`, and then **drops `GC_CITY_URL` from `env.sh`** — a target
   named twice makes `gc` fail closed. It also strips the local-target family
   (`GC_CITY`, `GC_CITY_PATH`, `GC_CITY_ROOT`, `GC_CITY_RUNTIME_DIR`,
   `GC_BEADS*`) before the launch argv filter runs. Do not work around either
   by re-adding them to the provider env.
9. **Stage what must be secret at runtime, value-blind.**
   `GC_NOMAD_EXTRA_SECRET_KEYS` is generic: name a key and the pack bridges that
   *name* from the supervisor's own process env into `$NOMAD_SECRETS_DIR`
   without the value ever entering a jobspec, an argv, or this repository. The
   credential NAME is the deployer's choice — the runtime has no credential
   vocabulary and none of its own keys to reserve; the box installs what it
   finds there.
10. **Resolvers and placement are configuration, not cosmetics.**
    `GC_NOMAD_DNS_SERVERS` unset means Nomad declares no `dns` stanza, the alloc
    inherits the **client host's** `resolv.conf`, and on a systemd-resolved host
    that is the `127.0.0.53` stub — unreachable from the alloc netns, so nothing
    inside the box resolves a name and every failure downstream looks like an
    agent or network defect. Give it real resolver addresses (a non-address
    value is a config error, not a warning). Placement has the same trap in a
    different shape: the node pool must actually span the boxes you expect work
    to land on, and the pack appends `GC_NOMAD_NODE_POOL` /
    `GC_NOMAD_NODE_CONSTRAINT` to the parent job — so either is a jobspec change
    and needs step 6's re-registration to take effect.
11. **Check it without a cluster.** `gc-runtime-nomad check` warns when log
   shipping or the artifact volume is unconfigured.
   `GC_BIN=$(command -v gc) ./conformance.sh` runs `gc runtime check` and the
   full RPP lifecycle round-trip against the in-memory fake Nomad API — no
   cluster, no network.

### Part 2 — Provider toolchain in the box

The rule, in the abstract: **the box must provide the seat's provider command on
`PATH` exactly as `gc` renders it.** The command line the provider config
produces for a session has to resolve and run inside the allocation, unchanged,
with whatever siblings that command assumes present alongside it. Nothing
provider-specific lives in the runtime: `gc-runtime-nomad` never names,
downloads, installs, or inspects a provider — it launches a pane and execs the
launcher from part 1 step 5, which execs what it is told. If a provider needs a
file, a binary, or a credential, that is the deployment staging it (the toolchain
volume, the launcher, the value-blind secret channel). A seat that comes up but
cannot run a tool is a toolchain-staging defect wearing a runtime costume.

**Worked example — Codex.** Stage the agent toolchain as a **full standalone
package**: `codex-package-<target>.tar.gz` — **not** the bare
`codex-<target>.tar.gz`. The standalone package is what carries
`bin/codex-code-mode-host`, `codex-resources/` (bwrap, zsh), `codex-path/` (rg)
and `codex-package.json`; staged as a single file, the entrypoint runs and
reports its execution host as unavailable, which degrades the agent to a chat
window that cannot run a tool and reads like a runtime failure. Pick the target
triple from the box (`file`/`ldd` on the binary you are replacing), confirm the
sha against the release's own `codex-package_SHA256SUMS`, keep a dated rollback
copy, and verify by running the launcher's own `PATH` line:
`env -i PATH=<staged bin dir>:/usr/bin:/bin sh -c 'command -v codex;
command -v codex-code-mode-host'`. A toolchain staging receipt must name the
artifact, its sha256, and the layout it asserts — a missing sha is the symptom
that the layout was never checked. Its credential rides the generic channel from
part 1 step 9 under whatever name the deployment chose (here `CODEX_AUTH_JSON`),
staged value-blind into `$NOMAD_SECRETS_DIR`, and the launcher from step 5
installs it to `$CODEX_HOME/auth.json`.

**The pinned build input is `toolchain/TOOLCHAIN.lock`, and it is the pointer
this section's own rule demands.** The rule above is that a toolchain staging
receipt names the artifact, its sha256, and the layout it asserts; that lock is
exactly that, per component (gc, bd, git, tmux, the Codex standalone package)
and per image (base, builder, agent, rollback) — digest-pinned references, never
floating tags. `toolchain/build-image.sh` reads every value in it and passes
each as a build ARG, `toolchain/Dockerfile` consumes the ARGs, and
`toolchain/BUILD-RECEIPT.md` carries the measurements — including the pins that
were rejected and why. It ships no delivery layer: publishing stays an executive
host step, and the live pin it must be read alongside is the estate record, not
this pack's. (cr-u7b0s.45 landed the lock; cr-5xrcu added this pointer on
2026-09-09, having found `rg 'toolchain/' README.md` empty in a tree that
already carried it.)

Prebaking the toolchain into the box image is the intended 1.0.x shape —
`gc build-image` is the prebake verb gc ships - its contract at the pin is
K8s-shaped, see the `GC_NOMAD_AGENT_IMAGE` row in step 7 - and epic `cr-u7b0s` is
that work; manual staging as above is what runs today.

### Estate deployment values live outside this pack

The keys above are a contract, not a set of values. One deployment's filled-in
record - endpoint and mTLS custody, host-firewall allowlist, resolver address,
placement state (enforced vs emergent), host-volume and launcher pointers, the
off-Mac Go rule - is not this pack's to carry. Westlands' lives in the admitted
cleanroom card
`_cleanroom/service_groups/agent_fabric/gc_control_plane_relay/gc_control_plane_relay.md`
(`gc_control_plane_relay`, linked from `CLEANROOM-MAP.yaml`). Read it before
debugging an alloc egress timeout: a host absent from that estate's firewall
table fails exactly like a broken runtime would. Nothing in that record is a
default to copy.

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
| `check` | Pack-local diagnostic. Emits warnings when the optional log sink or artifact volume is unset; does not require Nomad configuration. |
| `protocol` | `{"version":0,"capabilities":["proc.provision","proc.exec"]}` |
| `provision` | Registers the parent job (idempotent upsert) if needed, then dispatches a tmux-only child for the session — no agent launched, sidecar launched marker stays unset. Idempotent over the box it already made: a session whose bound child is still live but never launched is ADOPTED (no second child dispatched, staging re-run), so a controller re-driving `provision` after an attempt whose staging or launch never confirmed can finish the job instead of collecting "already exists" forever (fnrt-t4l.46). A session whose child is live AND launched is a genuine duplicate and is still rejected with an "already exists" error (04 §6 wire-contract constant). Reads an optional staging config (JSON) on stdin and materializes it (see `start`'s staging row) before returning. |
| `start` | `provision` + launch: dispatches, stages an optional workspace/secrets config read as JSON from stdin (`staging.go`'s `stageConfig`, NRT-P1-06 — workspace files land under `WorkDir` and env entries land as individual files plus a sourceable `env.sh` under `$NOMAD_SECRETS_DIR`, both via tar-over-exec-stdin, 04 §5), then sources that script and execs the launch command (`tmux new-session -d -s main`, plus `-e KEY=VALUE` for any `envArgvSafe`-classified env entries) into the alloc, then sets the launched marker. Empty/absent stdin is a no-op config — pre-staging callers are unaffected. A session left live-but-unlaunched by an earlier attempt (a `provision`, or a `start` whose launch never confirmed) is resumed into rather than re-dispatched; only a live, already-launched session is rejected, with the same "already exists" error as `provision`. |
| `exec` | Runs a command inside the session's current alloc over the Nomad alloc-exec WebSocket and streams its stdout; **the op's own exit code is the remote command's exit code** (04 §3 exec row, RPP-CONN-001) — not the 0/1/2 lifecycle-op convention. Works regardless of the launched marker — a provisioned-but-not-launched box already answers exec (RPP-PROVISION-001). |
| `relaunch` | Re-execs the launch command into the SAME alloc — no fresh dispatch, no fresh env — then sets the launched marker (04 §7 warm relaunch, launch-only fingerprint drift). Fails if the session has no live alloc to relaunch into. |
| `nudge` / `peek` / `interrupt` / `send-keys` / `clear-scrollback` | The driving verbs (NRT-P1-05), realized as tmux commands sent into the session's tmux pane over the same exec mechanism. `nudge`/`interrupt` are best-effort — a not-found session answers success rather than an error, per the RPP's best-effort convention. |
| `stop` | Best-effort `tmux kill-session` for the agent's session before anything else (NRT-P2-06.1) — the task's own exit is independent of this (its supervisor script traps and exits on Nomad's own SIGTERM), but tmux's server is a detached daemon nothing else reaps. When log shipping is enabled, requests and waits for the `log-shipper` graceful flush. If `GC_NOMAD_EGRESS_DIR` is set, then copies the session's transcript/evidence files (via the Nomad client fs API) into it, retrying up to 3 times before giving up and marking the tombstone `evidence_lost` rather than wedging (R2b-04); a flush failure also records `evidence_lost`, while a successful egress receipts completion in the sidecar instead. The order is agent stop → shipper flush → egress → deregister. Then deregisters the session's child job without purge, confirms terminal via a blocking read, and tombstones the sidecar binding. Idempotent — a session with no binding is a no-op success, and a stop that fails after egress but before deregister does not re-attempt egress on retry. |
| `is-running` | Prints `true`/`false`. False whenever the launched marker is unset, even if the alloc is running (RPP-PROVISION-001: "provisioned, agent never launched" reads as not-running). Once launched, it also execs an in-box liveness probe into the alloc (`tmux has-session -t main`, plus `kill -0 <pid>` on the pane pid `start`/`relaunch` captured) — a box whose alloc stays non-terminal but whose agent died inside it (08 §3 in-box agent kill row) answers `false`, not `true`; ClientStatus alone can never see that gap. The honesty split (04 §6) still governs every transport fault along the way — a lookup error listing allocs, or a transport error execing the probe itself — which answers last-known-good (`true`) rather than `false`; only a definitive, non-transport probe result (the tmux session/pid genuinely gone) reads as agent-dead. |
| `list-running [prefix]` | Prints one running session name per line — launched sessions only. Enumerates the children-of-parent jobs list (`GET /v1/jobs?meta=true`, 04 §2.1 rule 2/3) rather than trusting the sidecar as the existence source, decodes each non-terminal child's `gc_session` Meta key, and — when a prefix argument is given — filters the decoded names to it (`ListRunning(prefix)`, E2a amendment A-1). The sidecar is still consulted for the launched marker, which the children list alone cannot answer. Exits 1 on any lookup error rather than returning a partial list. |

Every other operation besides the pack-local `check` diagnostic exits 2 — the
RPP forward-compatibility signal the caller treats as a no-op success.

### Roles

The lab ACL model splits acting into two roles:

- **Operator** — cluster-side actions: node drain, client restarts,
  partitions, snapshot/restore, upgrades, and force-leave. These use the
  management token (or root) and are never part of the pack's session path.
- **Pack session path** — dispatch, launch, peek, stop, is-running, and
  list-running run only on the narrowed runtime token and never need
  register-job: a matching parent job makes the registration check a
  read-only no-op, and a missing or drifted one fails the op with a
  re-registration error.

### Parent job lifecycle

`gc-sessions` is registered once, via the pack, with the management token
(run `provision` or `start` once with `GC_NOMAD_TOKEN` set to the management
token), and stamped with the `gc_jobspec_hash` fingerprint of its jobspec.
After **any** jobspec change — task spec or node pool — re-register it the
same way. Until then the pack reports `parent job "gc-sessions" is stale —
re-register with a management token` and refuses to dispatch; the runtime
token cannot fix the drift in place.

## Out of scope

- Retention policy for egressed transcript/evidence files (owner decision,
  `NRT-P1-07`) — no directory listing/discovery either, just the two
  well-known files (`ops.go`'s `egressFiles`). (The older version of this
  bullet also scoped out any egress sink beyond a local directory; that is
  overruled as of `fnrt-t4l.13`'s `GC_NOMAD_LOG_SINK` HTTP sink — see
  above. `NRT-P1-07`'s own stop-path file egress is still local-directory
  only; it is a distinct mechanism from the log-shipper's live streaming.)
- Remote artifact blocks and the env.ledger tunnel (`NRT-P1-06`) — staging
  only materializes what a caller sends inline on stdin; the launch command
  starts a bare `tmux new-session` (plus `-e` flags) after sourcing the
  staged environment. As of the `GC_NOMAD_AGENT_LAUNCH_SCRIPT` seam
  (`ops.go`'s `buildLaunchCommand`), a deployment can configure that tmux
  pane's initial command to be a real agent bootstrap script (PATH prepend,
  an agent HOME pointed inside `$NOMAD_SECRETS_DIR`, staged-auth install,
  then exec the agent) instead of the bare placeholder shell — the pack
  itself stays agent-agnostic; unset, the placeholder session is unchanged.
  The session task's own command (`jobspec.go`'s
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
