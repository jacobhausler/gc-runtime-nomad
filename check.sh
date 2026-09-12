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

  # THE SUITE'S OWN DEPENDENCIES, named before they become 64 failures.
  #
  # MEASURED on a clean machine (a golang container, no tmux): `go test ./...`
  # in runtime/ produced 64 failures, every single one
  # "exec: tmux: not found", from tests that drive the real launch path. Two
  # tests guard for tmux and skip; the other sixty-four do not and fail. A
  # reader seeing that wall has to learn which failures are "expected", which
  # is exactly what gate 7 forbids and exactly how a suite stops being read.
  #
  # So the dependency is stated once, up front, with the count and the remedy,
  # and the suite is not run in a state where its verdict would be noise.
  # Refusing here is honest; printing PASS after skipping the driver tests
  # would be the dark-lane shape one layer up.
  # BOTH modules that drive the real launch path, not just runtime. Scoping
  # this to runtime alone was the third "guard narrower than the thing it
  # guards" mistake of the day: the drills module execs the same launch and
  # failed with the identical cause, so a clean machine still got a wall of
  # failures under a guard that claimed to have named them.
  case "$mod" in
  runtime | drills) needs_tmux=1 ;;
  *) needs_tmux=0 ;;
  esac
  if [ "$needs_tmux" = 1 ] && ! command -v tmux >/dev/null 2>&1; then
    fail "go test — tmux is not on PATH"
    echo "        This module drives the real launch path, which execs tmux:"
    echo "        64 tests fail with 'exec: tmux: not found' on a machine without it,"
    echo "        all from that one cause. That is a missing TEST DEPENDENCY, not 64"
    echo "        defects, and this refuses rather than hand you a wall of failures"
    echo "        to triage."
    echo "        Install tmux (apt-get install tmux / brew install tmux) and re-run."
    echo "        conformance.sh needs no tmux and is unaffected."
    continue
  fi

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
