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
  # Resolve the directory rather than telling the reader to go resolve it.
  # `gc` finds this runtime by bare name on PATH (RUNTIME-SEL-011), so an
  # install that lands outside PATH leaves gc reporting "pack runtime
  # unusable" with nothing pointing back here.
  bindir="$(go env GOBIN)"
  [ -n "$bindir" ] || bindir="$(go env GOPATH)/bin"
  echo "runtime-nomad: installed to ${bindir}, which is NOT on your PATH." >&2
  echo "runtime-nomad: gc resolves this runtime by bare name on PATH, so it cannot" >&2
  echo "               use the binary until that changes. Add it:" >&2
  echo "" >&2
  echo "                   export PATH=\"${bindir}:\$PATH\"" >&2
  echo "" >&2
  echo "               then re-run ./install.sh to verify." >&2
  exit 1
fi
installed="$(command -v gc-runtime-nomad)"
echo "runtime-nomad: installed ${installed}"

# Self-verify. An install step that only copies a file cannot tell you the
# install is unusable, and the two ways it is usually unusable are both cheap
# to check right here.

# 1. It speaks the protocol gc will talk to it in.
if ! handshake="$("${installed}" protocol 2>&1)"; then
  echo "runtime-nomad: FAILED - the installed binary does not answer the RPP handshake:" >&2
  echo "               ${handshake}" >&2
  exit 1
fi
echo "runtime-nomad: handshake ok - ${handshake}"

# 2. The gc it will be driven by can actually do the job. This is the most
#    common reason this pack does not work for a new installer, and finding
#    out here beats finding out after building an image and wiring a lane.
#    `check` exits nonzero when the gc on PATH cannot claim work from inside
#    an allocation; its own message explains the rest.
if ! "${installed}" check; then
  echo "" >&2
  echo "runtime-nomad: the binary is installed and answers the protocol, but the" >&2
  echo "               environment above will not run sessions. See COMPATIBILITY.md" >&2
  echo "               and docs/PIN-DEPENDENCIES.md." >&2
  exit 1
fi
echo "runtime-nomad: check ok"
