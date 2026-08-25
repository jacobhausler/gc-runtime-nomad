#!/usr/bin/env bash
# Assemble the phase-1 conformance receipt (NRT-P1-90): runs the full
# offline ladder in one hermetic pass — no live Nomad cluster or network
# required — and writes a pass/fail-by-item result to
# CONFORMANCE-RECEIPT.md, linked to the commit it ran at.
#
# Ladder items (08 §1 pack-tier conformance standard):
#   check         gc runtime check            (RPP handshake + lifecycle ops)
#   golden suite  gc runtime conformance       (RPP-* golden requirements)
#   env probes    TestM3StagingReceiptWorkspaceProbe (env.workspace)
#   L0-L2         go vet + go test, runtime and fakenomad modules
#   secrets-grep  TestM3StagingReceiptNoCanaryLeak (zero planted-canary hits)
#
# Requires `gc` on PATH (see conformance.sh). Override with GC_BIN.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"

if [ -n "$(git status --porcelain)" ]; then
  echo "receipt: refusing to run against a dirty working tree (git status --porcelain is non-empty)" >&2
  echo "receipt: commit or stash changes first so the receipt names a single tested commit" >&2
  exit 1
fi

commit="$(git rev-parse HEAD)"
ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Runs "$@" with output teed (indented) to stdout and captured to $1; returns
# the command's own exit status so callers get a clean PASS/FAIL signal.
run_step() {
  local log="$1"
  shift
  "$@" >"$log" 2>&1
  local rc=$?
  sed 's/^/    /' "$log"
  return $rc
}

echo "receipt: L0-L2 — runtime module (go vet + go test)"
if run_step "$tmp/runtime.log" bash -c 'cd runtime && go vet ./... && go test -count=1 ./...'; then
  l0l2_runtime=PASS
else
  l0l2_runtime=FAIL
fi

echo "receipt: L0-L2 — fakenomad module (go vet + go test)"
if run_step "$tmp/fakenomad.log" bash -c 'cd fakenomad && go vet ./... && go test -count=1 ./...'; then
  l0l2_fakenomad=PASS
else
  l0l2_fakenomad=FAIL
fi

if [ "$l0l2_runtime" = PASS ] && [ "$l0l2_fakenomad" = PASS ]; then
  status_l0l2=PASS
else
  status_l0l2=FAIL
fi

echo "receipt: secrets-grep (canary receipt — zero planted-canary hits)"
if run_step "$tmp/secrets.log" bash -c 'cd runtime && go test -count=1 -run TestM3StagingReceiptNoCanaryLeak -v ./...'; then
  status_secrets=PASS
else
  status_secrets=FAIL
fi

echo "receipt: env probe (env.workspace staging)"
if run_step "$tmp/envprobe.log" bash -c 'cd runtime && go test -count=1 -run TestM3StagingReceiptWorkspaceProbe -v ./...'; then
  status_env=PASS
else
  status_env=FAIL
fi

echo "receipt: check + golden suite (gc runtime check / gc runtime conformance)"
run_step "$tmp/conformance.log" env GC_BIN="${GC_BIN:-gc}" ./conformance.sh || true

check_line="$(grep -E '^[0-9]+ checks:' "$tmp/conformance.log" || true)"
golden_line="$(grep -E '^[0-9]+ requirements:' "$tmp/conformance.log" || true)"

if [ -n "$check_line" ] && [[ "$check_line" == *" 0 failed"* ]]; then
  status_check=PASS
else
  status_check=FAIL
fi
if [ -n "$golden_line" ] && [[ "$golden_line" == *" 0 failed"* ]]; then
  status_golden=PASS
else
  status_golden=FAIL
fi

out="$here/CONFORMANCE-RECEIPT.md"
{
  echo "# Phase-1 conformance receipt (NRT-P1-90)"
  echo
  echo "Commit: \`$commit\`"
  echo "Generated: $ts"
  echo
  echo "Full offline ladder — no live Nomad cluster or network required. A row"
  echo "reading FAIL means the ladder is red at this commit; re-run \`./receipt.sh\`"
  echo "after a fix and regenerate this file. Fixing a red ladder step is out of"
  echo "scope for this receipt — bounce back to the owning bead."
  echo
  echo "| Ladder step | Result | Evidence |"
  echo "|---|---|---|"
  echo "| check | $status_check | \`gc runtime check\` — ${check_line:-did not run} |"
  echo "| golden suite | $status_golden | \`gc runtime conformance\` — ${golden_line:-did not run} |"
  echo "| env probes | $status_env | \`go test -run TestM3StagingReceiptWorkspaceProbe ./runtime\` |"
  echo "| L0-L2 | $status_l0l2 | \`go vet ./... && go test ./...\` — runtime + fakenomad modules |"
  echo "| secrets-grep | $status_secrets | \`go test -run TestM3StagingReceiptNoCanaryLeak ./runtime\` |"
} >"$out"

echo
echo "receipt written to $out"
cat "$out"

if [ "$status_check" = PASS ] && [ "$status_golden" = PASS ] && \
   [ "$status_env" = PASS ] && [ "$status_l0l2" = PASS ] && \
   [ "$status_secrets" = PASS ]; then
  exit 0
fi
exit 1
