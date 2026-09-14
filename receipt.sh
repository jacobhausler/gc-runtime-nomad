#!/usr/bin/env bash
# Assemble the phase-1 conformance receipt: runs the full
# offline ladder in one hermetic pass — no live Nomad cluster or network
# required — and writes a pass/fail-by-item result to
# CONFORMANCE-RECEIPT.md, linked to the commit AND the gc pin that ran it.
#
# Ladder items:
#   check              gc runtime check       (RPP handshake + lifecycle ops)
#   golden suite       gc runtime conformance  (RPP-* golden requirements)
#   retry idempotency  provision/start re-drive rung
#   env probes         TestM3StagingReceiptWorkspaceProbe (env.workspace)
#   L0-L2              go vet + go test, runtime and fakenomad modules
#   secrets-grep       TestM3StagingReceiptNoCanaryLeak (zero planted-canary hits)
#   provenance         the gc identity the rows were produced by
#
# THE RECEIPT FAILS CLOSED. Every rung's own exit status gates
# this script's exit status, including conformance.sh's: the ladder's last rung
# runs *after* gc has printed both greppable summaries, so deriving rows from
# the summaries alone let a red rung leave a green receipt on disk. The rows
# report what gc said; the exit code reports what the ladder did.
#
# Machine-readable mode: `./receipt.sh --json-stdout` writes the result as one
# JSON object on stdout and puts every human line on stderr, so a seat driving
# this through a remote-build wrapper — one with no artifact-return flag, only a
# captured task stream — can capture the receipt and commit it as
# CONFORMANCE-RECEIPT.json. Field contract: pack_commit (40-hex), gc_commit,
# gc_sha256 (64-hex), and one {passed,failed,skipped,status} row per ladder
# item. gc's own machine-readable report carries no gc identity
# (runtimecontract.Report is {executable, protocol, results, summary};
# runtime.ProtocolInfo is {version, capabilities}), so the pin attribution is
# stamped here from `gc version --json` + the binary digest; a run that cannot
# name its gc fails the provenance row rather than emitting unattributed rows.
#
# RECEIPT_RUNNER_LOG (optional) is a citation and nothing more: when it names
# the committed path this run's own log lives at -- a CI log file, a builder
# wrapper's receipt -- the receipt prints that path. It gates no row and
# changes no result; a run with no citable log reads exactly as it did
# before. It exists because a ladder run whose log was written somewhere
# git never sees certifies nothing an adopter can go read.
# Requires `gc` on PATH (see conformance.sh) and `jq`. Override with GC_BIN.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"

json_stdout=0
for arg in "$@"; do
  case "$arg" in
    --json-stdout) json_stdout=1 ;;
    -h|--help)
      printf 'usage: receipt.sh [--json-stdout]\n'
      exit 0
      ;;
    *)
      printf 'receipt: unrecognized argument %s\n' "$arg" >&2
      exit 2
      ;;
  esac
done

# stdout carries the JSON object and nothing else in machine-readable mode.
if [ "$json_stdout" = 1 ]; then
  exec 3>&2
else
  exec 3>&1
fi
log() { printf '%s\n' "$*" >&3; }

command -v jq >/dev/null 2>&1 || {
  log "receipt: jq is required (the machine-readable result is built with it)"
  exit 1
}

if [ -n "$(git status --porcelain)" ]; then
  echo "receipt: refusing to run against a dirty working tree (git status --porcelain is non-empty)" >&2
  echo "receipt: commit or stash changes first so the receipt names a single tested commit" >&2
  exit 1
fi

commit="$(git rev-parse HEAD)"
ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Attribute the run to the gc that produced it. `gc version --json` is the only
# gc-side identity at the pin (`gc version` reports version "dev"); the digest
# covers the case where the version string says nothing.
gc_bin="${GC_BIN:-gc}"
gc_path="$(command -v "$gc_bin" 2>/dev/null || true)"
if [ -z "$gc_path" ] && [ -x "$gc_bin" ]; then
  gc_path="$gc_bin"
fi
gc_commit=""
gc_version=""
gc_sha256=""
if [ -n "$gc_path" ] && [ -x "$gc_path" ]; then
  gc_ver_json="$("$gc_path" version --json 2>/dev/null || true)"
  gc_commit="$(printf '%s' "$gc_ver_json" | jq -r '.commit // empty' 2>/dev/null || true)"
  gc_version="$(printf '%s' "$gc_ver_json" | jq -r '.version // empty' 2>/dev/null || true)"
  if command -v sha256sum >/dev/null 2>&1; then
    gc_sha256="$(sha256sum "$gc_path" 2>/dev/null | awk '{print $1}' || true)"
  else
    gc_sha256="$(shasum -a 256 "$gc_path" 2>/dev/null | awk '{print $1}' || true)"
  fi
fi
case "$gc_commit" in
  '' | *[!0-9a-fA-F]*) status_provenance=FAIL ;;
  *)
    if [ "${#gc_sha256}" -eq 64 ]; then
      status_provenance=PASS
    else
      status_provenance=FAIL
    fi
    ;;
esac

# Runs "$@" with output teed (indented) to the human channel and captured to $1;
# returns the command's own exit status so callers get a clean PASS/FAIL signal.
run_step() {
  local log_file="$1"
  shift
  "$@" >"$log_file" 2>&1
  local rc=$?
  sed 's/^/    /' "$log_file" >&3
  return $rc
}

# A {passed,failed,skipped} row for a rung whose counts come from one line.
counts_row() {
  local line="$1" name="$2"
  local counts
  counts="$(printf '%s' "$line" | sed -nE 's/.*: ([0-9]+) passed, ([0-9]+) failed, ([0-9]+) skipped.*/\1 \2 \3/p')"
  if [ -z "$counts" ]; then
    ROW_PASSED=0
    ROW_FAILED=1
    ROW_SKIPPED=0
    ROW_STATUS=FAIL
    return
  fi
  # shellcheck disable=SC2086
  set -- $counts
  ROW_PASSED=$1
  ROW_FAILED=$2
  ROW_SKIPPED=$3
  if [ "$ROW_FAILED" -eq 0 ] && [ "$ROW_PASSED" -gt 0 ]; then
    ROW_STATUS=PASS
  else
    ROW_STATUS=FAIL
  fi
}

# A {passed,failed,skipped} row for a shell rung that has no counts of its own.
binary_row() {
  if [ "$1" = PASS ]; then
    ROW_PASSED=1
    ROW_FAILED=0
    ROW_SKIPPED=0
    ROW_STATUS=PASS
  else
    ROW_PASSED=0
    ROW_FAILED=1
    ROW_SKIPPED=0
    ROW_STATUS=FAIL
  fi
}

row_json() {
  jq -cn --argjson passed "$1" --argjson failed "$2" --argjson skipped "$3" \
    --arg status "$4" '{passed:$passed,failed:$failed,skipped:$skipped,status:$status}'
}

log "receipt: L0-L2 — runtime module (go vet + go test)"
if run_step "$tmp/runtime.log" bash -c 'cd runtime && go vet ./... && go test -count=1 ./...'; then
  l0l2_runtime=PASS
else
  l0l2_runtime=FAIL
fi

log "receipt: L0-L2 — fakenomad module (go vet + go test)"
if run_step "$tmp/fakenomad.log" bash -c 'cd fakenomad && go vet ./... && go test -count=1 ./...'; then
  l0l2_fakenomad=PASS
else
  l0l2_fakenomad=FAIL
fi

log "receipt: L0-L2 — drills module (go vet + go test)"
if run_step "$tmp/drills.log" bash -c 'cd drills && go vet ./... && go test -count=1 ./...'; then
  l0l2_drills=PASS
else
  l0l2_drills=FAIL
fi

# EVERY MODULE IN THE TREE MUST HAVE A LEG ABOVE.
#
# The drills module sat broken from v1.0.7 until 2026-09-12 -- GC_NOMAD_AGENT_IMAGE
# became required and the harness never set it, so every drill failed closed --
# and it stayed broken because this receipt ran two modules out of three. A
# suite that is smaller than the tree it certifies will always eventually
# certify something it never ran, so the mismatch is now itself a failure.
log "receipt: module coverage — every go.mod has a leg"
covered="runtime fakenomad drills"
uncovered=""
for modfile in */go.mod; do
  [ -e "$modfile" ] || continue
  mod=${modfile%/go.mod}
  case " $covered " in
    *" $mod "*) ;;
    *) uncovered="$uncovered $mod" ;;
  esac
done
if [ -n "$uncovered" ]; then
  log "receipt: FAIL — module(s) with no receipt leg:$uncovered"
  status_modcover=FAIL
else
  status_modcover=PASS
fi

if [ "$l0l2_runtime" = PASS ] && [ "$l0l2_fakenomad" = PASS ] && [ "$l0l2_drills" = PASS ] && [ "$status_modcover" = PASS ]; then
  status_l0l2=PASS
else
  status_l0l2=FAIL
fi

log "receipt: secrets-grep (canary receipt — zero planted-canary hits)"
if run_step "$tmp/secrets.log" bash -c 'cd runtime && go test -count=1 -run TestM3StagingReceiptNoCanaryLeak -v ./...'; then
  status_secrets=PASS
else
  status_secrets=FAIL
fi

log "receipt: env probe (env.workspace staging)"
if run_step "$tmp/envprobe.log" bash -c 'cd runtime && go test -count=1 -run TestM3StagingReceiptWorkspaceProbe -v ./...'; then
  status_env=PASS
else
  status_env=FAIL
fi

log "receipt: check + golden suite + retry idempotency (gc runtime check / gc runtime conformance)"
# No `|| true` here: the ladder's exit status is a first-class result (see the
# header). It is captured rather than propagated immediately so the receipt is
# still written — with the failure in it — and this script's own exit is gated
# on it below.
run_step "$tmp/conformance.log" env GC_BIN="$gc_bin" ./conformance.sh
conformance_rc=$?

check_line="$(grep -E '^[0-9]+ checks:' "$tmp/conformance.log" 2>/dev/null | tail -n1 || true)"
golden_line="$(grep -E '^[0-9]+ requirements:' "$tmp/conformance.log" 2>/dev/null | tail -n1 || true)"
counts_row "$check_line" check
check_passed=$ROW_PASSED; check_failed=$ROW_FAILED; check_skipped=$ROW_SKIPPED; status_check=$ROW_STATUS
counts_row "$golden_line" golden
golden_passed=$ROW_PASSED; golden_failed=$ROW_FAILED; golden_skipped=$ROW_SKIPPED; status_golden=$ROW_STATUS

# The retry-idempotency rung is the one the summaries cannot see: it runs after
# gc has printed them. Its success line is the last line conformance.sh writes;
# the start line names the rung, so its absence means the ladder never reached
# it and its presence without a result means the rung failed.
retry_line="$(grep -E '^retry idempotency: [0-9]+ passed, [0-9]+ failed, [0-9]+ skipped' "$tmp/conformance.log" 2>/dev/null | tail -n1 || true)"
if [ -n "$retry_line" ]; then
  counts_row "$retry_line" retry
  if [ "$conformance_rc" -ne 0 ]; then
    ROW_STATUS=FAIL
    retry_failed=$ROW_FAILED
  else
    retry_failed=$ROW_FAILED
  fi
  retry_passed=$ROW_PASSED
  retry_skipped=$ROW_SKIPPED
  status_retry=$ROW_STATUS
elif grep -q 'provision/start retry idempotency' "$tmp/conformance.log" 2>/dev/null; then
  retry_passed=0
  retry_failed=1
  retry_skipped=0
  status_retry=FAIL
else
  retry_passed=0
  retry_failed=0
  retry_skipped=1
  status_retry=SKIPPED
fi

binary_row "$status_env"
env_passed=$ROW_PASSED; env_failed=$ROW_FAILED; env_skipped=$ROW_SKIPPED
binary_row "$status_l0l2"
l0l2_passed=$ROW_PASSED; l0l2_failed=$ROW_FAILED; l0l2_skipped=$ROW_SKIPPED
binary_row "$status_secrets"
secrets_passed=$ROW_PASSED; secrets_failed=$ROW_FAILED; secrets_skipped=$ROW_SKIPPED
binary_row "$status_provenance"
provenance_passed=$ROW_PASSED; provenance_failed=$ROW_FAILED; provenance_skipped=$ROW_SKIPPED

rows_json="$(jq -cn \
  --argjson check "$(row_json "$check_passed" "$check_failed" "$check_skipped" "$status_check")" \
  --argjson golden "$(row_json "$golden_passed" "$golden_failed" "$golden_skipped" "$status_golden")" \
  --argjson retry "$(row_json "$retry_passed" "$retry_failed" "$retry_skipped" "$status_retry")" \
  --argjson env "$(row_json "$env_passed" "$env_failed" "$env_skipped" "$status_env")" \
  --argjson l0l2 "$(row_json "$l0l2_passed" "$l0l2_failed" "$l0l2_skipped" "$status_l0l2")" \
  --argjson secrets "$(row_json "$secrets_passed" "$secrets_failed" "$secrets_skipped" "$status_secrets")" \
  --argjson provenance "$(row_json "$provenance_passed" "$provenance_failed" "$provenance_skipped" "$status_provenance")" \
  '{check:$check,golden:$golden,retry_idempotency:$retry,env_probes:$env,l0l2:$l0l2,secrets_grep:$secrets,provenance:$provenance}')"

overall=PASS
for row_status in "$status_check" "$status_golden" "$status_retry" "$status_env" \
                  "$status_l0l2" "$status_secrets" "$status_provenance"; do
  [ "$row_status" = PASS ] || overall=FAIL
done
# A ladder that exited non-zero can never be reported by a receipt that exits
# zero, whatever its rows happen to read.
[ "$conformance_rc" -eq 0 ] || overall=FAIL

out="$here/CONFORMANCE-RECEIPT.md"
{
  echo "# Phase-1 conformance receipt"
  echo
  echo "Commit: \`$commit\`"
  echo "gc: \`${gc_path:-unresolved}\` — commit \`${gc_commit:-unknown}\`, version \`${gc_version:-unknown}\`, sha256 \`${gc_sha256:-unknown}\`"
  echo "Generated: $ts"
  echo "Conformance ladder exit: $conformance_rc"
  # Citation only (see RECEIPT_RUNNER_LOG in the header): the receipt names a
  # log an adopter can actually open, or says nothing.
  if [ -n "${RECEIPT_RUNNER_LOG:-}" ]; then
    echo "Runner log: \`$RECEIPT_RUNNER_LOG\`"
  fi
  echo
  echo "Full offline ladder — no live Nomad cluster or network required. A row"
  echo "reading FAIL means the ladder is red at this commit; re-run \`./receipt.sh\`"
  echo "after a fix and regenerate this file. Fixing a red ladder step is out of"
  echo "scope for this receipt — bounce back to the owning bead."
  echo
  echo "The rows report what \`gc runtime check\` / \`gc runtime conformance\` said"
  echo "and what the ladder's own rungs did; \`receipt.sh\` exits non-zero if any row"
  echo "is FAIL **or** the ladder exited non-zero. Regenerate both twins after any"
  echo "pin roll or pack release — a receipt whose gc commit no longer matches the"
  echo "pin is stale by construction:"
  echo
  echo '```'
  echo "GC_BIN=\$(command -v gc) ./receipt.sh"
  echo "GC_BIN=\$(command -v gc) ./receipt.sh --json-stdout > CONFORMANCE-RECEIPT.json"
  echo '```'
  echo
  echo "| Ladder step | Result | Evidence |"
  echo "|---|---|---|"
  echo "| check | $status_check | \`gc runtime check\` — ${check_line:-did not run} |"
  echo "| golden suite | $status_golden | \`gc runtime conformance\` — ${golden_line:-did not run} |"
  echo "| retry idempotency | $status_retry | provision/start re-drive rung — ${retry_line:-did not run} |"
  echo "| env probes | $status_env | \`go test -run TestM3StagingReceiptWorkspaceProbe ./runtime\` |"
  echo "| L0-L2 | $status_l0l2 | \`go vet ./... && go test ./...\` — runtime + fakenomad + drills modules, plus a check that every go.mod has a leg |"
  echo "| secrets-grep | $status_secrets | \`go test -run TestM3StagingReceiptNoCanaryLeak ./runtime\` |"
  echo "| provenance | $status_provenance | \`gc version --json\` + sha256 of the resolved GC_BIN |"
} >"$out"

log
log "receipt written to $out"
[ "$json_stdout" = 1 ] || cat "$out"

json="$(jq -cn \
  --arg schema '/2' \
  --arg generated "$ts" \
  --arg pack_commit "$commit" \
  --arg gc_bin "${gc_path:-}" \
  --arg gc_version "$gc_version" \
  --arg gc_commit "$gc_commit" \
  --arg gc_sha256 "$gc_sha256" \
  --arg status "$overall" \
  --argjson conformance_exit "$conformance_rc" \
  --argjson rows "$rows_json" \
  '{schema:$schema,generated:$generated,pack_commit:$pack_commit,gc_bin:$gc_bin,gc_version:$gc_version,gc_commit:$gc_commit,gc_sha256:$gc_sha256,conformance_exit:$conformance_exit,rows:$rows,status:$status}')"

if [ "$json_stdout" = 1 ]; then
  printf '%s\n' "$json"
fi

if [ "$overall" = PASS ]; then
  exit 0
fi
exit 1
