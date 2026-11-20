#!/usr/bin/env bash
# Repeated unit-test runner for the 1.0 stability gate. See docs/testing.md.
#
#   hack/stress.sh [count] [packages...]
#
# count defaults to 20. The packages default to the ones whose tests run real
# processes, sockets, and stores, so a timing flake shows up here instead of in
# a release. Each package runs with -race, -count=<count>, and a fixed -shuffle
# seed, so the order of a failed run is reproducible.
#
# The script stops at the first failing package and prints its seed.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

COUNT="${1:-20}"
if [[ $# -gt 0 ]]; then
  shift
fi
if [[ $# -gt 0 ]]; then
  PACKAGES=("$@")
else
  PACKAGES=(
    ./internal/pluginhost
    ./internal/queue
    ./internal/session
    ./internal/httpapi
    ./internal/gateway
    ./internal/pluginsvc
    ./hack/loadtest
    ./test/integration
  )
fi

if ! [[ "$COUNT" =~ ^[1-9][0-9]*$ ]]; then
  echo "stress: count must be a positive integer, got $COUNT" >&2
  exit 2
fi

started=$SECONDS
for pkg in "${PACKAGES[@]}"; do
  # A fresh seed per package: -shuffle keeps the test order of one package.
  seed=$(( (RANDOM << 15) ^ RANDOM ))
  printf '\n==> %s (count %s, seed %s)\n' "$pkg" "$COUNT" "$seed"
  if ! go test -race -count="$COUNT" -shuffle="$seed" -timeout 30m "$pkg"; then
    printf '\nFAILED %s after %ss\n' "$pkg" "$((SECONDS - started))" >&2
    printf 'rerun: go test -race -count=%s -shuffle=%s -timeout 30m %s\n' "$COUNT" "$seed" "$pkg" >&2
    exit 1
  fi
done
printf '\nPASS: %d packages at count %s in %ss\n' "${#PACKAGES[@]}" "$COUNT" "$((SECONDS - started))"
