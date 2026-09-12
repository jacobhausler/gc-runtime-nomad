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
its own. The pack depends on ~19 behaviour families that live in a local
lineage and ride a *city pin*. Twelve of them have an open upstream PR;
**seven have no upstream PR at all.** The full table — family, upstream
PR, live merge state, what breaks without it, and the first pin that
carried it — is `docs/PIN-DEPENDENCIES.md`. That file is the authority;
this section is the summary.

The failure mode when the dependency is missing is the reason this
section is first: **the pack does not refuse, it goes dark.** Two
measured examples:

- Without the `runtime-provider` config seam, `gc` silently drops
  `runtime_provider` and `[[agent_groups]]` while parsing the city
  config. The pack's own `[runtimes.nomad]` declaration is discarded,
  the runtime is simply never selected, and the lane looks configured
  and serves nothing.
- Without `remote-worker-lifecycle`, a seat inside an alloc cannot claim
  at all. It starts, connects, and drains with
  `{"action":"drain","reason":"no_work"}` while the city lists its work
  as ready.

Neither prints an error naming this pack. Budget for that when you are
deciding whether to adopt.

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
| namespaces | default namespace is `gc`, override with `GC_NOMAD_NAMESPACE` |

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
than working. There is no arm64 path today. Give the pack amd64 clients,
or add a `cpu.arch` constraint of your own so a wrong-arch placement
fails loudly instead of running half-dark.

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
| `ps` | the liveness probe (`ps -eo comm`, falling back to `ps -o comm`) — this is how the runtime tells a working seat from an empty box |
| `gc`, `bd` | the seat's own Gas City toolchain, from the same EDGE lineage as the host `gc` |
| your agent CLI | codex, claude, or whatever the provider resolves to |

**The in-box `gc` must match the pin, not merely be recent.** Everything
in the *Gas City* section above applies to the `gc` inside the container
as much as to the one on the host.

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

The test suite runs on both. Note that only the *conformance and unit*
suites are portable; they run offline against this pack's own in-memory
Nomad fake (`fakenomad/`), never against a live cluster.

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

2. **amd64 only, and the arch mismatch is silent** (see *Nomad
   clients*). The image build refuses a wrong-arch `gc` loudly; the
   *placement* does not, so an arm64 client yields a running agent with
   no log shipping and no error.

3. **The conformance receipt is offline.** `CONFORMANCE-RECEIPT.md` is
   produced against `fakenomad`, this pack's own in-memory Nomad API
   fake. It proves lifecycle protocol conformance. It does **not** prove
   anything about your cluster, your image, or your `gc`.

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

5. **No credential story for the in-box agent yet.** How an agent inside
   an alloc obtains its provider credentials is deployment-specific and
   undocumented here. Tracked with the install work, `cr-pbqh1d`.

---

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
