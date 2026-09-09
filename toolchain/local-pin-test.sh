#!/usr/bin/env bash
# toolchain/local-pin-test.sh — offline fixture for cr-dk9i2 (the live-pin image path)
#
# Proves, with NO network, NO docker daemon and NO estate side effects:
#   1. the lock's live-pin entry is well-formed and what `check` reports;
#   2. `build` PREFERS the staged local gc and passes its checksum + commit into
#      the Dockerfile, into a MINTED build context (never the pack working tree);
#   3. it REFUSES rather than silently shipping the older published gc — named
#      but missing, checksum mismatch, unpinned line, no line at all;
#   4. --published is the deliberate way back to upstream, and then the local
#      args go empty (the Dockerfile's public default);
#   5. --city that is the estate checkout (or any non-city) is refused, and a
#      bounded city is accepted;
#   6. refresh-pin will not pin a missing binary or a non-x86-64 one;
#   7. the Dockerfile and the script agree on the staging contract.
#
# The happy path of refresh-pin CANNOT run here: it needs the staged linux pin,
# which lives on the build host (a Mac's own live pin is a darwin build). It is
# proven there — see BUILD-RECEIPT.md "Live pin in the image".
set -uo pipefail

TOOLCHAIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REAL_SCRIPT="${TOOLCHAIN_DIR}/build-image.sh"
DOCKERFILE="${TOOLCHAIN_DIR}/Dockerfile"
REAL_LOCK="${TOOLCHAIN_DIR}/TOOLCHAIN.lock"
W="$(mktemp -d "${TMPDIR:-/tmp}/gc-local-pin-test.XXXXXX")"
trap 'rm -rf "$W"' EXIT
fails=0
ok()   { printf 'ok   %s\n' "$1"; }
bad()  { printf 'FAIL %s\n' "$1"; fails=$((fails+1)); }
expect() { # expect <label> <needle> <cmd…>
  local label="$1" needle="$2"; shift 2
  local out; out="$("$@" 2>&1)" || true
  case "$out" in *"$needle"*) ok "$label" ;; *) bad "$label — no '$needle' in: $(printf '%s' "$out" | head -3)";; esac
}
refuse() { # refuse <label> <needle> <cmd…>  — must exit non-zero AND say why
  local label="$1" needle="$2"; shift 2
  local out rc=0
  out="$("$@" 2>&1)" || rc=$?
  [ "$rc" -ne 0 ] || { bad "$label — exit was 0, nothing was refused"; return; }
  case "$out" in *"$needle"*) ok "$label" ;; *) bad "$label — refused without naming '$needle': $(printf '%s' "$out" | head -3)";; esac
}

# A pack copy in /tmp with its own lock, so the pins can be moved without
# touching the estate checkout. PACK_ROOT follows BASH_SOURCE, so this works.
mint_pack() { # mint_pack <name> -> echo pack root
  pr="${W}/$1"; mkdir -p "${pr}/toolchain"
  cp "${REAL_SCRIPT}" "${DOCKERFILE}" "${REAL_LOCK}" "${pr}/toolchain/"
  printf '%s\n' "${pr}"
}
strip_lock_pin() { # strip_lock_pin <pack-root> — the lock WITHOUT any local-artifact line
  awk '$1!="gc_local_artifact"' "${1}/toolchain/TOOLCHAIN.lock" > "${1}/toolchain/l2" && mv "${1}/toolchain/l2" "${1}/toolchain/TOOLCHAIN.lock"
}
set_lock_pin() { # set_lock_pin <pack-root> <path> <sha> <pin>
  awk -v p="$2" -v s="$3" -v c="$4" '
    $1=="gc_local_artifact" { printf "gc_local_artifact %s sha256=%s pin=%s asserts=gc version --json --long -> commit == pin\n", p, s, c; next }
    { print }' "${1}/toolchain/TOOLCHAIN.lock" > "${1}/toolchain/l2" && mv "${1}/toolchain/l2" "${1}/toolchain/TOOLCHAIN.lock"
}

# --- 1. the shipped lock -------------------------------------------------------
expect "check passes on the shipped lock"        "six components sha256-pinned" bash "${REAL_SCRIPT}" check
expect "check reports the live pin"              "gc LOCAL  /mnt/nomad/gc/bin/" bash "${REAL_SCRIPT}" check
expect "check keeps upstream as the public default" "published gc 1.4.1 stays the public default" bash "${REAL_SCRIPT}" check

# --- 2. the preference, and a bounded context ---------------------------------
P="$(mint_pack prefer)"
FAKE="${P}/staged/gc"; mkdir -p "${P}/staged"
printf 'fake staged gc\n' > "${FAKE}"
FAKE_SHA="$(if command -v sha256sum >/dev/null 2>&1; then sha256sum -b "$FAKE" | awk '{print $1}'; else shasum -a 256 -b "$FAKE" | awk '{print $1}'; fi)"
set_lock_pin "${P}" "${FAKE}" "${FAKE_SHA}" "deadbee00"
out="$(GC_TOOLCHAIN_HOST_STEP=1 bash "${P}/toolchain/build-image.sh" build --dry-run 2>&1)" || true
case "$out" in *"--build-arg=GC_LOCAL_ARTIFACT=gc"*) ok "build passes the staged artifact to the Dockerfile";; *) bad "build did not pass GC_LOCAL_ARTIFACT=gc: $(printf '%s' "$out" | head -3)";; esac
case "$out" in *"--build-arg=GC_LOCAL_SHA256=${FAKE_SHA}"*) ok "build passes the pinned checksum";; *) bad "build did not pass GC_LOCAL_SHA256: $(printf '%s' "$out" | head -3)";; esac
case "$out" in *"--build-arg=GC_PIN_COMMIT=deadbee00"*) ok "build passes the pin commit";; *) bad "build did not pass GC_PIN_COMMIT";; esac
case "$out" in *"context="*"gc-toolchain-ctx."*"staged="*".local/gc"*) ok "build stages into a minted context, not the pack working tree";; *) bad "build did not report a minted staged context: $(printf '%s' "$out" | tail -3)";; esac
case "$out" in *"${P}/toolchain "*|*"${P}/toolchain"$'\n'*) bad "build used the pack working tree as the docker context";; *) ok "docker context is not the pack directory";; esac

# --- 3. the refusals: never a silent fallback to an older gc ------------------
P="$(mint_pack missing)"; FAKE="${P}/staged/gc"; mkdir -p "${P}/staged"; printf 'x\n' > "${FAKE}"
set_lock_pin "${P}" "${P}/staged/gone" "0000000000000000000000000000000000000000000000000000000000000000" "abc123456"
refuse "named-but-missing artifact is refused" "is not on this host" env GC_TOOLCHAIN_HOST_STEP=1 bash "${P}/toolchain/build-image.sh" build --dry-run

P="$(mint_pack mismatch)"; FAKE="${P}/staged/gc"; mkdir -p "${P}/staged"; printf 'the real bytes\n' > "${FAKE}"
set_lock_pin "${P}" "${FAKE}" "1111111111111111111111111111111111111111111111111111111111111111" "abc123456"
refuse "checksum mismatch is refused" "checksum mismatch" env GC_TOOLCHAIN_HOST_STEP=1 bash "${P}/toolchain/build-image.sh" build --dry-run

P="$(mint_pack unpinned)"; FAKE="${P}/staged/gc"; mkdir -p "${P}/staged"; printf 'x\n' > "${FAKE}"
set_lock_pin "${P}" "${FAKE}" "" ""
refuse "an unchecksummed local line is refused" "no sha256=<hex> pin" bash "${P}/toolchain/build-image.sh" check

P="$(mint_pack nolocal)"; strip_lock_pin "${P}"
refuse "no local line + no --published is refused" "which is likely older than the live pin" env GC_TOOLCHAIN_HOST_STEP=1 bash "${P}/toolchain/build-image.sh" build --dry-run
# the cr-dk9i2 COMMENT block names gc_local_artifact too; only a line whose first
# field is the key is a pin (that is what lock_get reads), so assert on the anchor.
grep -qE '^gc_local_artifact ' "${P}/toolchain/TOOLCHAIN.lock" && bad "strip_lock_pin left an active pin" || ok "a lock with only the comment block has no active pin"

# --- 4. --published stays the public default ----------------------------------
P="$(mint_pack published)"
out="$(GC_TOOLCHAIN_HOST_STEP=1 bash "${P}/toolchain/build-image.sh" build --dry-run --published 2>&1)" || true
case "$out" in *"gc source: PUBLISHED"*) ok "--published names itself out loud";; *) bad "--published did not announce itself: $(printf '%s' "$out" | head -3)";; esac
case "$out" in *"--build-arg=GC_LOCAL_ARTIFACT= "--*) ok "--published empties the local artifact args";; *) bad "--published left the local args set: $(printf '%s' "$out" | head -3)";; esac
out="$(GC_TOOLCHAIN_HOST_STEP=1 bash "${P}/toolchain/build-image.sh" build --dry-run --published 2>&1)" || true
case "$out" in *"--build-arg=GC_SHA256=8d8c8b51"*) ok "--published keeps the upstream published checksum";; *) bad "--published lost the published pin";; esac

# --- 5. the bounded-city refusal ----------------------------------------------
ESTATE="$(cd "${TOOLCHAIN_DIR}/../.." && git rev-parse --show-toplevel 2>/dev/null || true)"
case "${ESTATE}" in */gc-runtime-nomad|"") ESTATE="" ;; esac
if [ -n "${ESTATE}" ]; then
  refuse "the estate checkout is refused as a build context" "estate checkout" bash "${REAL_SCRIPT}" city-image --city="${ESTATE}" --dry-run
  refuse "…and by context-only too" "estate checkout" bash "${REAL_SCRIPT}" context-only --city="${ESTATE}" --dry-run
else
  ok "estate checkout not derivable from this checkout (pack not under <estate>/work/) — estate refusal not applicable"
fi
refuse "a non-city directory is refused" "no city.toml" bash "${REAL_SCRIPT}" city-image --city="${W}" --dry-run
BOUNDED="${W}/gctc-fake-city"; mkdir -p "${BOUNDED}"; printf '# bounded test city\n' > "${BOUNDED}/city.toml"
out="$(bash "${REAL_SCRIPT}" city-image --city="${BOUNDED}" --dry-run 2>&1)" || true
case "$out" in *"+ gc build-image "*gctc-fake-city*"--json"*) ok "a bounded city passes and gc build-image is the command run";; *) bad "a bounded city was not accepted: $(printf '%s' "$out" | head -3)";; esac
refuse "--city is required" "bounded city" bash "${REAL_SCRIPT}" city-image --dry-run

# --- 6. refresh-pin refusals --------------------------------------------------
refuse "refresh-pin will not pin a missing binary" "no gc binary at" bash "${REAL_SCRIPT}" refresh-pin --artifact="${W}/definitely-not-here"
refuse "refresh-pin will not pin a non-x86-64 binary" "not an x86-64 ELF" bash "${REAL_SCRIPT}" refresh-pin --artifact="${REAL_SCRIPT}"
refuse "refresh-pin with no --artifact prints usage" "usage: toolchain/build-image.sh refresh-pin" bash "${REAL_SCRIPT}" refresh-pin

# --- 7. the Dockerfile agrees with the script ---------------------------------
grep -q 'ARG GC_LOCAL_ARTIFACT=' "${DOCKERFILE}" && ok "Dockerfile declares the local-artifact ARGs" || bad "Dockerfile lost ARG GC_LOCAL_ARTIFACT"
grep -q 'ARG GC_LOCAL_SHA256=' "${DOCKERFILE}" && grep -q 'ARG GC_PIN_COMMIT=' "${DOCKERFILE}" && ok "Dockerfile declares the checksum + commit ARGs" || bad "Dockerfile lost GC_LOCAL_SHA256/GC_PIN_COMMIT"
grep -q 'COPY .local/' "${DOCKERFILE}" && ok "Dockerfile copies the minted staging dir" || bad "Dockerfile lost COPY .local/"
grep -q "jq -r '.commit // empty'" "${DOCKERFILE}" && ok "Dockerfile asserts the pin's reported commit by equality" || bad "Dockerfile lost the commit equality assertion"
# The docker build on rhuidean is what found the FIRST version of the gc RUN line
# unparseable ("/bin/sh: 1: Syntax error: Unterminated quoted string") — an
# ~8-minute round trip through bd's rebuild to learn it. So every RUN body is
# joined back from its continuations and parsed here, offline, with `sh -n`: a RUN
# that does not parse is a build step that fails, caught at no cost and no daemon.
# The image's /bin/sh is dash (ubuntu:24.04). /bin/dash exists on the Mac too; use
# it when present so this parses the way the build step will. The CANARY is not
# optional decoration: a syntax gate that cannot reject an unbalanced quote is a
# gate that passes every build, so it is reported as inert instead of as a pass.
DK_SH="/bin/sh"
command -v dash >/dev/null 2>&1 && DK_SH="$(command -v dash)"
if printf '%s\n' 'echo "unbalanced' | "$DK_SH" -n 2>/dev/null; then
  bad "the RUN-parse gate is INERT with ${DK_SH} — it accepted an unbalanced quote; it proves nothing here"
else
  ok "RUN-parse gate bites (${DK_SH} rejects an unbalanced quote)"
fi
while IFS= read -r run_body; do
  [ "$run_body" = "$(printf '\001END\001')" ] && continue
  label="$(printf '%s' "$run_body" | tr '\n' ' ' | cut -c1-46)"
  if printf '%s\n' "$run_body" | "$DK_SH" -n 2>/tmp/gc-dk-shn.err; then
    ok "Dockerfile RUN parses under sh -n: ${label}…"
  else
    bad "Dockerfile RUN will fail its build step: ${label}… — $(tr -d '\n' < /tmp/gc-dk-shn.err | cut -c1-110)"
  fi
done < <(python3 -c '
import sys
lines = open(sys.argv[1]).read().split("\n")
i = 0
while i < len(lines):
    if lines[i].startswith("RUN"):
        body = lines[i][3:].rstrip()
        while body.endswith(chr(92)) and i + 1 < len(lines):
            i += 1
            body = body[:-1] + " " + lines[i].strip()
        print(body)
    i += 1
' "${DOCKERFILE}")
grep -q 'sha256sum --check --strict' "${DOCKERFILE}" && ok "Dockerfile checksum-verifies what it installs" || bad "Dockerfile lost its checksum gate"
# An image that installs a binary it cannot load is the failure this lane keeps
# hitting — the host always has the library, the digest-pinned base does not.
grep -qE 'libicu74=\$\{LIBICU_VERSION\}' "${DOCKERFILE}" && ok "apt installs the local gc ICU dependency" || bad "Dockerfile lost libicu74: the staged pin cannot load without it"
grep -q 'not found' "${DOCKERFILE}" && ok "the build asserts nothing unresolved before running gc" || bad "Dockerfile has no ldd gate: an unloadable gc would build green again"
grep -qE '^libicu74 [^ ]+ sha256:[0-9a-f]{64}' "${REAL_LOCK}" && ok "ICU is checksum-pinned like every other apt line here" || bad "libicu74 is not checksum-pinned in the lock"
grep -q 'PLACEHOLDER' "${REAL_SCRIPT}" && ok "script mints a placeholder so COPY never sees an empty dir" || bad "script does not mint the placeholder COPY needs"

if [ "$fails" = 0 ]; then
  echo "local-pin-test: PASS"
else
  printf 'local-pin-test: FAIL (%d failing)\n' "$fails"
  exit 1
fi
