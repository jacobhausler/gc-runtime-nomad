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
| `toolchain/local-pin-test.sh` | the offline fixture for the live-pin path — preference, every refusal, the bounded-city gate, and every Dockerfile RUN parsed by the image's own shell family. 42 assertions as counted by that command's own `ok` lines; `bash toolchain/local-pin-test.sh` |

Nothing here publishes, pushes, promotes, canaries, pins, or touches a live
service. Publishing stays an executive host step and allocations never write
`/mnt/nomad/gc/bin` — that is the ruling on cr-gdeav.2.13 (escalation cr-on8x4),
which `GC_TOOLCHAIN_HOST_STEP=1` encodes. Until someone exports
`GC_NOMAD_AGENT_IMAGE`, the exec + read-only artifact-volume shape is what runs
(the volume mount is suppressed only when that variable is set), so this commit
is reversible with `git revert <sha>` and needs no estate action.

> **Superseded 2026-09-12.** The two sentences above describe a fallback that
> no longer exists. v1.0.7 (`cr-3vvnb5.2`) deleted the legacy `exec` task and
> the `GC_NOMAD_ARTIFACT_VOLUME` mount: the Docker image is now the only
> session shape, `GC_NOMAD_AGENT_IMAGE` is **required**, and unsetting it
> fails every lifecycle op closed rather than falling back. Rollback is a
> previous image tag, not a shape to drop into. The measurements below stand
> as recorded; only this reversibility claim is retired.


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

Rollback is a previous IMAGE TAG: `export GC_NOMAD_AGENT_IMAGE=gc-session-toolchain:rollback`
(the tag `build` re-points at whatever was live), or any earlier tag recorded in
the deployment receipt. There is no other shape to fall back to — unsetting the
variable fails every lifecycle op closed, because the Docker task is the only
session shape (cr-3vvnb5.2) and the legacy `exec` + read-only `/mnt/nomad`
artifact-volume shape it used to return to is deleted (cr-3vvnb5). The pin
rollback itself remains the executive host step it was ruled to be (repin
#5–#7 receipts).
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

  → Superseded by cr-dk9i2 below: it has since been built on rhuidean, twice
  failing and once succeeding, and both failures were real defects this receipt
  could not have seen. Read the cr-dk9i2 section as the current word on the
  Dockerfile; this bullet is kept as the record of what was and was not proven
  at the time.
- No pin, image, launchd unit, city service, supervisor, or live job was
  touched, and no live publish was invoked.

## The live pin in the image — cr-dk9i2 (2026-09-09T14:2x–14:5xZ, mechanic-3)

The section at the foot of this file stayed open because there was nothing
published to lock: the estate's live gc pin is a locally built binary with no
upstream checksum. cr-dk9i2 closes it with a **local artifact** entry instead of
a published-release pretence — and the alternative it replaces (shipping
published gc while the estate runs a newer pin) is the defect, not a detail.

| | |
|---|---|
| live pin | `e52f96f62` — `gc version --json --long` → `{"commit":"e52f96f62","version":"dev"}`, identical on the Mac and on the staged binary |
| staged artifact | `rhuidean:/mnt/nomad/gc/bin/e52f96f62/gc` — ELF 64-bit x86-64, `/mnt/nomad/gc/bin/current` → `e52f96f62` |
| sha256 | `0f3b9e3a3e115ba95096ce9ea01e08d4f97b397243ddca0ec259de6e0c2b25a7`, measured on the host that stages it |
| lock | `gc_local_artifact <path> sha256=<sha> pin=<commit>`; the published `gc 1.4.1` line is untouched and stays the PUBLIC default (`build --published` builds exactly it) |
| pack commit | `8e798e674ce417355a2893c37deb4d017e4959a4` (`main`) |

The native gc forms checked before any of this was added to the pack:
`gc --help` lists no pin/artifact/install command and `gc version --long` only
REPORTS the binary already running; `gc build-image --help`
(`--base-image`/`--tag`/`--context-only`/`--push`/`--json`/`--rig-path`) composes
CITY content onto `ARG BASE`, names no gc binary, and copies the city whole with
only secrets excluded (`internal/buildimage/context.go`). gc as shipped can
therefore neither carry a locally built pin nor refuse an estate-root context,
so the pack holds both gates and gc is used exactly as shipped. A third solution
— "delete the local layer" — is unavailable: with no local path the image is
older than the pin by construction.

### Measured

```sh
bash toolchain/local-pin-test.sh     # 31 assertions, offline → local-pin-test: PASS
bash toolchain/build-image.sh check  # → gc LOCAL … pin=e52f96f62 (and honestly "absent on this host" on a Mac)
bash toolchain/build-image.sh verify # → all five publisher checks still PASS (gc/bd/codex/git/tmux)

# refresh-pin's happy path runs only where the pin is staged (a Mac's own live
# pin is a darwin build; the lane refuses a non-x86-64 file rather than pin it):
ssh rhuidean … 'cd ~/cr-dk9i2 && bash toolchain/build-image.sh refresh-pin --artifact=/mnt/nomad/gc/bin/e52f96f62/gc'
   # → refresh-pin: … sha256=0f3b9e3a… pin=e52f96f62
   # diff vs the COMMITTED lock → IDENTICAL: a repin is one command and idempotent

# the build itself — and the THREE defects only a build could show. All three are
# at HEAD, each with an assertion that keeps it fixed:
#   1. gc RUN → `/bin/sh: 1: Syntax error: Unterminated quoted string`: cr-dk9i2's
#      own identity assertion was not parseable → 78d5942, plus the offline
#      RUN-parse gate with a bite-canary
#   2. gc RUN → `gc: error while loading shared libraries: libicui18n.so.74`
#      after the checksum passed: the estate-built gc links ICU, ubuntu:24.04
#      does not ship it, and rhuidean does — a gap the host structurally cannot
#      show → libicu74 pinned as a sixth component + an ldd gate, 2fd96d8
#   3. Codex RUN → `ln: failed to create symbolic link '/opt/codex/bin/current':
#      No such file or directory` at the last line: `ln -sfn` does not create a
#      parent. Pre-existing at cr-u7b0s.45, whose own receipt recorded that this
#      Dockerfile had never been built — first build, first finding → 4cf46af

GC_TOOLCHAIN_HOST_STEP=1 bash toolchain/build-image.sh build --tag=gc-agent:1.0.6-e52f96f62
   # the pin reached docker: --build-arg=GC_LOCAL_ARTIFACT=gc
   # --build-arg=GC_LOCAL_SHA256=0f3b9e3a… --build-arg=GC_PIN_COMMIT=e52f96f62
   # context = a minted /tmp/gc-toolchain-ctx.XXXXXX, never the pack working tree
   # → 35/35 steps green, "Successfully tagged gc-agent:1.0.6-e52f96f62"
   # image id  sha256:98c2e5f8d68ffc30bb4e7dd3b6d80c1d40ae67cd9c90d4fe23864d56876ce5d3
   #          (docker image inspect -f '{{.Id}}', 2026-09-09T15:25:03Z, rhuidean)
   # image size 498508363 bytes; RepoTags=[gc-agent:1.0.6-e52f96f62]
   # build receipt line: built agent=gc-agent:1.0.6-e52f96f62
   #   id=sha256:98c2e5f8…gc_source=local gc_pin=e52f96f62 rollback=gc-session-toolchain:rollback
   # in-image, from the built image itself:
   #   gc version --json --long → {"commit":"e52f96f62","version":"dev"}   ← the bead
   #   ldd /usr/local/bin/gc | grep -c "not found" → 0
   #   bd --version → bd version 1.1.0 (bf97b73749)
   #   git version 2.43.0 · tmux 3.4
   #   env -i PATH=/opt/codex/bin/current:/usr/bin:/bin sh -c 'command -v codex;
#     command -v codex-code-mode-host' → both resolve (the D4 worked example)
```

The fixture tag asserts its own identity from the inside: the Dockerfile
checksum-verifies the staged bytes and then fails the build unless
`gc version --json --long` inside the image reports `"commit":"e52f96f62"`.

**One line for the pin-roll lane (cr-u7b0s.51) and for any future image:** an
estate-built gc links `libicui18n.so.74`. Any image that ships a locally built gc
must carry `libicu74`, or it will build green and die at the first seat command.
Nothing else in the estate bakes a gc into an image today (the exec + artifact-
volume shape runs on hosts that have ICU), so this is a forward warning, not a
list of live exposures.

Refused, and proved refused by the fixture: a named-but-missing artifact, a
checksum that moved, an unchecksummed `gc_local_artifact` line, no local line at
all (that is the silent-fallback hole), and `--city=<estate root>` for
`city-image`/`context-only` — the last is the failure cr-5wz9v stopped at, and a
bounded city (city.toml, outside the checkout; `ops/tests/test-city-kit` mints
one under `/tmp`) is accepted.

## Left open for the pin-roll lane (surfaced, not decided here)

**Answered by cr-dk9i2, the next section down.** The choice it framed — move to a
published release, or rebuild from source and stamp the measured digest — went to
the second branch, and the stamp is `gc_local_artifact` with the live pin.

`gc` in the image is the **published v1.4.1** release. The estate's current gc
pin is EDGE `857ed398ad22de3d84b229a0f9b6557d694f4b5a`, which is **not published
upstream** (`gh api repos/gastownhall/gascity/commits/857ed398…` → 422 "No commit
found"), so it has no upstream-published checksum to lock. The pin-roll lane
(cr-u7b0s.51) must either move the image to a published release (this lock, as
written) or rebuild from the EDGE source and stamp `gc <ver> sha256:<measured
binary digest>` here in the same change. Shipping an unstamped `gc` binary is
exactly the gate-0 finding this atom exists to close, so the lock, not the
Dockerfile, stays the source of truth.
