# toolchain/Dockerfile — the session toolchain base (the image gc build-image composes city content onto)
#
# Provider-neutral by construction: this file never mentions Nomad, an
# allocation, a host volume, a host artifact path, or a provider binary path
# other than in-image paths. It is the Docker build INPUT — building it is a
# build-host step (toolchain/build-image.sh), and nothing here publishes,
# promotes, or canaries anything.
#
# Every ARG default below is a value read from toolchain/TOOLCHAIN.lock, which
# is the checksum pin of record. The build FAILS LOUD if a pin is wrong:
# `sha256sum --check --strict` on the downloaded archives and the two identity
# assertions on bd are the same discipline upstream uses
# (contrib/k8s/Dockerfile.base:70-75, Dockerfile.agent:39-52).

# Build ARGs so downloads resolve against the pinned registry, not a mirror.
ARG BASE_IMAGE=ubuntu:24.04@sha256:c4a8d5503dfb2a3eb8ab5f807da5bc69a85730fb49b5cfca2330194ebcc41c7b
ARG BUILD_IMAGE=golang:1.26.6-bookworm@sha256:433f9dc4f8ea3a1ce4e28f9f15d0f7c056b10475307f886d6f1ac1ccc4abd976

# --- bd: rebuilt from checksum-pinned release source -------------------------
# The published beads binaries are NOT shippable: beads_1.2.2_linux_amd64
# reports `dep google.golang.org/grpc v1.80.0` in its own Go build info
# (measured 2026-09-09), the version upstream rebuilds to escape. This stage is
# a faithful mirror of upstream's own recipe, contrib/k8s/Dockerfile.agent:22-52
# — same source ref, same published source digest, same ldflags, same two
# post-build assertions.
FROM ${BUILD_IMAGE} AS bd-builder

ARG BD_SOURCE_URL=https://github.com/gastownhall/beads/archive/bf97b73749ac3ef2fca2365b54537ac041ad4293.tar.gz
ARG BD_SOURCE_SHA256=a8b1d8dd85b2c008093615cb85937067a9597e760e8d39f93fe55f5c1cbb4d37
ARG BD_VERSION=1.1.0
ARG BD_SOURCE_REF=bf97b73749ac3ef2fca2365b54537ac041ad4293
ARG BD_BUILD=bf97b73749
ARG BD_BRANCH=HEAD
ARG GRPC_VERSION=1.82.1

RUN mkdir -p /src/bd /out \
    && curl -fsSL --retry 3 "${BD_SOURCE_URL}" -o /tmp/bd-source.tar.gz \
    && echo "${BD_SOURCE_SHA256}  /tmp/bd-source.tar.gz" | sha256sum --check --strict \
    && tar -xzf /tmp/bd-source.tar.gz --strip-components=1 -C /src/bd \
    && rm -f /tmp/bd-source.tar.gz

WORKDIR /src/bd
RUN bd_version="${BD_VERSION#v}" \
    && grep -Fq "Version = \"${bd_version}\"" cmd/bd/version.go \
    && go get "google.golang.org/grpc@v${GRPC_VERSION}" \
    && CGO_ENABLED=1 go build \
       -tags="gms_pure_go" \
       -trimpath \
       -ldflags="-s -w -X main.Version=${bd_version} -X main.Build=${BD_BUILD} -X main.Commit=${BD_SOURCE_REF} -X main.Branch=${BD_BRANCH}" \
       -o /out/bd \
       ./cmd/bd \
    && /out/bd --version | grep -Fq "bd version ${bd_version} (${BD_BUILD})" \
    && go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep google.golang.org/grpc v${GRPC_VERSION} "

# --- the toolchain image ----------------------------------------------------
FROM ${BASE_IMAGE}

ARG GC_URL=https://github.com/gastownhall/gascity/releases/download/v1.4.1/gascity_1.4.1_linux_amd64.tar.gz
ARG GC_SHA256=8d8c8b511db3fc44931445aab5cb9f212509c0867105c880d6c3d0e6e5d33e42
# Which gc goes into the image. The published release above is the PUBLIC
# DEFAULT; when GC_LOCAL_ARTIFACT names a file under .local/ (staged by
# toolchain/build-image.sh from the lock's gc_local_artifact), that binary — the
# deployment's LIVE pin — is installed instead and its own reported commit is
# asserted, because a gc older than the pin is the defect this closes.
# build-image.sh passes empty values for all three to mean "published".
ARG GC_LOCAL_ARTIFACT=
ARG GC_LOCAL_SHA256=
ARG GC_PIN_COMMIT=
ARG GIT_VERSION=1:2.43.0-1ubuntu7.3
ARG TMUX_VERSION=3.4-1ubuntu0.1
# libicu74 is not decoration: a locally built gc NEEDS it. `ldd` on the
# digest-pinned base reports `libicui18n.so.74 => not found` for the staged pin,
# so the install succeeded and the binary then died at its first command
# (`gc: error while loading shared libraries`). A build host normally has ICU
# installed, which is exactly why the host cannot show this gap and only an
# image build can. Measured and pinned; the assertion is at the foot of the gc RUN.
ARG LIBICU_VERSION=74.2-1ubuntu3.1
ARG CODEX_URL=https://github.com/openai/codex/releases/download/rust-v0.150.1/codex-package-x86_64-unknown-linux-musl.tar.gz
ARG CODEX_SHA256=00aba704f029f6dc0d948be407a756e0c97cc840132fd691353b2c6b0a505b17
ARG CODEX_ENTRYPOINT_SHA256=abf1bb1643a79f73aa78ee627e111e02d4f8c98f25813a0cf6ce277709664386
ARG CODEX_VERSION=0.150.1

ENV DEBIAN_FRONTEND=noninteractive

# apt's signed Release index is the integrity gate here, so the versions are
# pinned to what toolchain/TOOLCHAIN.lock records (and their index SHA256s are
# on the record for off-box audit). Package list = upstream's own base list
# (contrib/k8s/Dockerfile.base:109-123): gc/bd need flock (util-linux) and lsof
# at runtime, the launcher needs sh/tar, tmux is the session driver's verb.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       bash \
       ca-certificates \
       curl \
       libicu74=${LIBICU_VERSION} \
       git=${GIT_VERSION} \
       jq \
       lsof \
       procps \
       python3 \
       sudo \
       tini \
       tmux=${TMUX_VERSION} \
       util-linux \
       tar \
    && rm -rf /var/lib/apt/lists/* \
    && git --version \
    && tmux -V \
    && command -v sh tar flock lsof jq tini

# The build context's staging dir. `.local/` is minted by build-image.sh (it
# always contains a PLACEHOLDER, since COPY rejects an empty directory) and holds
# the pinned local gc under `.local/gc` when one is selected. It is removed in
# the same RUN that consumes it, so the artifact adds no layer.
COPY .local/ /tmp/toolchain-local/

# gc — either the deployment's LIVE pin (checksum-verified, commit-asserted) or,
# with no local artifact named, the published release archive. Both paths are
# checksum-verified and identity-asserted; the local path asserts the commit the
# pin REPORTS about itself, which is the assertion that would otherwise rot:
# gc as shipped has no way to say which binary belongs in a base image
# (`gc build-image --help` composes city content onto ARG BASE and names no gc).
RUN set -eu; \
    cd /; \
    if [ -n "${GC_LOCAL_ARTIFACT}" ]; then \
      src="/tmp/toolchain-local/${GC_LOCAL_ARTIFACT}"; \
      test -f "$src"; \
      echo "${GC_LOCAL_SHA256}  ${src}" | sha256sum --check --strict; \
      install -m 0755 "$src" /usr/local/bin/gc; \
      expected="${GC_PIN_COMMIT}"; \
      test -n "$expected" || { echo "gc local artifact named with no GC_PIN_COMMIT" >&2; exit 1; }; \
    else \
      curl -fsSL --retry 3 "${GC_URL}" -o /tmp/gc.tar.gz; \
      echo "${GC_SHA256}  /tmp/gc.tar.gz" | sha256sum --check --strict; \
      tar -xzf /tmp/gc.tar.gz -C /tmp gc; \
      install -m 0755 /tmp/gc /usr/local/bin/gc; \
      rm -f /tmp/gc /tmp/gc.tar.gz; \
      expected=""; \
    fi; \
    rm -rf /tmp/toolchain-local; \
    missing="$(ldd /usr/local/bin/gc | awk '/not found/ {printf "%s ", $1}')"; \
    test -z "${missing}" || { echo "gc has unresolved shared libraries in this image: ${missing}" >&2; echo "add the providing package to the apt RUN above; measured with: docker run --rm -v <staged gc>:/gc:ro <base_image> ldd /gc" >&2; exit 1; }; \
    reported="$(gc version --json --long)"; \
    echo "gc in image: ${reported}"; \
    if [ -n "${expected}" ]; then \
      reported_commit="$(printf '%s' "${reported}" | jq -r '.commit // empty')"; \
      test "${reported_commit}" = "${expected}" || { echo "gc identity assertion FAILED: the image reports commit ${reported_commit}, the pin is ${expected}" >&2; exit 1; }; \
      echo "gc identity assertion PASS: the image gc is ${reported_commit}"; \
    fi; \
    gc version

# bd — rebuilt above from the checksum-pinned release source.
COPY --from=bd-builder /out/bd /usr/local/bin/bd
RUN BD_DISABLE_METRICS=1 BD_DISABLE_EVENT_FLUSH=1 bd --version

# Codex — the COMPLETE standalone package (NOT the bare codex-<target> archive,
# which is what made a dogfood seat come up as a chat window that cannot run a
# tool). Layout is asserted from the package's own codex-package.json, the
# entrypoint is checksum-asserted, and the PATH dir mirrors the shape the
# launcher already expects: a `current` symlink to a versioned directory.
# `install -d /opt/codex/bin` is load-bearing, found the hard way: `ln -sfn`
# does not create the parent, so the very last step of the unpack failed
# (`ln: failed to create symbolic link '/opt/codex/bin/current': No such file or
# directory`) — the file the versions dir was created from a path that existed
# and the link target's parent never was. The commit that added this Dockerfile
# recorded honestly that it had never been built; that is why the first build to
# run it found this, and it is not a regression from any later change.
RUN set -eu; \
    curl -fsSL --retry 3 "${CODEX_URL}" -o /tmp/codex-package.tar.gz; \
    echo "${CODEX_SHA256}  /tmp/codex-package.tar.gz" | sha256sum --check --strict; \
    mkdir -p /tmp/codex-unpacked; \
    tar -xzf /tmp/codex-package.tar.gz -C /tmp/codex-unpacked; \
    pkg="$(dirname "$(find /tmp/codex-unpacked -name codex-package.json -print -quit)")"; \
    test -n "$pkg"; \
    install -d "/opt/codex/versions/${CODEX_VERSION}"; \
    cp -a "$pkg/." "/opt/codex/versions/${CODEX_VERSION}/"; \
    rm -rf /tmp/codex-unpacked /tmp/codex-package.tar.gz; \
    cd "/opt/codex/versions/${CODEX_VERSION}"; \
    ln -sfn bin/codex codex; \
    ln -sfn bin/codex-code-mode-host codex-code-mode-host; \
    echo "${CODEX_ENTRYPOINT_SHA256}  bin/codex" | (cd "/opt/codex/versions/${CODEX_VERSION}" && sha256sum --check --strict); \
    test -x bin/codex-code-mode-host; \
    test -d codex-resources; \
    test -d codex-path; \
    cat codex-package.json; \
    install -d /opt/codex/bin; \
    ln -sfn "/opt/codex/versions/${CODEX_VERSION}" /opt/codex/bin/current

# The PATH dir a seat needs to run a tool. The launch seam owns what a session's
# PATH is; this is the in-image default, and the build
# proves it with the README's own check rather than asserting it.
ENV PATH="/opt/codex/bin/current:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
RUN env -i PATH="/opt/codex/bin/current:/usr/bin:/bin" sh -c 'command -v codex && command -v codex-code-mode-host'

# No secrets, credentials, city content, or runtime state are baked here —
# gc build-image excludes them from the context it composes on top of this
# base (internal/buildimage/context.go:39-63) and the pack stages secrets
# value-blind at runtime (README part 1 step 9).
# The gc binary itself may be a locally built artifact: a build-time copy of the
# staged pin, verified by checksum and by the commit it reports. That path is a
# BUILD input only — the image still mounts nothing, and no allocation writes
# the pin-staging directory.
