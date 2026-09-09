#!/usr/bin/env bash
# toolchain/build-image.sh — the executive host step for the session toolchain image
#
# cr-u7b0s.45. Validates and re-verifies the checksum pins in
# toolchain/TOOLCHAIN.lock, builds the toolchain image from
# toolchain/Dockerfile, composes city content onto it with gc's own native
# verb, and holds the rollback tag. It NEVER pushes and NEVER runs in an
# allocation.
#
# Why this file exists (the native gc form checked, per the city gate):
# `gc build-image --help` — --context-only / --base-image (default
# gc-agent:latest) / --tag (required unless --context-only) / --push / --json,
# plus pinned internal/buildimage/{context.go,dockerfile.go}. gc build-image
# assembles CITY CONTENT onto `ARG BASE` and excludes secrets; it does not
# build or checksum this estate's provider toolchain (contrib/k8s carries no
# Codex at all) and `prebaked`/`image` have K8s-only readers. So gc is used
# exactly as shipped for the half it does, and the pack holds provenance +
# rollback for the half gc has no command for. `--push` is deliberately not
# offered: publishing the runtime registry is a reserved owner decision
# (AGENTS.md hard stop). The GC_TOOLCHAIN_HOST_STEP gate below encodes the
# ruling on cr-gdeav.2.13 (cr-on8x4): allocations never publish the toolchain.
set -euo pipefail

PACK_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCK="${PACK_ROOT}/toolchain/TOOLCHAIN.lock"
DOCKERFILE="${PACK_ROOT}/toolchain/Dockerfile"
APT_INDEX="https://archive.ubuntu.com/ubuntu/dists/noble-updates/main/binary-amd64/Packages.gz"

die() { echo "toolchain: $*" >&2; exit 1; }

lock_get() { awk -v k="$1" '$1==k {print $2; exit}' "$LOCK"; }

lock_version() { awk -v n="$1" '$1==n {print $2; exit}' "$LOCK"; }

# key=value pairs on a component line (url=…, artifact=…, source_ref=…, …)
lock_attr() {
  awk -v n="$1" -v a="$2" '$1==n {
    for (i=3; i<=NF; i++) if ($i ~ "^"a"=") { sub("^"a"=", "", $i); print $i; exit }
  }' "$LOCK"
}

# the bare `sha256:<digest>` pin on a component line
lock_sha() {
  awk -v n="$1" '$1==n {
    for (i=3; i<=NF; i++) if ($i ~ /^sha256:[0-9a-f]{64}$/) { sub("^sha256:", "", $i); print $i; exit }
  }' "$LOCK"
}

require_lock() {
  [ -f "$LOCK" ] || die "missing ${LOCK}"
  [ -f "$DOCKERFILE" ] || die "missing ${DOCKERFILE}"
  for c in gc bd git tmux codex; do
    grep -qE "^${c}[[:space:]].*sha256:[0-9a-f]{64}" "$LOCK" || die "component ${c} is missing or unpinned in ${LOCK}"
    [ -n "$(lock_sha "$c")" ] || die "component ${c} has no parseable sha256 pin"
  done
  grep -qE '^base_image[[:space:]].*@sha256:[0-9a-f]{64}' "$LOCK" || die "base_image is not digest-pinned in ${LOCK}"
  grep -qE '^codex[[:space:]].*codex-package\.json' "$LOCK" || die "the Codex standalone layout is not asserted in ${LOCK}"
  grep -qE '^rollback_image[[:space:]]' "$LOCK" || die "rollback_image is not named in ${LOCK}"
}

args_from_lock() {
  printf '%s\n' \
    "--build-arg=BASE_IMAGE=$(lock_get base_image)" \
    "--build-arg=BUILD_IMAGE=$(lock_get build_image)" \
    "--build-arg=GC_URL=$(lock_attr gc url)" \
    "--build-arg=GC_SHA256=$(lock_sha gc)" \
    "--build-arg=GIT_VERSION=$(lock_version git)" \
    "--build-arg=TMUX_VERSION=$(lock_version tmux)" \
    "--build-arg=BD_SOURCE_URL=$(lock_attr bd url)" \
    "--build-arg=BD_SOURCE_SHA256=$(lock_sha bd)" \
    "--build-arg=BD_VERSION=$(lock_version bd)" \
    "--build-arg=BD_SOURCE_REF=$(lock_attr bd source_ref)" \
    "--build-arg=GRPC_VERSION=$(lock_attr bd rebuilt_grpc)" \
    "--build-arg=CODEX_URL=$(lock_attr codex url)" \
    "--build-arg=CODEX_SHA256=$(lock_sha codex)" \
    "--build-arg=CODEX_ENTRYPOINT_SHA256=$(lock_attr codex entrypoint_sha256)" \
    "--build-arg=CODEX_VERSION=$(lock_version codex)"
}

apt_field() { # apt_field <index-file> <package> <field-with-colon> — one value from the signed Release index
  awk -v p="$2" -v f="$3" '
    /^Package: / { inpkg = ($2 == p) }
    inpkg && $1 == f { v = substr($0, length($1) + 2); exit }
    END { if (v != "") print v }
  ' "$1"
}

cmd="${1:-help}"; shift || true
DRY=0; RECEIPT=""; CITY=""; OUT_TAG=""
for a in "$@"; do
  case "$a" in
    --dry-run) DRY=1 ;;
    --receipt=*) RECEIPT="${a#--receipt=}" ;;
    --city=*) CITY="${a#--city=}" ;;
    --tag=*) OUT_TAG="${a#--tag=}" ;;
    *) die "unknown argument: $a" ;;
  esac
done

run() { if [ "$DRY" = 1 ]; then echo "+ $*"; else "$@"; fi; }

case "$cmd" in
  check)
    require_lock
    echo "toolchain lock: base       $(lock_get base_image)"
    echo "toolchain lock: build      $(lock_get build_image)"
    echo "toolchain lock: agent      $(lock_get agent_image)   rollback $(lock_get rollback_image)"
    for c in gc bd git tmux codex; do
      printf 'toolchain lock: %-9s %s sha256:%s\n' "$c" "$(lock_version "$c")" "$(lock_sha "$c")"
    done
    echo "toolchain lock: five components sha256-pinned, base digest-pinned, Codex standalone layout asserted, rollback named"
    ;;

  verify) # re-check every pin against its publisher (read-only, needs network)
    require_lock
    pv="$(lock_version gc)"
    pub="$(curl -fsSL --max-time 120 "https://github.com/gastownhall/gascity/releases/download/v${pv}/gascity_${pv}_checksums.txt" | awk -v a="$(lock_attr gc artifact)" '$2==a {print $1; exit}')"
    [ "$pub" = "$(lock_sha gc)" ] || die "gc published checksum moved: published=${pub:-none} locked=$(lock_sha gc)"
    echo "verify gc ${pv}: published checksum matches the lock"

    curl -fsSL --max-time 300 "$(lock_attr bd url)" -o /tmp/gc-toolchain-bd-src.tgz || die "bd source fetch failed"
    bd_now="$(shasum -a 256 -b /tmp/gc-toolchain-bd-src.tgz | awk '{print $1}')"
    rm -f /tmp/gc-toolchain-bd-src.tgz
    [ "$bd_now" = "$(lock_sha bd)" ] || die "bd release-source checksum moved: fetched=${bd_now} locked=$(lock_sha bd)"
    echo "verify bd $(lock_version bd): release-source checksum matches the lock (published binary is deliberately NOT used — it carries grpc 1.80.0)"

    cv="$(lock_version codex)"
    pub="$(curl -fsSL --max-time 120 "https://github.com/openai/codex/releases/download/rust-v${cv}/codex-package_SHA256SUMS" | awk -v a="$(lock_attr codex artifact)" '$2==a {print $1; exit}')"
    [ "$pub" = "$(lock_sha codex)" ] || die "codex published checksum moved: published=${pub:-none} locked=$(lock_sha codex)"
    echo "verify codex ${cv}: release SHA256SUMS matches the lock"

    index="$(mktemp)"; curl -fsSL --max-time 300 "$APT_INDEX" | gzip -dc > "$index" || { rm -f "$index"; die "apt Release index fetch failed"; }
    for c in git tmux; do
      idx_ver="$(apt_field "$index" "$c" "Version:")"
      idx_sha="$(apt_field "$index" "$c" "SHA256:")"
      [ "${idx_ver:-}" = "$(lock_version "$c")" ] || die "${c} Release index no longer offers $(lock_version "$c") (index says ${idx_ver:-absent})"
      [ "${idx_sha:-}" = "$(lock_sha "$c")" ] || die "${c} Release-index checksum moved: index=${idx_sha:-none} locked=$(lock_sha "$c")"
      echo "verify ${c} ${idx_ver}: signed Release index SHA256 matches the lock"
    done
    rm -f "$index"
    ;;

  build) # the host step an allocation must never run
    [ "${GC_TOOLCHAIN_HOST_STEP:-}" = 1 ] || die "refusing to build: publishing the toolchain is an executive host step (ruling cr-on8x4 on cr-gdeav.2.13). Re-run with GC_TOOLCHAIN_HOST_STEP=1 on the build host."
    require_lock
    command -v docker >/dev/null || die "docker not found on this host"
    agent="$(lock_get agent_image)"; rollback="$(lock_get rollback_image)"
    if [ "$DRY" = 0 ] && docker image inspect "$agent" >/dev/null 2>&1; then
      prev_id="$(docker image inspect -f '{{.Id}}' "$agent")"
      run docker tag "$agent" "$rollback"
      echo "toolchain rollback: ${agent} -> ${rollback} (${prev_id})"
    fi
    # shellcheck disable=SC2046
    run docker build -f "$DOCKERFILE" -t "$agent" $(args_from_lock) "${PACK_ROOT}/toolchain"
    id="$(docker image inspect -f '{{.Id}}' "$agent" 2>/dev/null || echo dry-run)"
    [ "$DRY" = 1 ] || echo "toolchain built: ${agent} id=${id}"
    echo "next:  export GC_NOMAD_AGENT_IMAGE=${agent}    # the ONE image input the pack reads (cr-u7b0s.41)"
    echo "back:  export GC_NOMAD_AGENT_IMAGE=${rollback} # or unset it -> legacy exec + read-only artifact volume"
    [ -z "$RECEIPT" ] || printf 'built agent=%s id=%s rollback=%s at=%s\n' "$agent" "$id" "$rollback" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$RECEIPT"
    ;;

  city-image) # gc's native composition step, on top of the toolchain base
    require_lock
    [ -n "$CITY" ] || die "--city=<city-path> required (gc build-image copies the whole city directory — use a bounded city, never an estate root)"
    agent="${OUT_TAG:-$(lock_get agent_image)}"
    run gc build-image "$CITY" --base-image "$(lock_get agent_image)" --tag "$agent" --json
    [ "$DRY" = 1 ] || echo "toolchain city-image: ${agent} carries city content; secrets stay runtime-staged (buildimage/context.go excludes them)"
    ;;

  context-only) # PROVES CONTEXT ASSEMBLY ONLY — not a runnable image
    require_lock
    [ -n "$CITY" ] || die "--city=<city-path> required"
    run gc build-image "$CITY" --base-image "$(lock_get agent_image)" --context-only --json
    [ "$DRY" = 1 ] || echo "toolchain context-only: context assembly proven; a runnable image is NOT proven by this command"
    ;;

  *)
    [ "$cmd" = help ] || die "unknown command: $cmd"
    cat <<'USAGE'
usage: toolchain/build-image.sh <check|verify|build|city-image|context-only> [--dry-run]
       [--city=<city-path>] [--tag=<image:tag>] [--receipt=<path>]

  check         validate toolchain/TOOLCHAIN.lock (offline; the release gate)
  verify        re-check every pin against its publisher (gc/bd/codex releases,
                Ubuntu Release index) — read-only, needs network
  build         docker build the toolchain image, rollback-tagging whatever is
                live first (executive host step: needs GC_TOOLCHAIN_HOST_STEP=1,
                never pushed)
  city-image    gc build-image --base-image <toolchain> --tag <agent_image>
  context-only  gc build-image --context-only (assembly proof only)
USAGE
    ;;
esac
