#!/usr/bin/env bash
# check.sh — gofmt, go vet and go test across EVERY module in this repo.
#
# `go test ./...` in one module is not this repo's suite: there are three
# modules with their own go.mod (runtime, fakenomad, drills), and a module
# nobody runs is a module that breaks silently. The drills module sat broken
# from v1.0.7 until 2026-09-12 for exactly that reason.
#
# gofmt is included deliberately. `go test` never sees formatting or vet, and
# both have caught real defects in this repo that the tests did not.
#
# Usage: ./check.sh
set -uo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

status=0
fail() { printf '  FAIL  %s\n' "$1"; status=1; }
ok()   { printf '  ok    %s\n' "$1"; }

mods=()
for modfile in */go.mod; do
  [ -e "$modfile" ] || continue
  mods+=("${modfile%/go.mod}")
done
if [ "${#mods[@]}" -eq 0 ]; then
  echo "check: no Go modules found — is this the repo root?" >&2
  exit 2
fi

echo "check: modules: ${mods[*]}"
for mod in "${mods[@]}"; do
  echo "check: $mod"
  unformatted=$(cd "$mod" && gofmt -l . 2>/dev/null)
  if [ -n "$unformatted" ]; then
    fail "gofmt — $(echo "$unformatted" | tr '\n' ' ')"
  else
    ok "gofmt"
  fi

  if (cd "$mod" && go vet ./... >/tmp/check-vet.$$ 2>&1); then
    ok "go vet"
  else
    fail "go vet"
    sed 's/^/        /' /tmp/check-vet.$$ | head -20
  fi
  rm -f /tmp/check-vet.$$

  # -v so skips are visible. A suite that quietly skips its driver tests on a
  # machine without tmux reports "ok" while asserting nothing — the same
  # dark-lane shape the runtime itself is built to refuse, one layer up.
  if (cd "$mod" && go test -count=1 -v ./... >/tmp/check-test.$$ 2>&1); then
    skipped=$(grep -cE '^\s*--- SKIP' /tmp/check-test.$$ || true)
    if [ "${skipped:-0}" -gt 0 ]; then
      ok "go test ($skipped skipped)"
      grep -E '^\s*--- SKIP' /tmp/check-test.$$ | sed 's/^/        /' | head -10
    else
      ok "go test"
    fi
  else
    fail "go test"
    grep -E '^(---|\s+---) FAIL|^FAIL' /tmp/check-test.$$ | head -20 | sed 's/^/        /'
  fi
  rm -f /tmp/check-test.$$
done

if [ "$status" -eq 0 ]; then
  echo "check: PASS — every module clean"
else
  echo "check: FAIL — see above" >&2
fi
exit "$status"
