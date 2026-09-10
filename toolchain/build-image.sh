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
#
# cr-dk9i2 (1.0.6) adds three things. Each names the native gc form checked
# first, per the mechanic charter:
#
# 1. LOCAL ARTIFACT. The lock's `gc` component is the upstream-PUBLISHED release
#    (1.4.1, commit 58ef17e3b). The estate's live pin is NEWER — measured
#    2026-09-09T14:2xZ, `gc version --json --long` -> {"commit":"e52f96f62",
#    "version":"dev"} — so an image assembled only from published release bytes
#    ships a gc older than the pin it exists to carry. Native forms checked:
#    `gc --help` lists no pin/artifact/install command and `gc version --long`
#    only REPORTS the binary already running; `gc build-image --help` has no
#    flag naming which gc binary goes into the base, it composes city content
#    onto `ARG BASE`. gc as shipped therefore cannot carry a locally built pin,
#    so the lock records `gc_local_artifact` and gc stays as shipped. `build`
#    prefers it; `--published` opts back to the public default deliberately; a
#    pinned-but-missing or mismatched binary is a hard refusal, never a silent
#    fallback to the older gc.
# 2. REFRESH-PIN. The nightly-pin-roll order moves the ESTATE pin and knows
#    nothing about this file, and no gc command rewrites a pack lock. One command
#    here rewrites the two local-artifact entries from the binary actually
#    staged on the build host.
# 3. BOUNDED CONTEXT. gc build-image copies the whole city directory and excludes
#    only secrets (internal/buildimage/context.go), so `--city=/Users/home/westlands`
#    clears gc's own gate and would ship the estate. `require_bounded_city` is
#    the refusal gc exposes no flag for; ops/tests/test-city-kit mints a city
#    that passes it (mints under /tmp, private GC_HOME).
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

file_sha256() { # portable: the builders ship sha256sum, a Mac ships shasum
  if command -v sha256sum >/dev/null 2>&1; then sha256sum -b "$1" | awk '{print $1}'
  else shasum -a 256 -b "$1" | awk '{print $1}'
  fi
}

# --- the live-pin local artifact (cr-dk9i2) ----------------------------------
# An empty path means "no local artifact pinned" — and `build` then refuses
# rather than quietly shipping the older published gc.
local_artifact_path() {
  if [ -n "${GC_LOCAL_ARTIFACT:-}" ]; then printf '%s\n' "$GC_LOCAL_ARTIFACT"
  else lock_get gc_local_artifact
  fi
}
local_artifact_sha() { lock_attr gc_local_artifact sha256; }
local_artifact_pin() { lock_attr gc_local_artifact pin; }

require_lock() {
  [ -f "$LOCK" ] || die "missing ${LOCK}"
  [ -f "$DOCKERFILE" ] || die "missing ${DOCKERFILE}"
  for c in gc bd git tmux codex libicu74; do
    grep -qE "^${c}[[:space:]].*sha256:[0-9a-f]{64}" "$LOCK" || die "component ${c} is missing or unpinned in ${LOCK}"
    [ -n "$(lock_sha "$c")" ] || die "component ${c} has no parseable sha256 pin"
  done
  grep -qE '^base_image[[:space:]].*@sha256:[0-9a-f]{64}' "$LOCK" || die "base_image is not digest-pinned in ${LOCK}"
  grep -qE '^codex[[:space:]].*codex-package\.json' "$LOCK" || die "the Codex standalone layout is not asserted in ${LOCK}"
  grep -qE '^rollback_image[[:space:]]' "$LOCK" || die "rollback_image is not named in ${LOCK}"
  # The local artifact is optional (the published gc is the public default), but
  # when it is named it is a checksum pin like every other line here — an
  # unchecksummed local binary is exactly the hole cr-dk9i2 is closing.
  if [ -n "$(lock_get gc_local_artifact)" ]; then
    [ -n "$(local_artifact_sha)" ] || die "gc_local_artifact is named but carries no sha256=<hex> pin in ${LOCK}"
    [ -n "$(local_artifact_pin)" ] || die "gc_local_artifact is named but carries no pin=<gc commit> in ${LOCK}"
  fi
}

# The estate checkout, derived rather than hardcoded: the pack lives at
# <estate>/work/gc-runtime-nomad, so one level up answers which checkout owns it.
estate_root() { git -C "${PACK_ROOT}/.." rev-parse --show-toplevel 2>/dev/null || true; }

# gc build-image copies the city whole (secrets excepted); nothing in gc refuses
# an estate root, so the refusal lives here. It only ever refuses MORE than gc
# does — never fewer cities — which is the safe direction for a gate.
require_bounded_city() {
  [ -n "$CITY" ] || die "--city=<city-path> required: gc build-image copies the WHOLE city directory. Pass a bounded city — ops/tests/test-city-kit/mk-test-city.sh mints one under /tmp."
  [ -d "$CITY" ] || die "no such city directory: $CITY"
  local_abs="$(cd "$CITY" && pwd -P)"
  if [ ! -f "${local_abs}/city.toml" ]; then
    die "refusing ${local_abs}: no city.toml, so this is not a city root. gc build-image copies whatever it is given — pass a bounded city root."
  fi
  root="$(estate_root)"
  if [ -n "$root" ]; then
    case "$local_abs" in
      "$root"|"$root"/*)
        die "refusing ${local_abs}: it is the estate checkout (or lives inside it). gc build-image excludes only secrets, so this would copy the whole estate — cleanroom, ops, backups, worktrees — into a session image. Pass a bounded city: ops/tests/test-city-kit/mk-test-city.sh mints one under /tmp."
        ;;
    esac
  fi
  if [ -e "${local_abs}/_cleanroom" ]; then
    die "refusing ${local_abs}: it carries estate material (_cleanroom). Pass a bounded city instead."
  fi
  CITY="$local_abs"
}

args_from_lock() {
  printf '%s\n' \
    "--build-arg=BASE_IMAGE=$(lock_get base_image)" \
    "--build-arg=BUILD_IMAGE=$(lock_get build_image)" \
    "--build-arg=GC_URL=$(lock_attr gc url)" \
    "--build-arg=GC_SHA256=$(lock_sha gc)" \
    "--build-arg=GIT_VERSION=$(lock_version git)" \
    "--build-arg=TMUX_VERSION=$(lock_version tmux)" \
    "--build-arg=LIBICU_VERSION=$(lock_version libicu74)" \
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
DRY=0; RECEIPT=""; CITY=""; OUT_TAG=""; ARTIFACT=""; PUBLISHED=0
for a in "$@"; do
  case "$a" in
    --dry-run) DRY=1 ;;
    --receipt=*) RECEIPT="${a#--receipt=}" ;;
    --city=*) CITY="${a#--city=}" ;;
    --tag=*) OUT_TAG="${a#--tag=}" ;;
    --artifact=*) ARTIFACT="${a#--artifact=}" ;;
    --published) PUBLISHED=1 ;;
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
    for c in gc bd git tmux codex libicu74; do
      printf 'toolchain lock: %-9s %s sha256:%s\n' "$c" "$(lock_version "$c")" "$(lock_sha "$c")"
    done
    if [ -n "$(lock_get gc_local_artifact)" ]; then
      printf 'toolchain lock: gc LOCAL  %s sha256=%s pin=%s  (build prefers this; published gc %s stays the public default)\n' \
        "$(lock_get gc_local_artifact)" "$(local_artifact_sha)" "$(local_artifact_pin)" "$(lock_version gc)"
      if [ -f "$(lock_get gc_local_artifact)" ]; then
        printf 'toolchain lock: gc LOCAL present on this host, on-disk sha256=%s\n' "$(file_sha256 "$(lock_get gc_local_artifact)")"
      else
        printf 'toolchain lock: gc LOCAL absent on this host — build must run where %s is staged (or pass --published)\n' "$(lock_get gc_local_artifact)"
      fi
    else
      echo "toolchain lock: no gc_local_artifact — a build would ship published gc $(lock_version gc), which may be older than the live pin; refresh-pin to carry the pin"
    fi
    echo "toolchain lock: six components sha256-pinned (the local gc shared-library dep included), base digest-pinned, Codex standalone layout asserted, rollback named"
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
    for c in git tmux libicu74; do
      idx_ver="$(apt_field "$index" "$c" "Version:")"
      idx_sha="$(apt_field "$index" "$c" "SHA256:")"
      [ "${idx_ver:-}" = "$(lock_version "$c")" ] || die "${c} Release index no longer offers $(lock_version "$c") (index says ${idx_ver:-absent})"
      [ "${idx_sha:-}" = "$(lock_sha "$c")" ] || die "${c} Release-index checksum moved: index=${idx_sha:-none} locked=$(lock_sha "$c")"
      echo "verify ${c} ${idx_ver}: signed Release index SHA256 matches the lock"
    done
    rm -f "$index"

    # The local pin is asserted against its own publisher: the binary ITSELF.
    # Upstream publishes nothing for a dev pin, so the gate is the staged file's
    # own checksum plus the commit it reports.
    if [ -n "$(lock_get gc_local_artifact)" ] && [ -f "$(lock_get gc_local_artifact)" ]; then
      now="$(file_sha256 "$(lock_get gc_local_artifact)")"
      [ "$now" = "$(local_artifact_sha)" ] || die "gc_local_artifact moved: on-disk=${now} locked=$(local_artifact_sha)"
      echo "verify gc_local_artifact: staged binary matches the lock; commit $(local_artifact_pin)"
    fi
    ;;

  refresh-pin) # rewrite the two local-artifact lock entries from the staged pin (cr-dk9i2)
    [ -f "$LOCK" ] || die "missing ${LOCK}"
    art="${ARTIFACT:-${GC_LOCAL_ARTIFACT:-}}"
    [ -n "$art" ] || die "usage: toolchain/build-image.sh refresh-pin --artifact=<path-to-linux-gc>   (e.g. --artifact=/mnt/nomad/gc/bin/<pin>/gc on the build host that stages it)"
    [ -f "$art" ] || die "no gc binary at ${art}. This runs on the host that STAGES the linux pin (rhuidean: /mnt/nomad/gc/bin/<pin>/gc); a Mac's live pin is a darwin build and cannot ship in a linux/amd64 image."
    kind="$(file -b "$art" 2>/dev/null || echo unknown)"
    case "$kind" in
      *"ELF 64-bit"*x86-64*) : ;;
      *) die "refusing to pin ${art}: not an x86-64 ELF (file says: ${kind}). The image is linux/amd64 — a darwin or arm64 gc builds green and fails at the first seat command." ;;
    esac
    long="$("$art" version --json --long 2>/dev/null || true)"
    pin="$(printf '%s' "$long" | { command -v jq >/dev/null 2>&1 && jq -r '.commit // empty' || sed -n 's/.*"commit":"\([^\"]*\)".*/\1/p'; } 2>/dev/null || true)"
    [ -n "$pin" ] || die "could not read a gc commit from ${art}: '$art version --json --long' printed ${long:-nothing}"
    sha="$(file_sha256 "$art")"
    was="$(lock_get gc_local_artifact) sha256=$(lock_attr gc_local_artifact sha256)"
    tmp="$(mktemp)"
    if ! awk -v p="$art" -v s="$sha" -v c="$pin" '
        $1 == "gc_local_artifact" {
          printf "gc_local_artifact %s sha256=%s pin=%s asserts=gc version --json --long -> commit == pin\n", p, s, c
          pinned = 1
          next
        }
        { print }
        END { exit (pinned ? 0 : 3) }
      ' "$LOCK" > "$tmp"; then
      rm -f "$tmp"
      die "no gc_local_artifact line in ${LOCK} — that line is part of the lock, add it (see the cr-dk9i2 block) rather than pinning by hand"
    fi
    mv "$tmp" "$LOCK"
    echo "refresh-pin: gc_local_artifact ${was:-none} -> ${art} sha256=${sha} pin=${pin}"
    echo "next: toolchain/build-image.sh check   # offline; then build on this host"
    ;;

  build) # the host step an allocation must never run
    [ "${GC_TOOLCHAIN_HOST_STEP:-}" = 1 ] || die "refusing to build: publishing the toolchain is an executive host step (ruling cr-on8x4 on cr-gdeav.2.13). Re-run with GC_TOOLCHAIN_HOST_STEP=1 on the build host."
    require_lock
    command -v docker >/dev/null || die "docker not found on this host"
    agent="${OUT_TAG:-$(lock_get agent_image)}"
    rollback="$(lock_get rollback_image)"
    art=""; art_sha=""; art_pin=""; staged=""
    if [ "$PUBLISHED" = 1 ]; then
      echo "toolchain gc source: PUBLISHED gc $(lock_version gc) (--published; the image will NOT carry the live pin)"
    else
      art="$(local_artifact_path)"
      if [ -z "$art" ]; then
        die "no gc_local_artifact in ${LOCK}. Building now ships published gc $(lock_version gc), which is likely older than the live pin. Pin the staged binary with 'toolchain/build-image.sh refresh-pin --artifact=/mnt/nomad/gc/bin/<pin>/gc', or pass --published deliberately."
      fi
      [ -f "$art" ] || die "gc_local_artifact=${art} is not on this host. Run the build on the host that stages it, or pass --published for upstream gc $(lock_version gc). Refusing to fall back silently — shipping an older gc is the defect this path exists to close."
      art_sha="$(local_artifact_sha)"; art_pin="$(local_artifact_pin)"
      now="$(file_sha256 "$art")"
      [ "$now" = "$art_sha" ] || die "gc_local_artifact checksum mismatch: on-disk=${now} locked=${art_sha}. Either the staged pin moved or the lock is stale — re-run 'toolchain/build-image.sh refresh-pin --artifact=${art}' only if this binary IS the pin you mean to ship."
      echo "toolchain gc source: LIVE PIN ${art} (commit ${art_pin}, sha256 ${art_sha})"
    fi
    if [ "$DRY" = 0 ] && docker image inspect "$agent" >/dev/null 2>&1; then
      prev_id="$(docker image inspect -f '{{.Id}}' "$agent")"
      run docker tag "$agent" "$rollback"
      echo "toolchain rollback: ${agent} -> ${rollback} (${prev_id})"
    fi
    # A minted build context, never the pack working tree: the staged gc is a
    # ~270MB artifact that must not sit in git's way, and a minted context is
    # bounded by construction. COPY .local/ needs the dir to be non-empty, hence
    # the placeholder; its presence or absence is not what selects the gc source.
    ctx="$(mktemp -d "${TMPDIR:-/tmp}/gc-toolchain-ctx.XXXXXX")"
    mkdir -p "${ctx}/.local"
    printf 'minted by toolchain/build-image.sh — build-context staging, never committed\n' > "${ctx}/.local/PLACEHOLDER"
    if [ -n "$art" ]; then
      cp "$art" "${ctx}/.local/gc"
      staged_sha="$(file_sha256 "${ctx}/.local/gc")"
      [ "$staged_sha" = "$art_sha" ] || { rm -rf "$ctx"; die "staged copy changed checksum: ${staged_sha} != ${art_sha}"; }
      staged="${ctx}/.local/gc"
    fi
    local_args=(--build-arg=GC_LOCAL_ARTIFACT= --build-arg=GC_LOCAL_SHA256= --build-arg=GC_PIN_COMMIT=)
    if [ -n "$art" ]; then
      local_args=(--build-arg=GC_LOCAL_ARTIFACT=gc --build-arg=GC_LOCAL_SHA256="$art_sha" --build-arg=GC_PIN_COMMIT="$art_pin")
    fi
    # shellcheck disable=SC2046
    run docker build -f "$DOCKERFILE" -t "$agent" $(args_from_lock) "${local_args[@]}" "$ctx"
    if [ "$DRY" = 1 ]; then
      echo "toolchain dry-run: context=${ctx}${staged:+ staged=${staged}} tag=${agent}"
    else
      id="$(docker image inspect -f '{{.Id}}' "$agent")"
      echo "toolchain built: ${agent} id=${id}"
      [ -z "$RECEIPT" ] || printf 'built agent=%s id=%s gc_source=%s gc_pin=%s rollback=%s at=%s\n' "$agent" "$id" "$([ -n "$art" ] && echo local || echo published)" "${art_pin:-published-$(lock_version gc)}" "$rollback" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$RECEIPT"
    fi
    rm -rf "$ctx"
    echo "next:  export GC_NOMAD_AGENT_IMAGE=${agent}    # the ONE image input the pack reads (cr-u7b0s.41)"
    echo "back:  export GC_NOMAD_AGENT_IMAGE=${rollback} # unsetting it fails every op closed - the image is the only session shape (cr-3vvnb5.2)"
    ;;

  city-image) # gc's native composition step, on top of the toolchain base
    require_lock
    require_bounded_city
    agent="${OUT_TAG:-$(lock_get agent_image)}"
    run gc build-image "$CITY" --base-image "$(lock_get agent_image)" --tag "$agent" --json
    [ "$DRY" = 1 ] || echo "toolchain city-image: ${agent} carries city content; secrets stay runtime-staged (buildimage/context.go excludes them)"
    ;;

  context-only) # PROVES CONTEXT ASSEMBLY ONLY — not a runnable image
    require_lock
    require_bounded_city
    run gc build-image "$CITY" --base-image "$(lock_get agent_image)" --context-only --json
    [ "$DRY" = 1 ] || echo "toolchain context-only: context assembly proven; a runnable image is NOT proven by this command"
    ;;

  *)
    [ "$cmd" = help ] || die "unknown command: $cmd"
    cat <<'USAGE'
usage: toolchain/build-image.sh <check|verify|refresh-pin|build|city-image|context-only>
       [--dry-run] [--city=<bounded-city>] [--tag=<image:tag>]
       [--artifact=<path-to-linux-gc>] [--published] [--receipt=<path>]

  check         validate toolchain/TOOLCHAIN.lock (offline; the release gate),
                including the live-pin gc_local_artifact entry when named
  verify        re-check every pin against its publisher (gc/bd/codex releases,
                Ubuntu Release index) and a staged local pin against its own
                checksum — read-only, needs network
  refresh-pin   rewrite gc_local_artifact's path+sha256+pin from the binary
                staged on THIS host (run it where /mnt/nomad/gc/bin lives)
  build         docker build the toolchain image, rollback-tagging whatever is
                live first (executive host step: needs GC_TOOLCHAIN_HOST_STEP=1,
                never pushed). Prefers the lock's live-pin local artifact;
                --published builds upstream gc deliberately instead.
  city-image    gc build-image --base-image <toolchain> --tag <agent_image>
  context-only  gc build-image --context-only (assembly proof only)

  --city MUST be a bounded city (a city root outside the estate checkout —
  ops/tests/test-city-kit/mk-test-city.sh mints one). gc build-image copies the
  whole directory and excludes only secrets, so an estate root is refused here.
USAGE
    ;;
esac
