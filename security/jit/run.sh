#!/usr/bin/env bash
# Run Azud's JIT security suite.
#
# Usage:
#   ./security/jit/run.sh                 # run all finding/control tests
#   RACE=1 ./security/jit/run.sh          # also run under the race detector
#   FUZZ=1 FUZZTIME=5s ./security/jit/run.sh
#   AZUD_TEST_BIN=/path/to/azud ./security/jit/run.sh
#
# The suite builds the CLI once into a temporary directory and drives it
# against a local fake SSH server. It never contacts a real host.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT"

CACHE="${AZUD_SECURITY_GOCACHE:-${TMPDIR:-/tmp}/azud-security-gocache}"
mkdir -p "$CACHE"
export GOCACHE="$CACHE"
export GOTMPDIR="${GOTMPDIR:-${TMPDIR:-/tmp}}"

ARGS=(-tags security -count=1 -v)
if [[ "${RACE:-0}" == "1" ]]; then
  ARGS+=(-race)
fi

printf 'AZUD / JIT SECURITY TESTS\n'
printf '%s\n' '--------------------------------------------------------'
printf '  ROOT   %s\n' "$ROOT"
printf '  GOCACHE %s\n' "$GOCACHE"
printf '  TAG    security\n'
printf '\n'

go test "${ARGS[@]}" ./security/jit "$@"

if [[ "${FUZZ:-0}" == "1" ]]; then
  FUZZTIME="${FUZZTIME:-3s}"
  printf '\nAZUD / JIT SECURITY FUZZERS (%s each)\n' "$FUZZTIME"
  printf '%s\n' '--------------------------------------------------------'
  for target in FuzzShellQuotePreservesData FuzzQuadletContainerFileLineInjection; do
    go test -tags security ./security/jit -run '^$' -fuzz "^${target}$" -fuzztime "$FUZZTIME"
  done
fi

printf '\nAll JIT security checks completed. Every finding is now a TestControl_*\n'
printf 'regression test; a failure means a security control regressed.\n'
