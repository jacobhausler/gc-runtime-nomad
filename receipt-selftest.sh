#!/usr/bin/env bash
# Fail-closed regression for the conformance receipt (cr-u7b0s.48).
#
# The distinguishing case is NOT a gc that fails loudly. With a stub GC_BIN that
# prints nothing, receipt.sh's prose-derived rows already read FAIL and it
# already exits non-zero, so that test passes before the fix and proves nothing.
# The bug was a GC_BIN stub that prints green RPP summaries and THEN exits
# non-zero: `receipt.sh` ran `./conformance.sh || true`, derived both rows by
# grepping the summaries, printed them as PASS, and exited 0 — a green published
# receipt over a red ladder.
#
#   case A (red)    stub prints "14 checks: … 0 failed" (exit 0) and
#                   "8 requirements: … 0 failed" then exits 3
#                   -> receipt MUST exit non-zero, and its JSON MUST show both
#                      summaries green (proving this is the silent case) while
#                      conformance_exit is 3 and status is FAIL.
#   case B (control) the same summaries, exit 0
#                   -> receipt MUST exit 0 and MUST emit the machine-readable
#                      contract: 40-hex pack_commit, gc_commit, 64-hex
#                      gc_sha256, a {passed,failed,skipped,status} row per item,
#                      and a retry-idempotency rung that actually ran.
#
# Go is built here, so this runs off-Mac (AGENTS.md):
#   /Users/home/westlands/ops/bin/nomad-run --repo . \
#     --rev "$(git rev-parse HEAD)" -- ./receipt-selftest.sh
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

case "$here" in
  '' | "$work"/*) printf 'selftest: refusing to copy an unexpected pack root %s\n' "$here" >&2; exit 2 ;;
esac

cat >"$work/gc-stub.sh" <<'STUB'
#!/usr/bin/env bash
# Prints green RPP summaries, then exits $STUB_RC on the conformance verb — the
# green-summary-but-red-exit case the old `|| true` swallowed.
if [ "${1:-}" = version ]; then
  printf '{"version":"selftest","commit":"0000000000000000000000000000000000000000"}\n'
  exit 0
fi
if [ "${1:-}" = runtime ] && [ "${2:-}" = check ]; then
  printf '\n14 checks: 14 passed, 0 failed, 0 skipped\n'
  exit 0
fi
if [ "${1:-}" = runtime ] && [ "${2:-}" = conformance ]; then
  printf '\n8 requirements: 8 passed, 0 failed, 0 skipped\n'
  exit "${STUB_RC:-0}"
fi
exit 0
STUB
chmod +x "$work/gc-stub.sh"

fail=0

run_case() {
  local label="$1" stub_rc="$2"
  local dst="$work/$label"
  cp -a "$here" "$dst"
  ( cd "$dst" && STUB_RC="$stub_rc" GC_BIN="$work/gc-stub.sh" ./receipt.sh --json-stdout \
      >"$work/$label.json" 2>"$work/$label.log" )
  CASE_RC=$?
}

printf 'selftest: case A — green summaries, red ladder (stub exit 3)\n'
run_case red 3
if [ "$CASE_RC" -eq 0 ]; then
  printf 'SELFTEST FAIL: a red conformance ladder still produced receipt exit 0\n' >&2
  fail=1
fi
if ! jq -e '(.rows.check.status == "PASS") and (.rows.golden.status == "PASS")
            and (.conformance_exit == 3) and (.status == "FAIL")' \
    "$work/red.json" >/dev/null 2>&1; then
  printf 'SELFTEST FAIL: case A JSON is not the silent-red shape (rows green, conformance_exit 3, status FAIL):\n' >&2
  sed -n '1,40p' "$work/red.json" >&2
  tail -n 30 "$work/red.log" >&2
  fail=1
fi

printf 'selftest: case B — green summaries, green ladder (stub exit 0)\n'
run_case green 0
if [ "$CASE_RC" -ne 0 ]; then
  printf 'SELFTEST FAIL: a green ladder produced receipt exit %s\n' "$CASE_RC" >&2
  tail -n 40 "$work/green.log" >&2
  fail=1
fi
if ! jq -e '(.schema == "nrt-p1-90/2")
            and (.pack_commit | test("^[0-9a-f]{40}$"))
            and (.gc_commit == "0000000000000000000000000000000000000000")
            and (.gc_sha256 | test("^[0-9a-f]{64}$"))
            and (.conformance_exit == 0)
            and (.rows.check.failed == 0)
            and (.rows.golden.failed == 0)
            and (.rows.retry_idempotency.passed == 1)
            and (.rows.retry_idempotency.failed == 0)
            and (.rows.retry_idempotency.skipped == 0)
            and (.rows.provenance.status == "PASS")
            and (.status == "PASS")' \
    "$work/green.json" >/dev/null 2>&1; then
  printf 'SELFTEST FAIL: case B JSON does not satisfy the field contract:\n' >&2
  sed -n '1,40p' "$work/green.json" >&2
  tail -n 30 "$work/green.log" >&2
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  printf 'RECEIPT-SELFTEST-OK\n'
  exit 0
fi
exit 1
