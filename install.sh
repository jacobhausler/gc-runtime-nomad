#!/usr/bin/env bash
# Install the gc-runtime-nomad executable onto PATH via the Go toolchain.
# This is the pack's install step (Gas City RUNTIME-SEL-011): the
# pack-runtimes doctor check then verifies the binary is installed and
# answers the RPP protocol handshake.
#
# What is NOT here any more: the opt-in agent-launch.sh deploy this script used
# to carry (cr-gdeav.6.10). That step existed to put a deployment-owned
# bootstrap on the shared artifact volume for the launched pane to exec, and
# the bootstrap was only ever needed to give a host-mounted toolchain a PATH and
# an agent HOME. The prebaked image owns the agent toolchain now: the pane runs
# gc's rendered launch command and nothing else, so there is no bootstrap left
# for an install step to deploy (cr-3vvnb5).
#
# Usage:
#   ./install.sh    go install the runtime onto PATH
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

case "${1:-}" in
  '') ;;
  *)
    echo "runtime-nomad: unknown argument '$1' (usage: install.sh)" >&2
    exit 2
    ;;
esac

echo "runtime-nomad: installing gc-runtime-nomad via go install"
( cd "$here/runtime" && go install . )

if ! command -v gc-runtime-nomad >/dev/null 2>&1; then
  echo "runtime-nomad: installed, but gc-runtime-nomad is not on PATH." >&2
  echo "Add \$(go env GOBIN) or \$(go env GOPATH)/bin to PATH." >&2
  exit 1
fi
echo "runtime-nomad: installed $(command -v gc-runtime-nomad)"
