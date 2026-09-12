# Compatibility

What this pack needs to run, stated plainly, including where it does not
work yet. Every version and requirement below was measured against a
running system on the date given, not read off a changelog.

Read this before `install.sh`. The single most important line is the
first one under **Gas City (`gc`)**: today this pack does **not** run on a
released upstream `gc`.

---

## Gas City (`gc`) — READ THIS FIRST

**Required: a `gc` built from the Westlands EDGE lineage. No released
upstream `gc` can run this pack.**

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
`origin/main`. The retraction is recorded rather than deleted because a
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

| feature | used for |
|---|---|
| parameterized batch job + dispatch | one parent job `gc-sessions`; every session is a dispatched child |
| dispatch `meta_optional` | `gc_session`, `gc_nonce`, `gc_created_at` — Nomad rejects any key not declared here with HTTP 500, so an undeclared key fails **every** provision |
| alloc-exec over WebSocket | `GET /v1/client/allocation/:alloc_id/exec` — this is how every command reaches the session |
| task `lifecycle` poststart sidecar | the log-shipper task |
| artifact stanza with `checksum` | fetching the pinned Vector binary |
| `X-Nomad-Index` on write responses | the pack tolerates a proxy that strips it, but native Nomad supplies it |
| namespaces | `GC_NOMAD_NAMESPACE`, defaulting to `default` (`newClient`, runtime/client.go) — the pack creates no namespace, so if you set one it must already exist |

**ACLs.** A token is optional (`GC_NOMAD_TOKEN`, sent as
`X-Nomad-Token`). If your cluster has ACLs on, the pack expects **two**
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
| `gc`, `bd` | the seat's own Gas City toolchain, from the same EDGE lineage as the host `gc` |
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

**Registry access.** The jobspec ships **no** Docker `auth` block. The
image has to be pullable by the Nomad client with no job-level
credentials — a public registry, a client-level docker config, or a
pre-pulled image. There is no per-job registry credential path today.

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

1. **One agent image per deployment.** `GC_NOMAD_AGENT_IMAGE` is read
   once at startup and baked into the *parent* job spec, which every
   dispatched child inherits. There is no per-lane, per-provider or
   per-agent override, so **a codex lane and a claude lane cannot
   coexist on one deployment of this runtime.** Tracked as `cr-420zkj`.

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
   Tracked as `cr-2dh2w9`.

5. **Credentials reach the agent's process environment.** See
   *Security posture* below — this is the item most likely to decide
   whether you can adopt, and it was undocumented until an outside
   review found it. Tracked with the install work, `cr-pbqh1d`.

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
