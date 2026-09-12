# Clean-machine build receipt

Gate 6 asks whether the image is *reproducible*, against the charge that it is
"one hand-built tag on one node". Asserting that is worthless; this is the
measurement, taken 2026-09-12.

## What was proven

An adopter with **no staged `gc`**, on a host that had **never built this
image**, produced a working agent image from the shipped lock with two commands.

Host: a Nomad client with docker 29.1.3 (overlayfs) that had **zero**
`gc-session-toolchain` / `gc-agent` images before this run — verified by
`docker images | grep -c` returning 0, not assumed from its role.

```
toolchain/build-image.sh check                          # offline, no network
GC_TOOLCHAIN_HOST_STEP=1 \
  toolchain/build-image.sh build --gc-release=75fbc2cf5b
```

## What each step actually reported

1. **`check` passed offline** and named the adopter's true state rather than
   papering over it: *"no `gc_local_artifact` — a build would ship published gc
   1.4.1, which may be older than the live pin"*. Six components sha256-pinned,
   base digest-pinned.
2. **The published `gc` was fetched AND verified**, not trusted:

   ```
   toolchain gc source: PUBLISHED PIN 75fbc2cf5b fetched from
   https://github.com/jacobhausler/gascity/releases/download
   (sha256 0b9fd8ca1161…, verified against its sidecar)
   ```
3. **The image build re-checked it** rather than relying on the fetch:
   `sha256sum --check --strict` on the staged binary, then an identity
   assertion that the gc in the image reports the expected commit, then
   `ldd /usr/local/bin/gc` refusing any unresolved library.
4. **The result runs.** In the built image:

   ```
   {"commit":"75fbc2cf5b","long":true,"ok":true,…}
   ldd $(command -v gc) | grep -c "not found"   ->  0
   ```

## What this does NOT prove

- **Not bit-identical.** Docker builds carry timestamps and buildkit
  attestation manifests, so two hosts produce different image IDs from the same
  lock. What is reproducible is the CONTENT: the same digest-pinned base and the
  same six sha256-pinned components, asserted by the build itself. Anyone
  claiming bit-reproducibility here should produce two matching IDs first.
- **One host, one architecture.** linux/amd64 only, which the pack states as a
  hard stop. This says nothing about arm64, because there is no arm64 path.
- **Not the `gc build-image` layer.** This is the toolchain base. Composing city
  content onto it is a separate step and its own gap.

## Why the ICU trap did not fire here

The fetched `gc` was built against `ubuntu:24.04` — the image's own base — so
`ldd` found nothing unresolved. A binary built anywhere else loads cleanly on
its build host and dies in the box; measured the same day, a Debian trixie
build gave `libicui18n.so.76: cannot open shared object file` while the image
has `.so.74`. That is why step 3 exists and why it is a refusal rather than a
warning.
