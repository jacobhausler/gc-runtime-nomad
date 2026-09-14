# Compatibility

What this pack needs to run, stated plainly, including where it does not
work yet. Every version and requirement below was measured against a
running system on the date given, not read off a changelog.

Read this before `install.sh`. The single most important line is the
first one under **Gas City (`gc`)**: today this pack does **not** run on a
released upstream `gc`.

**Three things that stop a deployment dead, stated here so you do not read 200
lines to find them:**

1. **A pinned `gc`, not an upstream release** — see the next section. A build
   is published; running it is a decision, not a download.
2. **linux/amd64 only**, enforced as a hard constraint on `${attr.cpu.arch}`.
   There is no arm64 path anywhere in the stack — not the agent image, not the
   in-box `gc`, not the log shipper. On an all-arm64 fleet every dispatch stays
   pending with a constraint failure. If your cluster is Apple Silicon or Pi
   nodes, stop here. (Detail under **Nomad clients**.)
3. **The `docker` task driver on every eligible client**, plus `exec` if you
   enable log shipping. The session task IS a docker task and there is no other
   session shape to fall back to.

---

## Gas City (`gc`) — READ THIS FIRST

**Required: a `gc` carrying behaviour families that are not all upstream. No
released upstream `gc` can run this pack. A build of the pin this pack is
tested against IS published, for linux/amd64 and darwin/arm64, with sha256
sidecars: <https://github.com/jacobhausler/gascity/releases>.**

### Auditing what that build is, before you run it

It is a fork build, and you should not run a 260 MB binary from a stranger's
release page on trust. The fork is public, so the source at that commit is
readable and the build is reproducible from it:

```sh
git clone https://github.com/jacobhausler/gascity gascity-fork && cd gascity-fork
git remote add upstream https://github.com/gastownhall/gascity && git fetch upstream
git log --oneline upstream/main..75fbc2cf5b      # what the fork adds
make build COMMIT=75fbc2cf5b VERSION=dev         # reproduce the published binary
```

**What you are looking at is a PINNED BUILD, not an abandoned fork, and the
difference is a discipline with a name.** The maintainer's estate runs the
EDGE protocol: every local deviation from `gastownhall/gascity` is a hot-patch
that must have an **upstream twin** — a filed PR or issue — and an **expiry
equal to that twin's merge.** When the twin lands, the local patch is dead
weight and the next pin roll drops it. A nightly audit files findings against
anything local that cannot name its twin and an expiry date.

So "these commits are not upstream yet" is the steady state of that model, not
a warning sign, and open PRs are not a stalled release — the pack's own
co-owner runs the same way, with his own queue of pending upstream PRs. If you
already run a pinned `gc` (and if you are considering this pack, you probably
do), this is the operating mode you are in.

For scale: the pin sits 97 commits ahead of `upstream/main` and 100 behind it
(measured 2026-09-12) — but that number is **not your audit surface.** Most of
it is the maintainer's own city work that merely shares the lineage. **Six
families are load-bearing for this pack**, and each one's upstream twin and
status is tabulated in
[`docs/PIN-DEPENDENCIES.md`](docs/PIN-DEPENDENCIES.md). Audit those six.

If your policy forbids running anything but an upstream release binary, this
pack is not for you yet, and no amount of framing changes that — the families
it needs are genuinely not upstream. Tracking their twins is how you know when
that stops being true.

This is not a version floor that a newer release will eventually clear on
its own. The pack rides a *city pin* carrying ~19 behaviour families.

**Carrying is not requiring**, and the difference is large enough to
state up front. Sorted by whether the pack can function without them
(measured against the upstream base commit, `6ba434271`), the blocking
set is **six families: two with filed PRs, and four unfiled follow-ups
that all stack on one of those two.** The rest are either needed only for
particular usage patterns, or change pool and dispatch behaviour
identically for a local lane and a Nomad lane — which makes them upstream
questions rather than requirements of this pack. Two are performance rows
by their own description, and one, `runtime-provider`, turns out not to
be required to select this pack at all: upstream already resolves
`session = "nomad"` through its pack-runtime registry.

`docs/PIN-DEPENDENCIES.md` carries the tiering, the reasoning for each
placement, and the full table. That file is the authority; this is the
summary.

The failure mode when the dependency is missing is the reason this
section is first: **the pack does not refuse, it goes dark.** The
measured example:

- Without `remote-worker-lifecycle`, a seat inside an alloc cannot claim
  at all. It starts, connects, and drains with
  `{"action":"drain","reason":"no_work"}` while the city lists its work
  as ready.

**Retracted: a second example used to stand here.** It claimed that
without the `runtime-provider` seam, `gc` drops `runtime_provider` and
`[[agent_groups]]` while parsing the city config, so this pack's runtime
is never selected. That is not true of a stock `gc`, and this page
asserted it thirty lines after saying the same family "turns out not to
be required" — the page contradicted itself, and the wrong half was the
one written as measured fact. Upstream resolves `session = "nomad"`
through its own pack-runtime registry; the selection path is present on
`origin/main`. What remains true, and is the honest version of the same
point: `runtime_provider` IS pin-only as a config key (zero occurrences at
`6ba434271`), so per-LANE selection needs the pin even though selecting the
pack at all does not. Upstream's route is the city-wide `[session] provider`,
which moves every session onto this runtime. See docs/PIN-DEPENDENCIES.md. The retraction is recorded rather than deleted because a
doc that overstates what it needs from its host is the specific way this
kind of page misleads an adopter, and the shape is worth keeping visible.

**That one now fails closed.** `remote-worker-lifecycle` is the family
that decides whether a seat can claim work at all, so the pack probes for
it before minting a box and refuses when it is absent:

```
$ gc-runtime-nomad check
FAIL: refusing to start: the `gc` on PATH does not carry the
remote-worker-lifecycle family, so a seat in an allocation cannot claim
work through /v0/city/{city}/worker/* ...
```

`check` exits nonzero, and `start`, `provision` and `relaunch` refuse the
same way. `stop`, `is-running` and `list-running` deliberately do **not**
— a box that already exists has to stay stoppable, or a startup refusal
becomes stranded allocations.

The probe is a local `gc bd claim` under a remote target, which both
lineages refuse at the target gate *before any network dial*: no city, no
token, no reachable host, ~30ms. It distinguishes them by which refusal
comes back.

Three outcomes, and the difference between the last two matters:

| outcome | meaning | box-minting ops |
|---|---|---|
| present | the family is there | proceed |
| missing | this `gc` answers with the upstream capability gate | **refuse** |
| absent | no `gc` answered — nothing on PATH, or it printed nothing | **refuse** |
| unknown | a `gc` answered in a shape this pack does not recognise | proceed |

**Unknown proceeds; absent does not.** An unfamiliar `gc` is not evidence
of a missing family, and a future `gc` may legitimately reword its
refusal — so refusing a session over an unparsed string would be worse
than the fault. But "no `gc` answered" is not an unparsed answer, it is
no answer, and this pack shells out to `gc` for everything a seat does.
Absent used to be folded into unknown, which meant the deployment least
likely to have the right `gc` — a stranger installing from public sources
who has not installed `gc` yet — was the one waved through into a green
box with a dark seat. It gets its own refusal, naming the absence rather
than the dependency table, because "gc is not installed" and "gc is the
wrong build" have different fixes.

If you are running an offline harness that mints no real sessions, set
`GC_NOMAD_SKIP_GC_CAPABILITY_GATE=1`. It warns on every use, loudly, and
in a real deployment it restores the dark lane.

**The other eighteen families are still undetected**, and for those the
paragraph above stands unchanged: no error names this pack. Budget for
that when you are deciding whether to adopt.

**Version numbers are useless here; use the commit.** The `gc` this pack
was last exercised against reports:

```
$ gc version --json
{"commit":"e78e884c1b", ... ,"version":"dev"}
```

`version` is the literal string `dev` on every build of this lineage, so
a version comparison can only ever be vacuous. The `commit` field is the
only real identity. Quote it in any bug report.

**Protocol.** The pack implements Runtime Provider Protocol **v0** and
declares exactly two capabilities:

```
{"version":0,"capabilities":["proc.provision","proc.exec"]}
```

**What a NEWER `gc` sees.** RPP is versioned for forward compatibility and the
pack holds up its end: an operation this build does not implement exits **2 and
prints nothing**, which is the protocol's "not implemented" answer and not an
error. A `gc` speaking a later protocol therefore degrades op-by-op rather than
failing wholesale — it loses whatever it asked for that this build lacks, and
keeps everything else. Two consequences worth knowing before you debug one:

- An op you expected to work being a silent no-op is this contract, not a
  fault. `gc-runtime-nomad help` lists exactly what this build serves.
- **An OLDER or differently-versioned `gc` is not covered**, and a reader was
  right to notice that only the newer direction was discussed. The pack
  declares v0 unconditionally; it does not inspect what `gc` asked for. If your
  `gc` speaks a protocol whose v0 means something else, nothing here will
  detect that — the handshake is a declaration, not a negotiation, and the
  failure would surface as an op behaving unexpectedly rather than as a version
  error. There is no version of `gc` known to be in that position, which is why
  it is untested rather than unsupported.
- The pack does **not** negotiate downward. It declares v0 and two
  capabilities; it does not inspect what `gc` asked for and adapt. If a future
  `gc` requires a capability listed below as deliberately undeclared, the honest
  answer is that this pack has not implemented it, and the version to change is
  this one.

`env.workspace`, `env.tooling`, `env.identity` and `env.transcripts` are
deliberately **not** declared. Workspace and secret staging do happen —
but the start wire does not materialize `work_dir` in the session, so
declaring `env.workspace` would convert a SKIP into a FAIL under
`gc runtime conformance`. Do not re-declare it without changing the start
wire first.

---

## Nomad cluster

**Exercised against Nomad v2.0.5** (`nomad version`, 2026-09-12). No
lower bound has been tested, so none is claimed. The features the pack
requires, if you want to reason about an older cluster yourself:

**No floor is claimed, but here is what a floor would be made of.** "You would
be the first tester" is a true statement and a useless one, so instead: the
pack's entire Nomad surface is listed below, newest feature first. Check the
ones you care about against your own version's changelog rather than taking a
number from us.

| What the pack uses | Where |
|---|---|
| **`NodePool` on the job** — by a wide margin the newest thing here, and the likely binding constraint. A Nomad without node pools cannot register the parent at all | parent jobspec |
| `ParameterizedJob` + `MetaOptional`, and the behaviour that an **undeclared** dispatch meta key is refused outright | parent jobspec, every dispatch |
| `artifact` with `GetterMode` and `GetterOptions` (`checksum`, `archive`) | the fetched `gc`, the optional image tarball, the log shipper |
| the docker driver's `load` | optional, registry-free image delivery |
| `template` stanzas as static file delivery | log shipper config |
| alloc **exec** (the websocket endpoint) | every staging and in-box probe |
| `X-Nomad-Index` on write responses — the pack tolerates a proxy that strips it | dispatch bookkeeping |
| `/v1/jobs`, `/v1/job/…`, `/v1/client/allocation/…`, `/v1/client/fs/cat/…` | everything |

Everything on that list except `NodePool` has been in Nomad for years. If your
version has node pools and refuses undeclared dispatch meta, the pack has no
other known reason to object — but "no known reason" is not "tested", and the
only version anyone has actually run it against is the one above.

| feature | used for |
|---|---|
| parameterized batch job + dispatch | one parent job `gc-sessions`; every session is a dispatched child |
| dispatch `meta_optional` | `gc_session`, `gc_nonce`, `gc_created_at` — Nomad rejects any key not declared here with HTTP 500, so an undeclared key fails **every** provision |
| alloc-exec over WebSocket | `GET /v1/client/allocation/:alloc_id/exec` — this is how every command reaches the session |
| task `lifecycle` poststart sidecar | the log-shipper task |
| artifact stanza with `checksum` | fetching the pinned Vector binary |
| `X-Nomad-Index` on write responses | the pack tolerates a proxy that strips it, but native Nomad supplies it |
| namespaces | `GC_NOMAD_NAMESPACE`, defaulting to `default` (`newClient`, runtime/client.go) — the pack creates no namespace, so if you set one it must already exist |

**ACLs.** A token is optional ONLY if your cluster has ACLs off
(`GC_NOMAD_TOKEN`, sent as `X-Nomad-Token`). With ACLs on it is required,
and its absence is the most expensive misconfiguration this pack has:

> A deployment ran with no `GC_NOMAD_TOKEN` reaching the runtime. Every op
> that MINTS a box kept working, so nothing looked broken. `is-running` has
> no evidence except the allocation list, could not read it, and so answered
> last-known-good on every call — which means **every session reported
> healthy**. A pool whose seats all report healthy sees no starvation and
> mints no replacement, so routed work sat unclaimed against a lane that
> looked alive. It was invisible until the warnings were made durable.

`gc-runtime-nomad check` now fails outright when the credential cannot list
allocations, precisely so this cannot be discovered the slow way. Run it
before you trust a deployment. The failure names the capability you need:
read access to the job and allocation endpoints in your namespace.

The token is read from the environment of the process running this pack —
**the supervisor's environment**, not your interactive shell's. A token you
can see in your terminal is not a token the runtime has.

If your cluster has ACLs on, the pack expects **two**
tokens, not one: a management token holding `submit-job`, used exactly
once to register the parent job, and a narrowed runtime token used for
every session-path op (dispatch, launch, peek, stop, is-running,
list-running). The runtime token never registers — against a matching
parent, registration is a read-only no-op; against a missing or drifted
one, the op fails with a re-registration error rather than silently
running on the wrong parent.

**What the pack does *not* ask of your cluster:** no host volumes, no
Vault integration, no Consul integration, and no declared workload
identity. Nomad's default workload identity is fail-closed and the
design keeps it that way on purpose.

**Node assumptions, stated so you can check them against your fleet.**

| assumption | detail |
|---|---|
| CPU architecture | `amd64` only, enforced as a hard constraint on `${attr.cpu.arch}`. There is no arm64 path; on an all-arm64 fleet every dispatch stays pending with a constraint failure, and the pack names that rather than waiting silently. |
| task driver | `docker` for the session task. The session agent task IS a docker task and there is no other session shape to fall back to, so a fleet without the docker driver cannot run this pack at all. |
| second driver | `exec` is additionally required when log shipping is enabled. |
| GPUs | **not brokered, and not required.** The pack declares no `device` stanza and asks for no GPU. It has been exercised on a fleet with **no `nomad-device-nvidia` plugin installed**, so a GPU-bearing node is scheduled as an ordinary node and nothing in a session jobspec requests, reserves or fences a GPU. If your agents need a GPU, that is work this pack does not do for you: add the device plugin and the stanza yourself, and note that nothing here will stop two sessions landing on one card. |
| memory | sized by the deployment. An under-sized task is OOM-killed by the kernel, and the pack names OOM as a sizing fault rather than a start fault — but it cannot size it for you. |
| networking | bridge mode. An allocation cannot reach the supervisor's loopback listeners, which is why a reachable city endpoint is a separate requirement rather than an inherited one. |

**Priority.** The parent job registers at priority 40, below a typical
system job. This is a documented starting point for a lab cluster with
preemption off, not an enforcement mechanism. If you run batch
preemption, set your own policy.

---

## Nomad clients

**Linux only, and this is enforced.** The parent jobspec carries a fatal
constraint:

```
${attr.kernel.name} = linux
```

Bridge/CNI networking is Linux-only; a non-Linux placement must fail
closed rather than fall back to host networking. A cluster with no Linux
clients will simply never place a session.

**Docker driver** is required — the session task is a Docker task and
there is no other session shape. **`exec` driver** is required if you
want log shipping; the shipper runs `/bin/sh` under `exec`.

**linux/amd64 only.** This is not a preference; it is enforced in two
places and silently assumed in a third.

- The agent image is amd64 by construction. `toolchain/Dockerfile`
  fetches `gascity_..._linux_amd64.tar.gz`, `toolchain/build-image.sh`
  reads the `binary-amd64` apt index, and that script **hard-refuses** a
  `gc` binary that is not an x86-64 ELF ("a darwin or arm64 gc builds
  green and fails at the first seat command").
- The log-shipper pins
  `vector-0.58.0-x86_64-unknown-linux-gnu.tar.gz` by URL and sha256.
- The jobspec carries **no `cpu.arch` constraint**. That is the silent
  part: on an arm64 client the group still places. The agent is the
  group leader and the shipper is a non-leader poststart sidecar, so if
  the shipper cannot exec its binary nothing tears down and nothing
  complains — **logs just stop shipping.**

`GC_NOMAD_LOG_SHIPPER_ARTIFACT` lets you point the shipper at a
different tarball, but the checksum sent to Nomad's artifact stanza is
always the x86_64 one, so an arm64 override fails the checksum rather
than working. There is no arm64 path today.

**As of the fence below, the silent part is closed.** The parent jobspec
now carries `${attr.cpu.arch} = amd64` as a second fatal rule beside the
kernel one, so an arm64 client fails to place rather than running an
agent whose logs quietly never ship. If you have somehow made an arm64
deployment work, this fence is what you would have to change.

---

## The agent image

You supply the image via `GC_NOMAD_AGENT_IMAGE`. The runtime keeps the
task alive with `/bin/sleep infinity` and then drives everything else
over alloc-exec, so the image must provide:

| binary | why |
|---|---|
| `/bin/sleep` | the task's own keepalive process |
| `/bin/sh` | every exec the runtime issues wraps in it |
| `tmux` | the session is a tmux session; the runtime drives it directly |
| `tar` | workspace and secret staging is a tar stream over exec-stdin, extracted in the box with `tar -x -f - -C <dir>` |
| `ps` | the launch-time agent probe (`ps -eo comm`, falling back to `ps -o comm`) — see below |
| `gc`, `bd` | the seat's own Gas City toolchain, from the same private development lineage as the host `gc` |
| your agent CLI | codex, claude, or whatever the provider resolves to |

**The in-box `gc` must match the pin, not merely be recent.** Everything
in the *Gas City* section above applies to the `gc` inside the container
as much as to the one on the host.

**Two different liveness probes, at two different layers.** They are easy to
confuse and the answer to "is this seat alive?" depends on which one you
mean:

- `is-running` (the RPP op) asks the box `tmux has-session -t main`, and
  `kill -0 <pane pid>` when it has a recorded pid. That answers *does the
  session still exist*.
- The launch-time probe (`boxRunsAnAgent`, runtime/ops.go) reads `ps -eo
  comm` and asks whether any process is running that is not just the
  keepalive, tmux and a shell. That answers *did an agent actually start*.

The second exists because the first says ALIVE for an empty box. Judge a
seat on the process list, never on the pane's foreground command — a pane
body that wraps the command keeps the shell in front while the agent runs
beneath it, so `pane_current_command` reads `sh` on healthy seats too.

**Two traps that build clean and fail later.** Both are enforced by
`toolchain/build-image.sh`, which refuses a staged `gc` rather than
baking it, because both are silent at build time:

- **The VCS stamp.** A `gc` built from a source *archive* rather than a
  checkout has no `.git` for the Makefile's ldflags to read, so it
  stamps its commit as the literal string `unknown`. It builds, runs,
  and ships — and the image can never be traced to a revision, which is
  the whole of what "reproducible" means. The build refuses a commit
  that is not a hex sha.
- **The architecture.** A darwin or arm64 `gc` builds green and dies at
  the first seat command. The build refuses anything that is not an
  x86-64 ELF.

**ABI, not just version.** A `gc` compiled on Debian bookworm (ICU 72)
links cleanly and then dies at runtime inside an Ubuntu noble image (ICU
74). Build the in-box `gc` against the image you will actually run it
in.

**A registry is effectively required, and the pack now says so.**

An arbitrary cluster expects to place an allocation on any eligible node.
An image reference with **no registry host** cannot support that: a bare
name like `gc-agent:3a3dda06a` resolves against Docker Hub, which will not
have a privately built agent image, so in practice it only ever resolves
from a node's **local image cache**. Every session is then pinned to
whichever nodes happen to hold a copy — and that pin is **invisible**. The
jobspec carries no constraint for it, the scheduler places freely, and the
allocation fails to start on any node that was not the build host.

`gc-runtime-nomad check` warns when the configured image names no
registry, and a pull failure on such a reference says outright that no
node can ever pull it, rather than reporting it as "not on this node" —
which understates a condition that will recur on every node, forever.

So: push the agent image to a registry your Nomad clients can reach and
reference it by that name. Pre-loading it on every eligible node also
works, and means redoing that on every image change.

**Private registries: no per-job credential path.** The jobspec ships
**no** Docker `auth` block, deliberately — a task-level `auth` stanza puts
the password in the jobspec, and therefore in Nomad's API and in every
copy of that job. The image has to be pullable by the Nomad client without
job-level credentials: a public registry, a client-level docker config, or
Nomad's own `docker` driver `auth` configuration. If your image must be
private, that credential is a **cluster** configuration step this pack
does not perform and cannot perform for you.

---

## The host that runs `gc`

The `gc-runtime-nomad` executable runs wherever `gc` runs — it is a
client of the Nomad HTTP API and needs no Nomad agent locally. It is
exercised on **macOS (darwin/arm64)** and **Linux**.

**`./check.sh` is the whole suite**, and it runs green on both with no
expected failures and **no skipped tests**: all three modules —
`runtime/`, `fakenomad/` and `drills/` — pass `gofmt`, `go vet` and
`go test`. Verified 2026-09-12 on macOS (darwin/arm64) and on Debian 13
(linux/amd64, tmux 3.5a).

If you see a failure, it is a defect, not a known quirk of your machine.
`check.sh` discovers modules from `*/go.mod` rather than listing them,
and reports skipped tests as well as failures — a suite that quietly
skips its driver tests on a host without `tmux` would otherwise report
success while asserting nothing. `receipt.sh` likewise runs every module
and fails if the tree grows one it does not run.

Note that only the *conformance and unit* suites are portable; they run
offline against this pack's own in-memory Nomad fake (`fakenomad/`),
never against a live cluster.

---

## Building

Go **1.25.9** (the `go` directive in all three modules: `runtime/`,
`fakenomad/`, `drills/`).

`./install.sh` builds and places `gc-runtime-nomad` on `PATH` via the Go
toolchain. `pack.toml` declares the runtime as a bare command name
resolved on `PATH` at session start, so the runtime versions
independently of the `gc` binary.

---

## Known limitations

These are measured, open, and tracked. They are listed here rather than
discovered later.

1. ~~**One agent image per deployment.**~~ **RESOLVED.** This said there
   was no per-lane override, so a codex lane and a claude lane could not
   coexist on one deployment. That is no longer true, and leaving the
   claim here would have been the more expensive error: an adopter would
   have stood up a second deployment they did not need. The pack reads
   `GC_NOMAD_AGENT_IMAGE` from each session's own start payload
   (`sessionAgentImage`), carries it as Nomad meta `gc_image`, and
   interpolates it into the child job's image; empty means inherit the
   deployment default. Set it in a provider's `env` block and that
   role gets its own toolchain while every other lane is untouched. See
   README §"Per-role images".

2. **amd64 only.** No arm64 path exists — the image build, the `gc`
   inside it, and the log-shipper binary are all x86_64. The placement
   now fails closed on a non-amd64 client rather than half-running, but
   failing closed is not support.

3. **Read the green checks narrowly — they cover less than they look
   like they cover.** This is the limitation most likely to mislead, so
   it is spelled out rather than summarized.

   - `CONFORMANCE-RECEIPT.md` is produced against `fakenomad`, this
     pack's own in-memory Nomad API fake. It proves lifecycle protocol
     conformance and nothing about your cluster, your image, or your
     `gc`.
   - **The fake is structurally blind to the jobspec.** It models job
     ID, namespace, node pool, meta, port labels, and a task's command /
     args / env / templates. It models no `ParameterizedJob` or
     `MetaOptional`, no driver, no image, no constraints, no resources,
     no restart policy, no artifact checksum, no task lifecycle, no ACL
     token and no TLS. Every one of those is load-bearing in production.
   - **That is not hypothetical — it has already happened.** The
     `gc_created_at` dispatch key was added without being declared in
     `MetaOptional`. Nomad rejects an undeclared key outright, HTTP 500,
     on *every* provision; zero boxes started. The offline ladder was
     green throughout. The post-mortem is a comment in `jobspec.go`.
   - **The fake runs a box's commands as local subprocesses.** The
     production session task is a Docker task, but the fake reads only
     the command and ignores driver and image — so a conformance run
     executes "in-box" commands on the developer's own machine, where
     `tmux`, `ps`, `sh` and `gc` happen to exist. A green receipt is
     partly a statement about the machine that produced it.
   - **CI is green on a `gc` that cannot run this pack.** The workflow
     pins the *upstream base* of the pin lineage — the commit carrying
     none of the 19 families — because the pin itself does not exist on
     the public repository and cannot be resolved by `go install`. So
     the suite passes against exactly the `gc` this document says makes
     the pack go dark. The workflow states this in its own comments; it
     is repeated here because a green check is what people actually
     read. A release needs an at-pin run against live Nomad, exercising
     parameterized dispatch with `meta_optional`, the Docker driver,
     constraints, the artifact checksum, the poststart sidecar, and both
     ACL tokens.

4. **The image build ships, but the last mile is undocumented.**
   `toolchain/Dockerfile`, `toolchain/TOOLCHAIN.lock` and
   `toolchain/build-image.sh` do give you a checksum-pinned,
   digest-pinned base with `sha256sum --check --strict` on every
   download — that half is real and reproducible. What is *not* written
   down for an outside adopter is the last mile: that base is a **base**,
   city content is composed onto it by `gc build-image`, and the
   resulting tag is what you set as `GC_NOMAD_AGENT_IMAGE`. Nor does
   `build-image.sh` run anywhere but the host that stages the linux pin.
   Tracked in the maintainer's issue list; open.

5. **Credentials reach the agent's process environment.** See
   *Security posture* below — this is the item most likely to decide
   whether you can adopt, and it was undocumented until an outside
   review found it. Tracked with the install work; open.

---

## Security posture

Three things here are deliberate, load-bearing, and were undocumented
until an outside adoption review found them in the source. They are
stated plainly because each one can decide whether this pack is
adoptable in a given environment.

### The agent runs with no sandbox and no approval gate

Every rendered agent config carries:

```toml
approval_policy = "never"
sandbox_mode = "danger-full-access"
```

There is no deployment override. The reasoning is that nobody is
watching an agent inside a dispatched alloc, so an approval prompt is a
hang rather than a control, and the alloc's own container is the
boundary. That reasoning holds **only as far as your container boundary
does.** If your Nomad clients run privileged containers, share a docker
socket, or mount host paths into task containers, an agent in this
runtime has whatever that grants. Decide that deliberately.

Related: the provider base URL is permitted to be plain `http://` and
the rendered config sets `requires_openai_auth = false`, so a
non-TLS provider endpoint is accepted without complaint.

### Certificate verification can be off without you knowing

The pack aliases four TLS settings — `CACERT`, `CLIENT_CERT`,
`CLIENT_KEY`, `TLS_SERVER_NAME` — and deliberately does **not** offer a
`GC_NOMAD_` spelling for skipping verification. But the Nomad SDK's own
`DefaultConfig` reads `NOMAD_SKIP_VERIFY` regardless, and the resulting
config is the one the alloc-exec WebSocket dialer uses — **the channel
every staged secret travels over.**

One stray `NOMAD_SKIP_VERIFY=1` in the supervisor's environment
therefore disabled verification for the API *and* for secret staging.
That is now loud: the runtime prints a warning naming the variable and
the secret channel every time a client is built, and `check` reports it
in the pre-flight. It is still not something the pack can refuse on your
behalf — but it will no longer happen quietly.

### Getting a credential to a seat, if we did not write your secrets file

Three routes exist, and **the name you choose decides which one you get.**
That is the whole decision, and it is not reversible after the fact:

| You name the key | The value travels | It comes to rest as | The agent sees |
|---|---|---|---|
| `FOO_PEM` (any `_PEM` suffix) | tar over exec-stdin | a **file** at `$NOMAD_SECRETS_DIR/FOO_PEM` | `FOO` — a **path**, never the bytes |
| anything else | tar over exec-stdin | a literal `export FOO='…'` in the sourced `env.sh` | `FOO` — the **value**, in its process environment |
| a name on the argv allow-list | the launch command line | nothing staged | the value, and so does anything that can read `/proc/<pid>/cmdline` |

**So: give a credential a `_PEM`-suffixed name and its bytes never enter an
environment.** That is the only form here that keeps a secret out of
`/proc/<pid>/environ` for the agent and every process it spawns. It is
named for PEM material but nothing checks the contents — any credential
you are willing to read from a file can use it.

**The caveat before you pick a name.** Nothing here keeps a secret secret once
it has reached a seat: for the life of that box the value is readable by the
agent and by every process the agent spawns. What crosses into the box is the
session identity, the city target plus its transport credentials, and exactly
the keys you named. What does not cross is this pack's own `GC_NOMAD_TOKEN`,
any key you did not name, and any secret value in a jobspec, in dispatch
metadata, or on the launch argv. That asymmetry is the recommendation: the pack
cannot protect a long-lived secret inside an allocation, so **do not put one
there** — mint a short-lived credential scoped to a single seat or lane at your
issuer and stage that, so a box you cannot audit is holding something that
expires without your involvement. A long-lived human or organization credential
in `GC_NOMAD_EXTRA_SECRET_KEYS` is the shape this section exists to warn
against, and `_PEM` naming alone does not fix it.

Two ways to supply the value in the first place:

- **Your provider's env map**, if `gc` can interpolate it there.
- **`GC_NOMAD_EXTRA_SECRET_KEYS`** — a comma-separated list of names the
  pack bridges out of the *supervisor's own process environment* into the
  session. This exists for values `gc`'s config cannot interpolate. Naming
  a key is what stages it; an unnamed key is never staged. A name you
  declare that the supervisor does not carry is now reported by name at
  session start rather than skipped in silence.

**What the pack will not do with a credential**, by construction rather
than by care:

- It never rides the **launch argv**. Only an explicit allow-list of
  names may — locale, terminal, city/rig/agent identity, session identity
  and derived paths — and anything unlisted is *presumed credential*.
  `GC_INSTANCE_TOKEN` is deliberately excluded even though it looks like
  identity, because it is a capability, and argv is world-readable.
- It never rides Nomad **dispatch metadata**. That carries session
  attribution only.
- It is never written into the **jobspec**.

### Getting the value into the supervisor's environment

Every reader who tried to deliver a credential stopped in the same place, and
one put it exactly: *"the bridge from my own non-conforming secrets file into
the supervisor's own process environment is entirely outside this pack's
documented scope."* True, and "however your deployment composes it" is honest
and useless. The pack cannot own your secrets manager — but it can tell you
what the supervisor's environment has to contain and what the two usual ways of
getting it there cost you.

**What the pack requires, and all it requires:** the process running
`gc supervisor` must have the value in its own environment under the name you
listed in `GC_NOMAD_EXTRA_SECRET_KEYS`. Nothing else. The pack reads that
environment and stages from it; it never reads your secrets file, and there is
no config key pointing at one.

**Route A — a file your process manager reads.** systemd's
`EnvironmentFile=`, or a plist/wrapper that sources a file before exec'ing the
supervisor.

- Works with any secrets manager that can *render a file*, which is most.
- The file is on disk for as long as the supervisor runs. `0600`, owned by the
  supervisor's user, and **not** in a directory your backups sweep.
- Rotation is: rewrite the file, restart the supervisor. That restart is the
  step the rotation runbook below depends on.

**Route B — a wrapper that fetches at exec time.** A small script that asks your
secrets manager and `exec`s the supervisor with the value exported.

- Nothing is written to disk, which is the reason to prefer it.
- The supervisor cannot start when your secrets manager is unreachable. That is
  usually correct — a seat with no credential fails anyway — but it couples
  your session fleet's boot to another service's availability, which is worth
  knowing before a power cut rather than during one.
- Do **not** pass the value on the supervisor's command line. Argv is world
  readable on every OS this runs on, and that is the one mistake in this area
  the pack cannot detect or undo.

**What neither route fixes:** a value in the supervisor's environment is
inherited by everything the supervisor spawns on that host, not only by boxes.
The pack strips its own credential-shaped keys from what it stages into a
session, but the supervisor's own children are not its business. If that
matters to you, the value should reach the supervisor and nothing else — which
argues for Route B and a narrow wrapper.

### Rotating a credential, when one has leaked

Three outside readers were asked to deliver, approve and rotate a credential
from these docs alone. Two could follow a delivery procedure; **none would put
a real credential through it, and rotation was not possible at all** — no
restart command, no bounded fleet operation, no verification, no purge. Their
findings are the reason this section exists, and it is written to be usable
under a clock rather than to be reassuring.

**First, say which credential you mean.** The docs previously used one word for
two things, and a reader had to infer the difference:

| | What it is | How it reaches a box | Rotating it |
|---|---|---|---|
| `GC_NOMAD_TOKEN` | the **Nomad ACL token** this pack uses to talk to the cluster | never leaves the orchestrator — it authenticates the API calls | mint a new one, update the supervisor's env, cycle the supervisor. Running boxes are unaffected: they never had it |
| a **staged provider secret** (`GC_NOMAD_EXTRA_SECRET_KEYS`, provider env) | a value the AGENT needs | staged into the box at start/provision — as a file for a `_PEM` name, as `env.sh` otherwise | needs every affected box **replaced**; see below |

**The fact that decides the whole procedure:** a staged secret is materialised
at **start/provision** time and never again. `relaunch` re-execs inside the
SAME allocation with no fresh dispatch and no fresh staging — the pack's own
comment says so — and `gc reload` cannot revoke an environment the supervisor
has already composed. **Neither rotates a live seat.** Only replacing the box
does.

**The procedure, and where it stops being ours.**

1. **Rotate at the source** (your secrets manager) and update whatever composes
   the supervisor's process environment. *This pack does not own that step and
   cannot tell you how to do it* — it reads the supervisor's env and nothing
   else. If you have no documented way to change that env and restart the
   supervisor, you have no rotation story, and that is worth fixing before you
   need one rather than during.
2. **Cycle the supervisor** so new sessions compose the new value. **This pack
   ships no command for that and never will** — the supervisor is started by
   *your* process manager (launchd, systemd, a container orchestrator), and the
   pack has no handle on it. Two docs assert a cycle is required; neither could
   tell you how, which is the gap this paragraph closes by naming it as yours.
3. **Replace every live box.** There is no bulk operation — `stop` is
   per-session by design — so the loop is explicit:

   ```sh
   gc-runtime-nomad list-running | while read -r s; do gc-runtime-nomad stop "$s"; done
   ```

   Each replacement stages the new value at provision. Budget for it: this is
   one dispatch per seat, and a large pool will not finish in ten minutes.

   **What happens to work a seat was mid-way through: the pack does not release
   it.** `stop` egresses the session's evidence, deregisters its child job,
   confirms it terminal and tombstones the binding — and touches the bead claim
   **not at all**. A seat stopped while holding claimed work leaves that claim
   exactly as `gc` left it, and recovering it is `gc`'s dead-assignee handling,
   not this pack's. Plan the rotation around that: you are trading in-flight
   work for a rotated credential, deliberately, and the pack will not soften
   the trade for you.

   **Check the exit code of that loop.** `list-running` exits **1 on any lookup
   error** rather than printing a partial list, so a transport fault makes the
   loop above iterate over nothing, complete instantly, and look like it
   rotated a fleet that it never touched. During a leak response that is the
   worst failure available:

   ```sh
   names=$(gc-runtime-nomad list-running) || { echo "list-running failed — NOTHING was rotated" >&2; exit 1; }
   printf '%s\n' "$names" | while read -r s; do [ -n "$s" ] && gc-runtime-nomad stop "$s"; done
   ```

**What you can and cannot verify.** Be exact about this, because the readers
were right that the docs implied more than they delivered:

- **`GC_NOMAD_TOKEN`: verifiable.** A bare HTTP status against the Nomad API
  with the OLD token proves it is dead — `403` means revoked. Never print the
  token to check it.
- **A staged provider secret: NOT verifiable by this pack.** It has no read-back
  path for a staged value by design, and it must not acquire one. Verification
  belongs where the credential is *honoured* — revoke at the issuer and confirm
  there. If your issuer cannot tell you whether a key is still in use, that is
  the gap, and it is not one the pack can close.

**The copy you will forget.** If the agent ever printed its environment, the
value may already sit in plaintext in your **log sink** and your
`GC_NOMAD_EGRESS_DIR`. The pack scrubs what IT composes; it cannot scrub what
an agent chose to print. Rotation is not complete until those are purged, and
the pack cannot do it for you — it does not own either store. Treat both as
in-scope for any leak response.

### Where credentials actually end up

The staging channel itself is value-blind and does not log secret
values. What the documentation never said is where the values come to
rest:

- **Only `_PEM`-suffixed keys get the pointer treatment.** For those,
  the environment script points at a staged file and never repeats the
  bytes. **Every other key becomes a literal `export KEY='value'`** in
  the environment script the pane sources. So an API key or an auth JSON
  blob lives in the agent's process environment — and therefore in
  `/proc/<pid>/environ` for the agent and every process it spawns.
- **The agent's stdout is shipped off-box.** When `GC_NOMAD_LOG_SINK` is
  set, the log-shipper tails `logs/agent.stdout` — the raw terminal tap
  — and posts it to that sink; `GC_NOMAD_EGRESS_DIR` gets a copy on
  stop. **Neither path scrubs.** If an agent ever prints its own
  environment, that output is durably persisted locally and POSTed to an
  external endpoint.

Those two facts compose. Treat the log sink and the egress directory as
credential-sensitive storage, scope the credentials you stage as
narrowly as you can, and prefer the `_PEM` pointer form wherever the
credential shape allows it.

## Box checkout of the deployment repository

A box's session working directory is scratch by default (see
`env.workspace` above) — no `.git`, nothing the deployment's own repo-bound
formulas can read or write. `GC_NOMAD_CITY_MIRROR_URL` closes the READ half
of that gap; this section documents the full boundary: what crosses, what
does not, and what to do instead of writing.

### READ — cloned at prime, behind an operator knob

Set `GC_NOMAD_CITY_MIRROR_URL` (optionally `GC_NOMAD_CITY_MIRROR_BRANCH`) to
a read-only git URL — `git://`, or any transport your box's image can reach
without a credential. `paneCwdPrelude` clones (or, on an adopted/relaunched
box that already holds a prior clone, fetches and hard-resets) it into the
session working directory before the agent launches. The resulting HEAD sha
is read back into the sidecar binding as `city_checkout_sha`.

**What crosses:** the URL and branch name, exactly as `workDir` already
crosses today — plain runtime config, not a credential. **What does not
cross:** anything. `git://` is anonymous and read-only by protocol; nothing
in this pack stages a token, key, or `.netrc` for it. If your mirror needs
authentication, this knob is the wrong tool — it has no credential path and
will not grow one; put an authenticating proxy in front of a URL this box
can reach unauthenticated instead.

**Image-baked content as a cache.** `gc build-image` can bake repository
content into the agent image at build time (see *The agent image* above).
That is compatible with this knob: image content is what a fresh box's
working directory starts with, and the prime-time fetch (the `git fetch` +
hard-reset branch of `cityMirrorPrelude`, taken because the directory is
already a git checkout) refreshes it in place. Treat the image as the cache
and the knob as what keeps the cache from going stale, not as two competing
mechanisms.

**Rollback:** unset `GC_NOMAD_CITY_MIRROR_URL`. The pane's cwd logic reverts
to exactly its pre-knob behavior — `cityMirrorPrelude` returns `""` and is
never invoked with a non-empty body.

**Failure mode:** soft, deliberately. An unreachable mirror, a shallow-fetch
error, or a working directory that is neither empty nor already a git
checkout logs to the pane's own stderr capture and the launch proceeds on
whatever directory it already had — never fails the launch. `is-running`'s
best-effort marker sync (`syncCityCheckoutMarker`) costs nothing when the
knob is unset: no exec call at all.

### WRITE — no repository credential ever enters a box

This pack implements no write path for the deployment repository, and does
not intend to grow one behind this knob or any other: `git://` carries no
credential to stage, and nothing here escalates it into one. A deployment
that needs a box's work to reach the repository has two conventions
available today, neither of which this pack mediates:

- **A public upstream repository:** through whatever seat/provider token
  your `gc` config already stages into the box's environment (e.g. a GitHub
  token staged as a provider env value) — branch push and PR/MR, the normal
  flow for that token, entirely outside this pack's surface.
- **The deployment's own (private) repository:** as a patch (`git
  format-patch` or equivalent) the box's session records as its output —
  session description, metadata, or a receipt endpoint your deployment
  defines — for a host-side lane with real write credentials to apply and
  commit. This pack does not implement that apply step; it is deployment
  convention layered on top of whatever channel your `gc` formulas already
  use to carry a session's output back out (comments, metadata, artifacts).

### BRIDGE / typed close — not implemented in this pack

A box session that ends its atom under a remote `gc` contract may be unable
to land a typed work-record close (outcome/commit/branch) if that surface
is not available to a remote/box-side caller in your `gc` version. This pack
does not implement a bridge for that today: a box in that position should
end its atom with whatever typed hand-off *is* available to it (its own
description/metadata), and a host-side step should perform the actual
close. If your `gc` gains a remote work-record write surface, wiring this
pack to it is future work, not something the checkout knob above implies.

See [`docs/typed-close-bridge.md`](docs/typed-close-bridge.md) for the
current, pin-dependent split of what the remote worker API does and does not
serve today (metadata + close works, `--outcome` does not, escalation
creation does not, and the remote close handler's ADR-0009 ancestry-gate
parity with the local close path) — that file is the one to re-check after a
pin roll, not this paragraph.

## Reporting a mismatch

Include all four of these. The first two are the ones that actually
disambiguate, and the ones most bug reports omit:

1. `gc version --json` — the **commit**, not the version.
2. `GC_NOMAD_AGENT_IMAGE`, and the output of `gc version --json` from
   **inside** that image.
3. `nomad version`, and `${attr.kernel.name}` / `${attr.cpu.arch}` of the
   client that placed the alloc.
4. `gc-runtime-nomad check` — it reports the environment warnings that
   turn into silent degradation.
