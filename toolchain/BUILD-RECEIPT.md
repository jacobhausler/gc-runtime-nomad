# Session toolchain packaging — build, deployment, and rollback receipt

Atom: cr-u7b0s.45 (Remediate G: package the box toolchain as a checksum-pinned
prebaked image) · written by `mechanic-3` 2026-09-09T03:5xZ · predecessor facts
receipt: `ops/receipts/nrt-gate0/remediation/2026-09-08-G-facts.md` (city
checkout), verdict `ops/receipts/nrt-gate0/2026-09-08-G-verdict.md`.

## What lands here

| file | role |
|---|---|
| `toolchain/TOOLCHAIN.lock` | the build input: base image **by digest**, the four binary components + complete Codex package each with its **upstream-published sha256** and the layout it asserts, the ONE `agent_image` reference, and the `rollback_image` tag |
| `toolchain/Dockerfile` | the provider-neutral Docker build input — digest-pinned base, version-pinned apt `git`/`tmux`, checksum-verified published `gc`, `bd` rebuilt from checksum-pinned release source with a patched grpc, the complete Codex standalone package with its layout asserted, no host volumes, no `/mnt/nomad`, no provider names |
| `toolchain/build-image.sh` | `check` (offline gate) · `verify` (re-check every pin against its publisher) · `build` (executive host step: rollback-tag then `docker build`, never `--push`) · `city-image` / `context-only` (gc's native verb on top of the toolchain base) |

Nothing here publishes, pushes, promotes, canaries, pins, or touches a live
service. Publishing stays an executive host step and allocations never write
`/mnt/nomad/gc/bin` — that is the ruling on cr-gdeav.2.13 (escalation cr-on8x4),
which `GC_TOOLCHAIN_HOST_STEP=1` encodes. Until someone exports
`GC_NOMAD_AGENT_IMAGE`, the exec + read-only artifact-volume shape is what runs
(the volume mount is suppressed only when that variable is set), so this commit
is reversible with `git revert <sha>` and needs no estate action.

## The pins, and how each was verified (2026-09-09T03:4xZ, by this atom)

| component | version | sha256 | publisher / verification |
|---|---|---|---|
| `gc` | 1.4.1 | `8d8c8b51…5d33e42` | `gascity_1.4.1_checksums.txt`; `verify` re-fetched it and it still matches. The extracted binary's own Go build info carries `main.version=v1.4.1`, `main.commit=58ef17e3bd685fd5cf7f21286277b208d3324590`, `grpc v1.82.1` |
| `bd` | 1.1.0 (rebuilt) | `a8b1d8dd…cbb4d37` | the **release source archive** at `bf97b737…` — the same digest pair upstream itself verifies (`contrib/k8s/Dockerfile.agent:25-26,:33`); `verify` re-downloaded it and it matches |
| `git` | `1:2.43.0-1ubuntu7.3` | `099bb129…bf78f87` | Ubuntu `noble-updates/main` signed Release index; `verify` re-reads the index and compares version **and** SHA256 |
| `tmux` | `3.4-1ubuntu0.1` | `2913e17a…d5338d` | same index (tmux is in `main`, not `universe`) |
| `codex` | 0.150.1 | `00aba704…a505b17` | the release's own `codex-package_SHA256SUMS`; entrypoint `bin/codex` identity `abf1bb16…664386`; layout asserted from `codex-package.json` (`layoutVersion 1`, `entrypoint bin/codex`, `resourcesDir codex-resources`, `pathDir codex-path`) |

Two findings that decided the pins, both measured here rather than inherited:

1. **Do not ship the published `bd` binary.** `beads_1.2.2_linux_amd64.tar.gz`
   (published checksum `8140098a…`, verified) reports
   `dep google.golang.org/grpc v1.80.0` in its own Go build info — the exact
   version upstream rebuilds release source to escape. Pinning the *published
   binary* would have been the lazy, wrong pin; the lock pins the release
   **source** digest and mirrors upstream's rebuild recipe and its two
   post-build assertions verbatim.
2. **The pin must be the *complete* Codex standalone package.** `README.md`
   §"Worked example — Codex" records the D4 failure (bare `codex-<target>`
   archive ⇒ a seat that runs but cannot run a tool). The Dockerfile therefore
   asserts `codex-package.json`, `bin/codex` by checksum,
   `bin/codex-code-mode-host`, `codex-resources/`, `codex-path/`, and then
   re-runs the README's own check inside the image:
   `env -i PATH=/opt/codex/bin/current:/usr/bin:/bin sh -c 'command -v codex && command -v codex-code-mode-host'`.

`sha256sum --check --strict` is the assertion form throughout, matching
upstream's own practice (`contrib/k8s/Dockerfile.base:70-75`,
`Dockerfile.agent:33`).

## The seam this coordinates with (and does not rename)

`GC_NOMAD_AGENT_IMAGE` is the pack's ONE image input, rendered as the session
task's `config.image` (cr-u7b0s.41 owns that seam). `agent_image` in the lock is
the value the build prints for it; this atom renames nothing and adds no second
image knob, no `[session.k8s] image`/`prebaked` key, and no host-volume fallback.

## Deployment and rollback (executive host step — not run by this atom)

```sh
cd work/gc-runtime-nomad
bash toolchain/build-image.sh check                      # offline gate
bash toolchain/build-image.sh verify                     # pins vs. their publishers
GC_TOOLCHAIN_HOST_STEP=1 bash toolchain/build-image.sh build \
    --receipt=ops/receipts/nrt-g/<date>-toolchain.md     # rollback-tags, then builds
gc build-image <bounded-city> --base-image gc-session-toolchain:1.0.5 \
    --tag gc-session-toolchain:1.0.5-city --json         # gc's own verb, city content on top
export GC_NOMAD_AGENT_IMAGE=gc-session-toolchain:1.0.5-city   # then a bounded canary, separately gated
```

Rollback, in order of preference: `export GC_NOMAD_AGENT_IMAGE=gc-session-toolchain:rollback`
(the tag `build` re-points at whatever was live), or **unset** it, which returns
the legacy `exec` + read-only `/mnt/nomad` artifact-volume shape; the pin rollback
itself remains the executive host step it was ruled to be (repin #5–#7 receipts).
No registry push in any direction: `--push` is not offered here — publishing the
runtime registry is a reserved owner decision (AGENTS.md hard stop), and the
admitted estate precedent for a locally built image is `force_pull = false`
(`ops/nomad/jobs/obs-stack.nomad.hcl:620-632`, "Locally built, never pushed").

## What is proven, and what is NOT

Proven by this atom, with commands and output:

```sh
bash toolchain/build-image.sh check            # five components sha256-pinned, base digest-pinned, layout asserted, rollback named → PASS
bash toolchain/build-image.sh verify           # gc/bd/codex publisher checksums + git/tmux Release-index version AND SHA256 → all five PASS
bash -n toolchain/build-image.sh install.sh    # → PASS
gc build-image /tmp/gc-fixture --base-image <ref> --context-only --json
   # → {"ok":true,"context_dir":"…","base_image":"<ref>"} and the emitted
   #   context Dockerfile is `ARG BASE=<ref>` + COPY workspace/ — the base
   #   reference passes through verbatim (measured with a marker tag)
```

NOT proven here — and the receipt that reads this must not confuse the two:

- `--context-only` proves **context assembly**, not a runnable image.
- `toolchain/Dockerfile` has **not been built**. There is no docker daemon on
  this Mac (`docker info` → `failed to connect to the docker API at
  unix:///var/run/docker.sock`) and a build is a heavy Go/toolchain operation;
  the disposable docker-driver canary is explicitly out of this atom's scope
  (1.0.x, per the facts receipt). First `build` on a host with a daemon is the
  real test of the rebuild and the Codex unpack steps.
- No pin, image, launchd unit, city service, supervisor, or live job was
  touched, and no live publish was invoked.

## Left open for the pin-roll lane (surfaced, not decided here)

`gc` in the image is the **published v1.4.1** release. The estate's current gc
pin is EDGE `857ed398ad22de3d84b229a0f9b6557d694f4b5a`, which is **not published
upstream** (`gh api repos/gastownhall/gascity/commits/857ed398…` → 422 "No commit
found"), so it has no upstream-published checksum to lock. The pin-roll lane
(cr-u7b0s.51) must either move the image to a published release (this lock, as
written) or rebuild from the EDGE source and stamp `gc <ver> sha256:<measured
binary digest>` here in the same change. Shipping an unstamped `gc` binary is
exactly the gate-0 finding this atom exists to close, so the lock, not the
Dockerfile, stays the source of truth.
