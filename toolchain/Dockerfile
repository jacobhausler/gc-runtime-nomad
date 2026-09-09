# toolchain/Dockerfile — estate session toolchain (the base gc build-image composes city content onto)
#
# cr-u7b0s.45. Provider-neutral by construction: this file never mentions
# Nomad, an allocation, a host volume, /mnt/nomad, or a provider binary path
# other than in-image paths. It is the Docker build INPUT — building it is an
# executive host step (toolchain/build-image.sh), and nothing here publishes,
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
ARG GIT_VERSION=1:2.43.0-1ubuntu7.3
ARG TMUX_VERSION=3.4-1ubuntu0.1
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

# gc — the published release archive, checksum-verified, identity-asserted.
RUN curl -fsSL --retry 3 "${GC_URL}" -o /tmp/gc.tar.gz \
    && echo "${GC_SHA256}  /tmp/gc.tar.gz" | sha256sum --check --strict \
    && tar -xzf /tmp/gc.tar.gz -C /tmp gc \
    && install -m 0755 /tmp/gc /usr/local/bin/gc \
    && rm -f /tmp/gc /tmp/gc.tar.gz \
    && gc version

# bd — rebuilt above from the checksum-pinned release source.
COPY --from=bd-builder /out/bd /usr/local/bin/bd
RUN BD_DISABLE_METRICS=1 BD_DISABLE_EVENT_FLUSH=1 bd --version

# Codex — the COMPLETE standalone package (NOT the bare codex-<target> archive,
# which is what made a D4 seat come up as a chat window that cannot run a
# tool). Layout is asserted from the package's own codex-package.json, the
# entrypoint is checksum-asserted, and the PATH dir mirrors the shape the
# launcher already expects: a `current` symlink to a versioned directory.
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
    ln -sfn "/opt/codex/versions/${CODEX_VERSION}" /opt/codex/bin/current

# The PATH dir a seat needs to run a tool. The launcher seam (cr-u7b0s.41/.42)
# owns what a session's PATH is; this is the in-image default, and the build
# proves it with the README's own check rather than asserting it.
ENV PATH="/opt/codex/bin/current:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
RUN env -i PATH="/opt/codex/bin/current:/usr/bin:/bin" sh -c 'command -v codex && command -v codex-code-mode-host'

# No secrets, credentials, city content, or runtime state are baked here —
# gc build-image excludes them from the context it composes on top of this
# base (internal/buildimage/context.go:39-63) and this estate stages secrets
# value-blind at runtime (README part 1 step 9).
