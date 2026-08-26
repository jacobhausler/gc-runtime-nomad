#!/usr/bin/env bash
# Build gc-runtime-nomad and its in-memory fake Nomad API server, then run
# the Gas City RPP conformance suite against the binary with no live Nomad
# cluster or network. This is the pack's CI gate (NRT-P1-04): a green run
# proves the pack-shipped runtime satisfies the Runtime Provider Protocol
# independently of the gascity codebase.
#
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
