# runtime-nomad

> Extracted from jacobhausler/gascity-packs fork with history on 2026-08-26;
> registry name jacobhausler/runtime-nomad; co-maintainers @jacobhausler
> @atbrace.

A Gas City **runtime pack**: it ships a runtime *executable*, not a service.
The executable `gc-runtime-nomad` speaks the Runtime Provider Protocol
(RPP v0). It answers the `protocol` handshake, the four session lifecycle
ops `start`/`stop`/`is-running`/`list-running`, plus the provision/launch split
and warm relaunch: `provision`/`exec`/`relaunch`. Everything is implemented over
Nomad job dispatch/deregister/blocking reads against a parameterized parent job
plus the alloc-exec WebSocket. `stop` also egresses each session's
transcript/evidence files via the Nomad client fs API before deregister, when
egress is configured. `exec` plus the driving verbs (`nudge`, `peek`,
`interrupt`, `send-keys`, `clear-scrollback`) are realized as tmux commands
over the same exec mechanism. `start`/`provision` accept an optional
workspace/secrets staging config on stdin, materialized into the alloc over
tar-over-exec-stdin. Setting `GC_NOMAD_LOG_SINK` adds a second `log-shipper`
task (a pinned+checksummed
`vector` binary) to the session task group, tailing the agent's session
JSONL and captured stdout to that HTTP JSON-lines sink and exposing
vector's own metrics via `prometheus_exporter` on a group-local port —
unset, this task is never added at all.

## Before you install

> **Status: installable from a published fork build. Not yet installable from
> an upstream `gc`.** Read this before anything else.
>
> This pack requires a `gc` carrying behaviour families that are not all
> upstream yet, so **`go install github.com/gastownhall/gascity/cmd/gc@<pin>`
> cannot resolve the pin for you** — and it never will, whatever the pin: the
> module path is `github.com/gastownhall/gascity`, so a fork URL does not
> resolve either. `go install` is the wrong door and this README used to send
> you to it.
>
> **Install the published binary instead** (Part 1, step 1). Builds of the pin
> this pack is tested against, for linux/amd64 and darwin/arm64, with sha256
> sidecars, are published at
> <https://github.com/jacobhausler/gascity/releases>.
>
> Be clear-eyed about what that is: **a fork build, not an upstream release.**
> If you will not run a fork, this pack is not ready for you, and the honest
> answer is to wait for the upstream PRs below. If you will, the binary is
> real, reproducible from the fork's own source at that commit, and the pack
> fails closed against a `gc` that lacks the families rather than running
> half-broken.
>
> The blocking set is smaller than it used to look: **six families — two
> with filed upstream PRs, three more now pushed onto one of them, and one
> that cannot stack on either.** The other thirteen the pin carries are not
> requirements of this pack. [`docs/PIN-DEPENDENCIES.md`](docs/PIN-DEPENDENCIES.md)
> tiers them and shows the work.
>
> **What is NOT blocking, contrary to earlier versions of this page.** Three
> things were once on this list and are off it, and saying so matters because
> each one used to imply cluster work an adopter does not have to do:
>
> - **No root-added host volume.** The jobspec declares none, and a test
>   asserts it stays that way. This one is worth being explicit about: a host
>   volume must be configured on every eligible client and the agent
>   restarted, and `nomad validate` cannot tell you the volume is missing — so
>   the failure would land at placement, on your cluster, about something you
>   never configured.
> - **No wrapper provider.** A lane onboards by naming its runtime
>   (`runtime_provider = "nomad"` on the agent, the nomad-only half in the
>   provider's `runtime_env.nomad`). The wrapper that used to be required is
>   deleted.
> - **No one-image-per-deployment limit.** `GC_NOMAD_AGENT_IMAGE` is
>   per-session, so a codex lane and a claude lane coexist on one deployment.
>
> What remains is the two in the paragraph above: a `gc` you cannot obtain,
> and an agent image you must build and host yourself.
>
> The pack now **refuses to start** on a `gc` missing the decisive family
> (`remote-worker-lifecycle`) rather than going dark, and `gc-runtime-nomad
> check` exits nonzero on one. That refusal names the family, the symptom you
> would otherwise have chased, and where the table is. It covers **one** of the
> nineteen families — the one that decides whether a seat can claim at all.
> For the other eighteen the old warning still stands: **the pack does not
> refuse, it goes dark**, and a lane reads as configured while serving
> nothing. Two measured examples are in `COMPATIBILITY.md`.
>
> What would have to become true for this line to be deleted: those families
> land upstream, or the lineage is published so a reader can build the `gc`
> this pack drives. Until one of those happens, treat everything below as a
> description of how the pack works rather than as instructions you can
> follow to a running system.

**[`COMPATIBILITY.md`](COMPATIBILITY.md) next.** It states what this pack
needs — Gas City, Nomad, clients, image, host, toolchain — and what it does
not support yet, with the measurement behind each claim.

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
├── runtime/        # nested Go module (zero gascity imports; one production dep, hashicorp/nomad/api)
│   ├── go.mod      # module github.com/gastownhall/gc-runtime-nomad (test-only require on ../fakenomad)
│   ├── main.go     # RPP op dispatch + env config
│   ├── client.go   # Nomad API client (register/dispatch/deregister/blocking reads/alloc-exec WS)
│   ├── exec_ws.go  # client-side RFC 6455 frame codec for the alloc-exec WebSocket
│   ├── jobspec.go  # parent job spec builder (job-template invariants) + optional log-shipper task
│   ├── sidecar.go  # session -> child-job-ID binding store + launched marker (sidecar state dir)
│   ├── staging.go  # wire start config, envArgvSafe classification, tar-over-exec-stdin builder
│   └── ops.go      # start/stop/is-running/list-running/provision/exec/relaunch/stage, stop-path fs egress
└── drills/         # nested Go module: L4 reconciler-sim harness — see drills/README.md
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
key table in this pack and it lives in Install -> part 1 -> step 6** - the copy
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
| Toolchain | The Docker image named by the **required** `GC_NOMAD_AGENT_IMAGE` carries the session toolchain; the binaries it must provide are one canonical list in one place, [`COMPATIBILITY.md`](COMPATIBILITY.md) → "The agent image" (this row deliberately does not copy it — the last copy here named a specific agent CLI, which contradicts the runtime/provider independence stated above, and omitted three binaries the runtime actually execs). The agent task declares no host volume, so nothing is delivered from the client host. `GC_NOMAD_AGENT_IMAGE` unset fails every lifecycle op closed — 1.0.7 removed the legacy `exec` task and the `GC_NOMAD_ARTIFACT_VOLUME` host-volume mount that used to feed it. |
| Identity/config | The city target arrives through the secrets environment channel as **exactly one** of a complete remote context (`GC_HOME` + `GC_CITY_CONTEXT`) or `GC_CITY_URL`; both at once is the fail-closed case, not a redundancy - see Install part 1 step 7. Session identity values are added by the lifecycle. |

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

Read the status banner at the top of this file first: step 1 below cannot be
completed outside the maintainer's own deployment today.

Everything here is provider-agnostic. The runtime and the agent **provider**
are separate abstractions: this pack schedules a box and feeds it; it does not
know, name, or ship what runs inside it. Part 1 is the runtime; part 2 is the
contract its provider side must satisfy. The concrete values any one
deployment fills in are not this pack's to carry.

### Part 1 — Runtime requirements

Provider-agnostic: what any deployment of this pack needs.

1. **`gc`, installed at a pin that carries the remote-worker families.**

   Download the published build for your platform and verify it, e.g.:

   ```sh
   REL=https://github.com/jacobhausler/gascity/releases/download/gc-75fbc2cf5b
   curl -fsSLO "$REL/gc-linux-amd64"  && curl -fsSLO "$REL/gc-linux-amd64.sha256"
   shasum -a 256 -c gc-linux-amd64.sha256     # or sha256sum -c
   install -m 0755 gc-linux-amd64 ~/.local/bin/gc
   ```

   `gc-darwin-arm64` is published alongside it. Verify the sha256 — the pack
   asks you to run a 260 MB binary from someone else's release page, and you
   should not do that on trust.

   **NOT `go install`.** It cannot work: the module path is
   `github.com/gastownhall/gascity`, so `go install` against the fork URL never
   resolves, and the upstream URL has no commit carrying these families. If you
   would rather build than download, clone the fork at that tag and
   `make build` — the ICU note in
   [`toolchain/TOOLCHAIN.md`](toolchain/README.md) applies to your own build
   host too.

   The families a pin must carry are not all upstream. Running an upstream `gc`
   is not a degraded mode — `gc-runtime-nomad check` fails closed and names the
   missing family, and a box whose seat cannot claim work is the failure this
   pack exists to make loud. `gc` is
   a tool dependency of this pack, never a Go import — the pack keeps zero gascity
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
2. **A Go toolchain where `go install` may run.** Build on the platform you
   will run on: the runtime binary is a Nomad API client and runs wherever
   `gc` does, but the `gc` that goes *inside* the agent image must be a
   linux/amd64 build (COMPATIBILITY.md, "The agent image").
   `install.sh` is that Go run plus a self-check: the launcher deploy it used
   to carry existed to serve the host-toolchain pane and is gone.
   It fails, with the exact `export PATH=...` line for your machine, when the
   binary lands somewhere `gc` cannot resolve it by bare name — then verifies
   the RPP handshake and runs `check`, so an incompatible `gc` is reported at
   install time rather than after you have built an image and wired a lane.
3. **A reachable Nomad API, plus two tokens:** a management token holding
   `submit-job` for the one-time parent-job registration (step 5), and the
   narrowed runtime token every session-path op runs on.
4. **Install the executable.** `./install.sh` runs `go install .` in
   `runtime/` and confirms `gc-runtime-nomad` landed on `PATH` (adding
   `$(go env GOBIN)` if it did not). This file *is* the pack's install step —
   the `pack-runtimes` `gc doctor` check then verifies the binary is installed
   and answers the RPP handshake (RUNTIME-SEL-011).
5. **Register the parent job once, with the management token.** Run
   `provision` or `start` once with `GC_NOMAD_TOKEN` set to that token; the
   registration is idempotent and stamps the jobspec fingerprint. After **any**
   jobspec change — task spec, node pool, DNS, constraints — re-register it the
   same way, or the pack reports the parent stale and refuses to dispatch.
   `GC_NOMAD_FORBID_REGISTER=true` makes an absent or drifted parent a
   fail-closed error instead of an attempted registration.
6. **Configure the runtime's environment.** These keys are read by the
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
   | `GC_NOMAD_SIDECAR_DIR` | yes | Session→child-job-ID bindings, launched markers, **and `diagnostics.log`** — this pack's own account of every failure and every dark-lane warning. Unset it and there is no durable record at all: gc's exec transport discards this pack's stderr, so the operator is left with an exit code and no cause. `gc-runtime-nomad check` prints the full path. |
   | `GC_NOMAD_TOKEN` | no | ACL token (`X-Nomad-Token`); the narrowed runtime token in normal use |
   | `GC_NOMAD_CACERT` | no | CA certificate path for direct Nomad TLS; overrides `NOMAD_CACERT` when both are set |
   | `GC_NOMAD_CLIENT_CERT` | no | Client certificate path for direct Nomad mTLS; overrides `NOMAD_CLIENT_CERT` when both are set |
   | `GC_NOMAD_CLIENT_KEY` | no | Client private-key path for direct Nomad mTLS; overrides `NOMAD_CLIENT_KEY` when both are set |
   | `GC_NOMAD_TLS_SERVER_NAME` | no | TLS server name/SNI for direct Nomad TLS; overrides `NOMAD_TLS_SERVER_NAME` when both are set |
   | `GC_NOMAD_NAMESPACE` | no | Nomad namespace (default `default`) |
   | `GC_NOMAD_PARENT_JOB` | no | Parameterized parent job ID (default `gc-sessions`) |
   | `GC_NOMAD_NODE_POOL` | no | Parent job's node pool (empty = Nomad's own default pool) |
   | `GC_NOMAD_NODE_CONSTRAINT` | no | `;`-separated Nomad-syntax constraints appended *after* the pack's linux-kernel fatal rule, e.g. `${node.unique.name} != prod-nomad` |
   | `GC_NOMAD_DNS_SERVERS` | no | Resolver addresses the session group's bridge network writes into the alloc's `/etc/resolv.conf` — see step 9 |
   | `GC_NOMAD_AGENT_IMAGE` | **yes** | Docker image carrying the session agent and its toolchain (see the image-content note below). Unset fails every lifecycle op closed at startup: the Docker task is the only session shape |
   | `GC_NOMAD_EXEC_TASK` | no | Task name alloc-exec targets, when it is not the pack's session task |
   | `GC_NOMAD_EXTRA_SECRET_KEYS` | no | Comma-separated names to bridge from the supervisor's own process env into `$NOMAD_SECRETS_DIR` — the value-blind channel for a secret `gc`'s provider env map cannot interpolate. Naming a key is what stages it; an unnamed key is never staged, and an extra key is always routed to the secrets dir, never to the launch argv. The names themselves belong to the deployment (part 2 works one Codex name through it) |
   | `GC_NOMAD_EGRESS_DIR` | no | Stop-path transcript/evidence directory; unset disables egress |
   | `GC_NOMAD_FORBID_REGISTER` | no | `true` = never register the parent; fail closed instead. Unset = the default (registration allowed); a value that is not a boolean is a config error and the op stops naming this variable rather than defaulting to allowed |
   | `GC_NOMAD_ADOPT_MAX_AGE` | no | Go duration capping how old a live-but-unlaunched box may be before it is reaped and replaced instead of adopted (default `30m`; unparseable is a config error) |
   | `GC_NOMAD_LOG_SINK` | no | The one key that adds the `log-shipper` task; unset ⇒ session logs are not shipped |
   | `GC_NOMAD_LOG_SINK_TOKEN_FILE` | no | In-box path the shipper reads its bearer token from |
   | `GC_NOMAD_LOG_LABELS` | no | `k=v,k=v` labels merged onto the fixed `session_name`/`alloc_id`/`node`/`runtime=nomad` set |
   | `GC_NOMAD_GC_ARTIFACT_URL` | no | URL **template** for the linux/amd64 `gc` each box fetches per allocation. `{commit}` is substituted with the commit of the `gc` DRIVING this pack, so the URL follows your pin without being edited. Nomad verifies it against a `.sha256` sidecar published beside it before the task starts, and the fetched copy goes first on the box's PATH. Unset ⇒ boxes use whatever `gc` the image carries, which only WARNS when it is older than the host. **A pin roll that does not publish an artifact for its new commit leaves every box fetching a 404 and dying before the task runs** — see `skills/gc-nomad-debug/SKILL.md` and the publishing note below the table |
   | `GC_NOMAD_AGENT_IMAGE_TAR` | no | URL of a `docker save` tarball of the agent image, fetched per allocation and imported with the docker driver's own `load`. Removes the need for a registry AND the hidden placement pin a bare local tag creates (only the node that built it can run it). Checksum-verified before load; pair with `GC_NOMAD_AGENT_IMAGE_TAR_SHA256`, or publish a `.sha256` sidecar and let it be derived |
   | `GC_NOMAD_AGENT_IMAGE_TAR_SHA256` | no | Explicit checksum for the tarball above; unset derives `file:<url>.sha256` |
   | `GC_NOMAD_LOG_SHIPPER_ARTIFACT` | no | URL or local path for the pinned Vector archive; unset uses the upstream release URL |

   The runtime also honors Nomad's standard `NOMAD_CACERT`,
   `NOMAD_CLIENT_CERT`, `NOMAD_CLIENT_KEY`, and `NOMAD_TLS_SERVER_NAME`
   variables. The `GC_NOMAD_*` aliases take precedence per variable when both
   forms are present. Values are paths only: certificate and key contents stay
   in deployment custody and never enter a jobspec, argv, log, or receipt.

   **Publishing the per-allocation `gc` (and what happens if you don't).**
   `GC_NOMAD_GC_ARTIFACT_URL` is the one key whose correctness is a function of
   your PIN rather than of your config, and that asymmetry has bitten: the
   template resolves `{commit}` to the live pin, so **the moment you roll the
   pin, every box starts fetching a URL that must already exist.** If it does
   not, allocations fail with `Failed Artifact Download` before the agent task
   starts — which is worse than a stale in-box `gc`, because that only warns.
   Two things follow, and neither is optional:

   - **Publish for the new commit as part of the roll, not after it.** Build the
     linux/amd64 `gc` for that commit, publish it plus a `.sha256` sidecar at
     the path your template resolves to, and only then move the pin. If your
     roll is scripted, the publish belongs in the same script — and it must NOT
     be conditional on a post-roll health check, because the URL is already live
     the instant the pin moves. (That exact coupling caused an outage here.)
   - **Build it against the agent image's own base.** A `gc` compiled on a
     different distribution links a different ICU soname, loads cleanly on its
     build host, and dies in the box with
     `libicui18n.so.NN: cannot open shared object file`. Verify by RUNNING it in
     the image (`docker run --rm -v …:/out <image> /out/gc version --json`), not
     on the machine that built it.

   **Immediate mitigation** if you are already in this state: unset
   `GC_NOMAD_GC_ARTIFACT_URL` and cycle the supervisor. Boxes fall back to the
   image's `gc`, which the pack reports as stale rather than failing on.

   **How you actually get an image.** This was the most common dead end for
   people trying to install this pack, because the answer was scattered across
   caveats instead of written down. It is two layers, and neither alone is the
   agent image:

   1. **The toolchain base**, built by this pack:

      ```
      toolchain/build-image.sh check                        # verify the lock offline first
      toolchain/build-image.sh build --gc-release=75fbc2cf5b
      ```

      `--gc-release=<commit>` fetches the `gc` published for that pin and
      **verifies it against its own sha256 sidecar** before building. You do
      not have to build a linux `gc` yourself, which was the second
      prerequisite chain that stopped people here: the image is required, and
      producing it used to require the one thing the pack could not hand you.
      A binary that does not match its published checksum is refused, and so is
      a binary published with no sidecar at all.

      If you would rather stage your own `gc` — building it yourself is
      strictly stronger than trusting a published checksum, and
      [`COMPATIBILITY.md`](COMPATIBILITY.md) explains how — the original path
      is unchanged:

      ```
      toolchain/build-image.sh build --artifact=<path to your linux/amd64 gc>
      ```

      This is the checksum-pinned session toolchain — `gc`, `bd`, `git`,
      `tmux`, `codex`, `libicu` — pinned in `toolchain/TOOLCHAIN.lock`. It must
      be built on a host where a **linux/amd64** `gc` is staged; a darwin or
      arm64 build cannot ship in a linux/amd64 image. Supply the checksum and
      pin alongside the path (`GC_LOCAL_ARTIFACT_SHA256`,
      `GC_LOCAL_ARTIFACT_PIN`) — see `toolchain/TOOLCHAIN.lock`.

   2. **City content composed onto it**, by gc:

      ```
      gc build-image <city-path> --tag <your-registry>/<name>:<tag>
      ```

      Point `GC_NOMAD_AGENT_IMAGE` at the result.

   One caveat about step 2, stated because it misleads people: `gc build-image`
   is documented entirely in Kubernetes terms — Pods, init containers, K8s
   Secret mounts, `prebaked = true` in `[session.k8s]` — and at the pinned gc
   it names Nomad zero times. That is a **documentation** gap, not an artifact
   one. What it emits is an ordinary OCI image, and this pack runs it as a
   docker task. You do not need a Kubernetes cluster to use it.

   Image *content* beyond that is not this pack's contract.

   **Per-role images.** `GC_NOMAD_AGENT_IMAGE` is not only a deployment-wide
   default. The pack reads it from each session's own start payload
   (`sessionAgentImage`), carries it as Nomad meta `gc_image`, and interpolates
   it into the child job's `config.image`. Set it in a provider's
   `runtime_env.nomad` and that role gets its own toolchain while every other
   lane keeps the default:

   ```toml
   [providers.my-codex-lane.runtime_env.nomad]
   GC_NOMAD_AGENT_IMAGE = "registry.example/agent-codex:1.4"
   ```

   Empty means "use the deployment default the parent job already carries", so
   adding one role's image never disturbs the others. This is what lets a codex
   lane and a claude lane coexist in a single deployment.

   The identity/config side of the allocation is *not* configured here:
   `GC_CITY_URL`, `GC_CITY_NAME`, and whatever transport credentials the
   endpoint needs arrive through the provider env / secrets channel, and session
   identity (`GC_SESSION_ID`/`GC_AGENT`/`GC_SESSION_NAME`) is added by the
   lifecycle.
7. **Give the allocations a city endpoint they can actually reach — named
   exactly once.** An allocation cannot reach the supervisor's loopback
   listeners, so a remote session needs either a complete **remote context**
   (`GC_HOME` + `GC_CITY_CONTEXT`, staged by the pack as `gc-home/contexts.toml`
   — an mTLS context in the maintainer's own deployment) or `GC_CITY_URL`. Both at once is a
   failure, not a redundancy: when the provider env carries a complete remote
   context, the pack stages `gc-home/contexts.toml`, exports `GC_HOME` +
   `GC_CITY_CONTEXT`, and then **drops `GC_CITY_URL` from `env.sh`** — a target
   named twice makes `gc` fail closed. It also strips the local-target family
   (`GC_CITY`, `GC_CITY_PATH`, `GC_CITY_ROOT`, `GC_CITY_RUNTIME_DIR`,
   `GC_BEADS*`) before the launch argv filter runs. Do not work around either
   by re-adding them to the provider env.
8. **Stage what must be secret at runtime, value-blind.**
   `GC_NOMAD_EXTRA_SECRET_KEYS` is generic: name a key and the pack bridges that
   *name* from the supervisor's own process env into `$NOMAD_SECRETS_DIR`
   without the value ever entering a jobspec, an argv, or this repository. The
   credential NAME is the deployer's choice — the runtime has no credential
   vocabulary and none of its own keys to reserve; the box installs what it
   finds there.
9. **Resolvers and placement are configuration, not cosmetics.**
    `GC_NOMAD_DNS_SERVERS` unset means Nomad declares no `dns` stanza, the alloc
    inherits the **client host's** `resolv.conf`, and on a systemd-resolved host
    that is the `127.0.0.53` stub — unreachable from the alloc netns, so nothing
    inside the box resolves a name and every failure downstream looks like an
    agent or network defect. Give it real resolver addresses (a non-address
    value is a config error, not a warning). Placement has the same trap in a
    different shape: the node pool must actually span the boxes you expect work
    to land on, and the pack appends `GC_NOMAD_NODE_POOL` /
    `GC_NOMAD_NODE_CONSTRAINT` to the parent job — so either is a jobspec change
    and needs step 5's re-registration to take effect.
10. **Check it without a cluster.** `gc-runtime-nomad check` warns when log
   shipping is unconfigured, and when `GC_NOMAD_AGENT_IMAGE` is unset — the
   session task is a Docker task, so every lifecycle op fails closed without it.
   `GC_BIN=$(command -v gc) ./conformance.sh` runs `gc runtime check` and the
   full RPP lifecycle round-trip against the in-memory fake Nomad API — no
   cluster, no network.

### Part 2 — Provider toolchain in the box

The rule, in the abstract: **the box must provide the seat's provider command on
`PATH` exactly as `gc` renders it.** The command line the provider config
produces for a session has to resolve and run inside the allocation, unchanged,
with whatever siblings that command assumes present alongside it. The runtime never downloads or installs a provider, and the pane it launches
runs gc's rendered launch command and nothing else. If a provider
needs a file, a binary, or a credential, that is the deployment staging it
(the prebaked image, the value-blind secret channel). A seat that comes up but
cannot run a tool is a toolchain-staging defect wearing a runtime costume.

**One exception, and it is a real one.** `runtime/codex_home.go` is
provider-specific: the runtime detects a Codex-shaped secret environment and
stages that provider's home itself — `CODEX_HOME`, `config.toml`, `auth.json`,
`model_providers`, `wire_api`. This document used to claim the runtime "never
names … a provider", which was simply false, and the claim is worth retiring
loudly rather than quietly: **runtime and provider are supposed to be
independent axes.** Any lane should be able to keep its provider and change
only its runtime. Every provider-specific branch in the runtime is a place
that independence is not real yet, and this is the one that exists today. It
is a known defect, not a design.

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
part 1 step 8 under whatever name the deployment chose (here `CODEX_AUTH_JSON`),
staged value-blind into `$NOMAD_SECRETS_DIR`, and the pack installs it into the
box's own `$CODEX_HOME` from that staged environment (`codex_home.go`) — image
mode owns the agent home, so no launcher step does.

**The pinned build input is `toolchain/TOOLCHAIN.lock`, and it is the pointer
this section's own rule demands.** The rule above is that a toolchain staging
receipt names the artifact, its sha256, and the layout it asserts; that lock is
exactly that, per component (gc, bd, git, tmux, the Codex standalone package)
and per image (base, builder, agent, rollback) — digest-pinned references, never
floating tags. `toolchain/build-image.sh` reads every value in it and passes
each as a build ARG, `toolchain/Dockerfile` consumes the ARGs, and
`toolchain/BUILD-RECEIPT.md` carries the measurements — including the pins that
were rejected and why. It ships no delivery layer: nothing in this pack
publishes an image, and the `gc` pin the lock must be read alongside belongs
to whoever operates the deployment.

### Building the agent image

`GC_NOMAD_AGENT_IMAGE` is required and there is no default, so this is a step
every deployment has to do. It is two images: a **base** carrying the session
toolchain, then your **city content** composed onto it. `toolchain/` ships
both halves; `toolchain/build-image.sh --help` lists every verb, and the ones
that matter here are:

```bash
# 0. validate the pin file (offline; no docker, no network)
bash toolchain/build-image.sh check

# 1. base toolchain image. Needs docker and a linux/amd64 host.
#    The gate below is not ceremony: this builds an image, and a build that
#    runs inside an agent allocation would be a session building its own
#    successor. Run it on a build host, never in a box.
GC_TOOLCHAIN_HOST_STEP=1 bash toolchain/build-image.sh build --published

# 2. compose city content onto that base -- this is `gc build-image`, and it
#    is the step that produces the tag you actually run
bash toolchain/build-image.sh city-image --city=<city-root> --tag=<image:tag>

# 3. the result is what GC_NOMAD_AGENT_IMAGE points at
export GC_NOMAD_AGENT_IMAGE=<image:tag>
```

Three things that will bite you:

- **`--city` must be a bounded city root**, not a working checkout.
  `gc build-image` copies the whole directory and excludes only secrets, so
  pointing it at a live working checkout ships that whole checkout into an
  image. The script refuses it.
- **`--published` builds against the published upstream `gc` release.** That
  is the portable path, and today it produces an image whose in-box `gc`
  cannot run this pack — see the status banner at the top of this file. This
  is the same blocker, not a second one. Without `--published`, `build`
  prefers a locally staged pin binary named in `TOOLCHAIN.lock`, which only
  exists on a host that stages one.
- **The in-box `gc` must be built for the image it runs in.** A `gc` compiled
  against a different libc or ICU major links cleanly and dies at the first
  seat command; `build-image.sh` refuses a `gc` that is not an x86-64 ELF for
  exactly this reason.

`toolchain/TOOLCHAIN.lock` is the checksum pin of record for every component
and image, and `toolchain/BUILD-RECEIPT.md` carries one maintainer's measured
run of the above — including the pins that were rejected and why. Neither is
a set of values to copy.

Prebaking the toolchain into the box image is the intended 1.0.x shape —
`gc build-image` is the prebake verb gc ships - its contract at the pin is
K8s-shaped, see the `GC_NOMAD_AGENT_IMAGE` row in step 6 - and the `toolchain/`
directory is that work; manual staging as above is what runs today.

### Deployment values live outside this pack

The keys above are a contract, not a set of values. What any one deployment
fills in — API endpoint and mTLS custody, host-firewall allowlist, resolver
address, whether placement is enforced or emergent — is not this pack's to
carry, and nothing another deployment chose is a default to copy.

Keep that record wherever your team keeps such things, and **read it before
you debug an alloc egress timeout**. That is the lesson worth porting from
the deployments that have already hit it: a host missing from your firewall
or allowlist table fails in a way that is indistinguishable from a broken
runtime. The runtime cannot tell you the difference, because from inside the
alloc the two look identical.

## Debugging

The pack ships its own diagnostic skill at
[`skills/gc-nomad-debug/SKILL.md`](skills/gc-nomad-debug/SKILL.md). `gc`
discovers a pack's skills by convention from `<packDir>/skills/`, so it needs
no declaration and arrives with the pack — an adopter gets the diagnosis, not
just the runtime.

It exists because the failure this runtime produces is almost never a crash:
it is a box that comes up, looks healthy, and does nothing. The first
instruction is `gc-runtime-nomad check`.

Its checkable claims are guarded by tests (`runtime/skill_doc_test.go`): the
fatal placement rules it names are read off the live jobspec, and the probe and
entry point off the runtime. A skill is documentation that gets *followed*
rather than read, so a line that has quietly become false is worse than one
that is missing — a line in this pack's deployment-side sibling skill was
falsified by a change in this very repo and went unnoticed until it was graded
by hand.

## Conformance

```bash
GC_BIN=$(command -v gc) ./conformance.sh
```

`conformance.sh` builds the executable and an in-memory fake Nomad API
server (`fakenomad`), then runs `gc runtime check` and `gc runtime
conformance` through the full RPP lifecycle round-trip — no live Nomad
cluster or network required. This is the pack's CI gate; `gc`
itself is a tool dependency — a published binary on PATH (Part 1, step 1),
never a Go import — so the pack keeps zero gascity Go dependencies. Optional-op requirements
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
at, to `CONFORMANCE-RECEIPT.md`. Exits non-zero if any ladder item is red.
Fixing a red ladder step is out of scope for the receipt itself — bounce it
back to whoever owns the failing rung.

## RPP operations

| Op | Notes |
|----|-------|
| `check` | Pack-local diagnostic. Emits warnings when the optional log sink is unset, and when the required `GC_NOMAD_AGENT_IMAGE` is unset; does not require Nomad configuration. |
| `protocol` | `{"version":0,"capabilities":["proc.provision","proc.exec"]}` |
| `provision` | Registers the parent job (idempotent upsert) if needed, then dispatches a tmux-only child for the session — no agent launched, sidecar launched marker stays unset. Idempotent over the box it already made: a session whose bound child is still live but never launched is ADOPTED (no second child dispatched, staging re-run), so a controller re-driving `provision` after an attempt whose staging or launch never confirmed can finish the job instead of collecting "already exists" forever. A session whose child is live AND launched is a genuine duplicate and is still rejected with an "already exists" error (the wire-contract constant). Reads an optional staging config (JSON) on stdin and materializes it (see `start`'s staging row) before returning. |
| `start` | `provision` + launch: dispatches, stages an optional workspace/secrets config read as JSON from stdin (`staging.go`'s `stageConfig` — workspace files land under `WorkDir` and env entries land as individual files plus a sourceable `env.sh` under `$NOMAD_SECRETS_DIR`, both via tar-over-exec-stdin), then sources that script and execs the launch command (`tmux new-session -d -s main`, plus `-e KEY=VALUE` for any `envArgvSafe`-classified env entries) into the alloc, then sets the launched marker. Empty/absent stdin is a no-op config — pre-staging callers are unaffected. A session left live-but-unlaunched by an earlier attempt (a `provision`, or a `start` whose launch never confirmed) is resumed into rather than re-dispatched; only a live, already-launched session is rejected, with the same "already exists" error as `provision`. |
| `exec` | Runs a command inside the session's current alloc over the Nomad alloc-exec WebSocket and streams its stdout; **the op's own exit code is the remote command's exit code** (RPP-CONN-001) — not the 0/1/2 lifecycle-op convention. Works regardless of the launched marker — a provisioned-but-not-launched box already answers exec (RPP-PROVISION-001). |
| `relaunch` | Re-execs the launch command into the SAME alloc — no fresh dispatch, no fresh env — then sets the launched marker (warm relaunch, launch-only fingerprint drift). Fails if the session has no live alloc to relaunch into. |
| `nudge` / `peek` / `interrupt` / `send-keys` / `clear-scrollback` | The driving verbs, realized as tmux commands sent into the session's tmux pane over the same exec mechanism. `nudge`/`interrupt` are best-effort — a not-found session answers success rather than an error, per the RPP's best-effort convention. |
| `stop` | Best-effort `tmux kill-session` for the agent's session before anything else — the task's own exit is independent of this (its supervisor script traps and exits on Nomad's own SIGTERM), but tmux's server is a detached daemon nothing else reaps. When log shipping is enabled, requests and waits for the `log-shipper` graceful flush. If `GC_NOMAD_EGRESS_DIR` is set, then copies the session's transcript/evidence files (via the Nomad client fs API) into it, retrying up to 3 times before giving up and marking the tombstone `evidence_lost` rather than wedging; a flush failure also records `evidence_lost`, while a successful egress receipts completion in the sidecar instead. The order is agent stop → shipper flush → egress → deregister. Then deregisters the session's child job without purge, confirms terminal via a blocking read, and tombstones the sidecar binding. Idempotent — a session with no binding is a no-op success, and a stop that fails after egress but before deregister does not re-attempt egress on retry. |
| `is-running` | Prints `true`/`false`. False whenever the launched marker is unset, even if the alloc is running (RPP-PROVISION-001: "provisioned, agent never launched" reads as not-running). Once launched, it also execs an in-box liveness probe into the alloc (`tmux has-session -t main`, plus `kill -0 <pid>` on the pane pid `start`/`relaunch` captured) — a box whose alloc stays non-terminal but whose agent died inside it (the in-box agent-kill case) answers `false`, not `true`; ClientStatus alone can never see that gap. The honesty split still governs every transport fault along the way — a lookup error listing allocs, or a transport error execing the probe itself — which answers last-known-good (`true`) rather than `false`; only a definitive, non-transport probe result (the tmux session/pid genuinely gone) reads as agent-dead. |
| `list-running [prefix]` | Prints one running session name per line — launched sessions only. Enumerates the children-of-parent jobs list (`GET /v1/jobs?meta=true`) rather than trusting the sidecar as the existence source, decodes each non-terminal child's `gc_session` Meta key, and — when a prefix argument is given — filters the decoded names to it (`ListRunning(prefix)`). The sidecar is still consulted for the launched marker, which the children list alone cannot answer. Exits 1 on any lookup error rather than returning a partial list. |

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

- Retention policy for egressed transcript/evidence files (an owner
  decision) — no directory listing/discovery either, just the two
  well-known files (`ops.go`'s `egressFiles`). (The older version of this
  bullet also scoped out any egress sink beyond a local directory; that is
  overruled by the `GC_NOMAD_LOG_SINK` HTTP sink — see above. The stop-path
  file egress is still local-directory only; it is a distinct mechanism from
  the log-shipper's live streaming.)
- Remote artifact blocks and the env.ledger tunnel — staging
  only materializes what a caller sends inline on stdin. The tmux pane's
  initial command is gc's own rendered launch command (plus `-e` flags) run
  after sourcing the staged environment, and an empty rendered command fails
  the launch closed (`ops.go`'s `buildLaunchCommand`). The deployment-owned
  bootstrap seam that used to sit in front of it — a script the pane execs,
  carrying a PATH prepend, an agent HOME pointed inside `$NOMAD_SECRETS_DIR`
  and a staged-auth install — no longer exists: the prebaked image owns the
  agent toolchain and the pack stays agent-agnostic.
  The session task's own command (`jobspec.go`'s
  `sessionSupervisorScript`) is a real long-lived
  trap-and-loop that keeps the alloc alive until stop — no longer the
  `/bin/true` placeholder that used to exit immediately on a real client —
  but it carries no agent itself; the agent is still launched afterward as
  a separate detached tmux-client command over alloc-exec.
- `PackOverlayDirs`/`OverlayDir` merge semantics (the overlay-precedence
  rules) — `stageConfig.Files` is this pack's own flat CopyFiles-analog
  channel; provider-side overlay resolution is not reimplemented.
- The fuller sidecar record (dispatch-attempt counter, disputed-children
  ledger, staleness datum) — this pack's sidecar binding is scoped to
  exactly what start/stop/is-running/list-running/provision/exec/relaunch
  need. The single-flight lock concurrent same-name `start`/`provision`
  needs is a per-session `flock` on a file in the sidecar
  directory rather than a sidecar-record field — it has to survive across
  provider processes, since `gc` execs this binary fresh per op (a race is
  between two OS processes, never two goroutines in one).
