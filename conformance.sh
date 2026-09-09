#!/usr/bin/env bash
# Build gc-runtime-nomad and its in-memory fake Nomad API server, then run
# the Gas City RPP conformance suite against the binary with no live Nomad
# cluster or network. This is the pack's CI gate (NRT-P1-04): a green run
# proves the pack-shipped runtime satisfies the Runtime Provider Protocol
# independently of the gascity codebase.
#
# Requires `gc` AND `tmux` on PATH (tmux because the pack's own launch command
# is a tmux command exec'd into the box -- see the prerequisite door below).
# Requires `gc` on PATH (install with a pinned:
#   go install github.com/gastownhall/gascity/cmd/gc@<pin>
# — a tool install, not a source import, so the pack keeps zero gascity Go
# dependencies). Override the binary with GC_BIN=/path/to/gc.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
runtime_dir="$here/runtime"
fakenomad_dir="$here/fakenomad"
bindir="$(mktemp -d)"
sidecar_dir="$(mktemp -d)"
trap 'rm -rf "$bindir" "$sidecar_dir"; [ -n "${server_pid:-}" ] && kill "$server_pid" 2>/dev/null || true' EXIT

gc_bin="${GC_BIN:-gc}"
if ! command -v "$gc_bin" >/dev/null 2>&1 && [ ! -x "$gc_bin" ]; then
  echo "conformance: gc binary not found (set GC_BIN or install gc)" >&2
  exit 1
fi

# Prerequisite door -- the same shape receipt.sh uses for jq (receipt.sh:63).
# `start` launches the in-box agent with tmux: runtime/ops.go's
# buildLaunchCommandForTmuxWithPaneCommand builds
# `tmux new-session -d -s main` and execs it into the alloc, so tmux must be
# installed on whatever host runs this ladder (a Linux container included).
# Without it `start` fails inside the box with exit 127 and the only line that
# reaches the caller is the pack's informational "Meta binding recovery not
# used" decision log (runtime/ops.go liveChild -> logDecision) -- a line that
# also prints in a GREEN run and reads like a binding-recovery bug when it is
# not one. cr-fslju: that is exactly how a missing tmux was misdiagnosed as
# Remediate H failing on Linux while the identical ladder passed on Darwin.
if ! command -v tmux >/dev/null 2>&1; then
  echo "conformance: tmux is required (the pack's start runs 'tmux new-session -d -s main' in the box); install it, e.g. apt-get install -y tmux" >&2
  exit 1
fi

echo "conformance: building gc-runtime-nomad + fakenomad"
( cd "$runtime_dir" && go build -o "$bindir/gc-runtime-nomad" . )
( cd "$fakenomad_dir" && go build -o "$bindir/fakenomad" ./cmd/fakenomad )

echo "conformance: starting fake Nomad server"
server_out="$(mktemp)"
"$bindir/fakenomad" >"$server_out" 2>/dev/null &
server_pid=$!
# Wait for the server to announce its base URL on the first stdout line.
for _ in $(seq 1 50); do
  url="$(head -n1 "$server_out" 2>/dev/null || true)"
  [ -n "$url" ] && break
  sleep 0.1
done
if [ -z "${url:-}" ]; then
  echo "conformance: fake Nomad server did not report a URL" >&2
  exit 1
fi
echo "conformance: fake Nomad server at $url"

export GC_NOMAD_ADDR="$url"
export GC_NOMAD_TOKEN=""
export GC_NOMAD_NAMESPACE="default"
export GC_NOMAD_SIDECAR_DIR="$sidecar_dir"

"$bindir/gc-runtime-nomad" check

echo "conformance: gc runtime check"
"$gc_bin" runtime check "$bindir/gc-runtime-nomad"

echo "conformance: gc runtime conformance"
"$gc_bin" runtime conformance "$bindir/gc-runtime-nomad"

# Provision/start retry idempotency (fnrt-t4l.46) — driven through the
# binary's own wire, not the Go ops layer, because the failure this rung
# guards was a controller re-driving the CLI verb every tick: a provision
# whose launch was never confirmed left a live, unlaunched box, and every
# subsequent provision was rejected with "already exists" (61 of them in
# DF-03) instead of finishing the job. Provision must adopt the box it
# already made, start must resume into it, and neither may mint a second
# child.
echo "conformance: provision/start retry idempotency (fnrt-t4l.46)"
retry_session="gc-rpp-retry-$$"
"$bindir/gc-runtime-nomad" provision "$retry_session" </dev/null
"$bindir/gc-runtime-nomad" provision "$retry_session" </dev/null
if [ "$("$bindir/gc-runtime-nomad" is-running "$retry_session")" != "false" ]; then
  echo "conformance: re-provision must not launch the agent" >&2
  exit 1
fi
"$bindir/gc-runtime-nomad" start "$retry_session" </dev/null
if [ "$("$bindir/gc-runtime-nomad" is-running "$retry_session")" != "true" ]; then
  echo "conformance: start after provision must resume into the provisioned box" >&2
  exit 1
fi
# A launched session is a genuine duplicate, not a retry: both verbs must
# still reject it with the 04 §6 wire-contract phrase gc reads as
# ErrSessionExists.
for verb in start provision; do
  if err="$("$bindir/gc-runtime-nomad" "$verb" "$retry_session" </dev/null 2>&1)"; then
    echo "conformance: $verb on a launched session succeeded; want an already-exists rejection" >&2
    exit 1
  fi
  case "$err" in
    *"already exists"*) ;;
    *) echo "conformance: $verb on a launched session said '$err'; want an \"already exists\" error" >&2; exit 1 ;;
  esac
done
"$bindir/gc-runtime-nomad" stop "$retry_session"
# Machine-readable rung result. This is the LAST line the ladder writes, and
# receipt.sh derives the retry_idempotency row from it: the rung runs after gc
# has printed both summaries, so the summaries cannot report it (cr-u7b0s.48).
echo "retry idempotency: 1 passed, 0 failed, 0 skipped"
