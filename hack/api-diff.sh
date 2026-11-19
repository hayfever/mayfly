#!/usr/bin/env bash
# Fail when a change to api/openapi.yaml breaks an existing client.
#
# Usage: hack/api-diff.sh [base-ref]
#
# The base ref defaults to $API_DIFF_BASE, then to the newest v* tag. When the
# base has no spec yet, the check prints a notice and passes.
set -euo pipefail

# Pinned so the gate and a local run report the same changes.
oasdiff_version="v1.39.0"
spec="api/openapi.yaml"

cd "$(git rev-parse --show-toplevel)"

base="${1:-${API_DIFF_BASE:-}}"
if [ -z "$base" ]; then
  base="$(git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"
fi
if [ -z "$base" ]; then
  echo "api-diff: no v* tag found; skipping the spec comparison" >&2
  exit 0
fi
if ! git rev-parse --verify --quiet "$base^{commit}" >/dev/null; then
  echo "api-diff: base ref $base does not exist" >&2
  exit 2
fi
if ! git cat-file -e "$base:$spec" 2>/dev/null; then
  echo "api-diff: $base has no $spec; skipping the spec comparison" >&2
  exit 0
fi

echo "api-diff: comparing $spec at $base with the working tree"
go run "github.com/oasdiff/oasdiff@$oasdiff_version" breaking \
  --fail-on ERR \
  --color never \
  "$base:$spec" "$spec"
