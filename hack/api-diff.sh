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
  # Version sort ranks v0.2.0 above v0.2.0-rc.1; git describe would pick by
  # commit ancestry instead.
  base="$(git -c versionsort.suffix=-rc tag --list 'v*' --sort=-version:refname | head -n 1)"
fi
if [ -z "$base" ]; then
  echo "api-diff: no v* tag found; skipping the spec comparison" >&2
  exit 0
fi
if ! git rev-parse --verify --quiet "$base^{commit}" >/dev/null; then
  echo "api-diff: base ref $base does not exist" >&2
  exit 2
fi
# The job envelope is versioned: v1 may gain optional fields, but an existing
# field may not change shape or meaning. A breaking change adds
# api/job.v2.schema.json instead of editing v1.
schema="api/job.v1.schema.json"
if ! git cat-file -e "$base:$schema" 2>/dev/null; then
  echo "api-diff: $base has no $schema; skipping the job envelope comparison" >&2
elif ! git show "$base:$schema" | cmp -s - "$schema"; then
  echo "api-diff: $schema changed from $base" >&2
  echo "api-diff: add a new schema version for a breaking envelope change" >&2
  exit 1
fi
if ! git cat-file -e "$base:$spec" 2>/dev/null; then
  # Before the first tag carries a spec there is no baseline to fail on, but
  # the intended change must still be visible in the pull request. Report it
  # against origin/main without failing: a pre-1.0 release may break a client
  # when the PR lists the break.
  if git rev-parse --verify --quiet "origin/main:$spec" >/dev/null; then
    echo "api-diff: $base has no $spec; reporting changes against origin/main" >&2
    go run "github.com/oasdiff/oasdiff@$oasdiff_version" breaking \
      --color never \
      "origin/main:$spec" "$spec" || true
    exit 0
  fi
  echo "api-diff: $base has no $spec; skipping the spec comparison" >&2
  exit 0
fi

echo "api-diff: comparing $spec at $base with the working tree"
go run "github.com/oasdiff/oasdiff@$oasdiff_version" breaking \
  --fail-on ERR \
  --color never \
  "$base:$spec" "$spec"
