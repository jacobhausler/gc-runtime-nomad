#!/usr/bin/env bash
# Install the gc-runtime-nomad executable onto PATH via the Go toolchain, and
# (opt-in) deploy the versioned agent launcher the launched pane execs.
# This is the pack's install step (Gas City RUNTIME-SEL-011): the
# pack-runtimes doctor check then verifies the binary is installed and
# answers the RPP protocol handshake.
#
# Usage:
#   ./install.sh                    go install + agent-launch deploy (if configured)
#   ./install.sh agent-launch       ONLY the agent-launch deploy
#
# The second form exists because this estate forbids Go toolchain runs on the
# Mac (AGENTS.md - builds go through ops/bin/nomad-run), while the launcher
# deploy is a pure ssh+scp job that has to be runnable from the operator seat.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

install_go=1
install_agent_launch=1
case "${1:-}" in
  agent-launch) install_go=0 ;;
  '') ;;
  *)
    echo "runtime-nomad: unknown argument '$1' (usage: install.sh [agent-launch])" >&2
    exit 2
    ;;
esac

sha256_of() {
  # sha256sum is GNU coreutils, shasum -a 256 is what ships on the Mac; this
  # script runs on both (the pack is installed from the Mac and from Linux).
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

agent_launch_deploy() {
  # $1 local source, $2 ssh host, $3 absolute target path on that host.
  local source="$1" host="$2" target="$3"
  local stamp remote_sha local_sha meta owner group mode rollback tmp

  if [ ! -f "$source" ]; then
    echo "runtime-nomad: agent-launch source $source not found" >&2
    exit 1
  fi
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  local_sha=$(sha256_of "$source")

  # One read to learn what is live now: owner, group, mode (numeric, so the
  # deploy preserves them without needing the host's user/group database).
  meta=$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$host" \
    "stat -c '%u %g %a' '$target' 2>/dev/null || echo missing") || {
    echo "runtime-nomad: cannot read $target on $host" >&2
    exit 1
  }
  if [ "$meta" = missing ]; then
    owner=0; group=0; mode=644; rollback=none
  else
    set -- $meta
    owner=$1; group=$2; mode=$3
    # Same content already live: say so and change nothing (re-runnable).
    remote_sha=$(ssh -o BatchMode=yes "$host" "sha256sum '$target' | awk '{print \$1}'")
    if [ "$remote_sha" = "$local_sha" ]; then
      echo "runtime-nomad: agent-launch.sh on $host already matches $source (sha256=$local_sha)"
      return 0
    fi
    # Dated rollback copy, mode-preserving, before anything is overwritten.
    rollback="$target.pre-$stamp"
    ssh -o BatchMode=yes "$host" "set -eu; test -e '$target'; sudo -n cp -p '$target' '$rollback'" || {
      echo "runtime-nomad: refused to deploy - the rollback copy of $target could not be written" >&2
      exit 1
    }
  fi

  tmp="/tmp/agent-launch.sh.cr-gdeav-6-10.$stamp"
  scp -q -o BatchMode=yes "$source" "$host:$tmp" || {
    echo "runtime-nomad: scp of $source to $host:$tmp failed" >&2
    exit 1
  }
  # install -m/-o/-g lands the bytes atomically-enough for a file this small and
  # normalizes identity explicitly, so a deploy can never widen who may run the
  # estate's agent bootstrap.
  ssh -o BatchMode=yes "$host" "set -eu; sudo -n install -m '$mode' -o '$owner' -g '$group' '$tmp' '$target'" || {
    echo "runtime-nomad: install of $tmp to $target failed (rollback copy: $rollback)" >&2
    exit 1
  }
  remote_sha=$(ssh -o BatchMode=yes "$host" "sha256sum '$target' | awk '{print \$1}'") || {
    echo "runtime-nomad: cannot verify $target after deploy (rollback copy: $rollback)" >&2
    exit 1
  }
  if [ "$remote_sha" != "$local_sha" ]; then
    echo "runtime-nomad: sha256 mismatch after deploy - local=$local_sha live=$remote_sha (rollback copy: $rollback)" >&2
    exit 1
  fi
  echo "runtime-nomad: deployed agent-launch.sh host=$host target=$target sha256=$remote_sha owner=$owner:$group mode=$mode rollback=$rollback"
}


if [ "$install_go" = 1 ]; then
  echo "runtime-nomad: installing gc-runtime-nomad via go install"
  ( cd "$here/runtime" && go install . )

  if ! command -v gc-runtime-nomad >/dev/null 2>&1; then
    echo "runtime-nomad: installed, but gc-runtime-nomad is not on PATH." >&2
    echo "Add \$(go env GOBIN) or \$(go env GOPATH)/bin to PATH." >&2
    exit 1
  fi
  echo "runtime-nomad: installed $(command -v gc-runtime-nomad)"
fi

# --- agent-launch.sh deploy (cr-gdeav.6.10) ---------------------------------
# The launched pane execs a deployment-owned bootstrap that lives on the shared
# artifact volume (city.toml's provider command/GC_NOMAD_AGENT_LAUNCH_SCRIPT
# sources it), and until today the ONLY copy of it was the live file: cr-gdeav.6.5
# gate 0 found that copy had no git owner - no diff, no review, no rollback - and
# the city checkout's ops/nomad/agent-launch.sh is its first versioned source.
# Since the live copy is what the pack's launch path actually execs, an install
# that only does `go install` cannot make a launcher change take effect; the Sep 2
# bytes kept being the live bootstrap no matter what the repo said (D3 attempt 3).
#
# Native form checked first: gc has no artifact-volume deploy step - the volume is
# a Nomad host volume gc never sees (its provider env only names paths inside the
# alloc), and the pack has no install hook beyond this script (RUNTIME-SEL-011 is
# satisfied by exactly this file). So this is the pack's own install step, not a
# new layer around a gc command.
#
# Deliberately opt-in, because a pack install on an arbitrary box must not ssh
# into the estate: unset GC_NOMAD_AGENT_LAUNCH_SOURCE and install.sh stays the
# `go install` it always was (the rollback path). When it IS set: verify-by-sha256
# (idempotent - a matching live copy is left alone), take a dated rollback copy
# before overwriting (nothing is ever deleted), preserve the live file's owner,
# group and mode rather than imposing new ones, and fail closed on any mismatch
# between the bytes shipped and the bytes verified.
: "${GC_NOMAD_AGENT_LAUNCH_SOURCE:=}"
: "${GC_NOMAD_AGENT_LAUNCH_HOST:=rhuidean}"
: "${GC_NOMAD_AGENT_LAUNCH_TARGET:=/mnt/nomad/gc/agent-launch.sh}"

if [ "$install_agent_launch" = 0 ]; then
  :
elif [ -z "$GC_NOMAD_AGENT_LAUNCH_SOURCE" ]; then
  echo "runtime-nomad: agent-launch.sh deploy skipped (GC_NOMAD_AGENT_LAUNCH_SOURCE unset)."
else
  agent_launch_deploy "$GC_NOMAD_AGENT_LAUNCH_SOURCE" "$GC_NOMAD_AGENT_LAUNCH_HOST" "$GC_NOMAD_AGENT_LAUNCH_TARGET"
fi
