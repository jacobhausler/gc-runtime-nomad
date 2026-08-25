#!/usr/bin/env bash
# Install the gc-runtime-nomad executable onto PATH via the Go toolchain.
# This is the pack's install step (Gas City RUNTIME-SEL-011): the
# pack-runtimes doctor check then verifies the binary is installed and
# answers the RPP protocol handshake.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "runtime-nomad: installing gc-runtime-nomad via go install"
( cd "$here/runtime" && go install . )

if ! command -v gc-runtime-nomad >/dev/null 2>&1; then
  echo "runtime-nomad: installed, but gc-runtime-nomad is not on PATH." >&2
  echo "Add \$(go env GOBIN) or \$(go env GOPATH)/bin to PATH." >&2
  exit 1
fi
echo "runtime-nomad: installed $(command -v gc-runtime-nomad)"
