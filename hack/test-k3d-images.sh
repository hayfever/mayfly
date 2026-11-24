#!/usr/bin/env bash
# Fixture checks for the k3d image matcher. No k3d, docker, or network.
#
#   ./hack/test-k3d-images.sh
#
# Each crictl fixture stands for one node's `crictl images -o json` output.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=hack/lib/k3d-images.sh
. "$ROOT/hack/lib/k3d-images.sh"

checks=0
failures=0

pass() {
  checks=$((checks + 1))
  printf 'ok   %s\n' "$1"
}

fail() {
  checks=$((checks + 1))
  failures=$((failures + 1))
  printf 'FAIL %s: %s\n' "$1" "$2"
}

expect_present() {
  local name=$1 json=$2 expected=$3 status=0
  k3d_image_present "$expected" "$json" || status=$?
  if ((status == 0)); then
    pass "$name"
  else
    fail "$name" "expected $expected present, exit $status"
  fi
}

expect_absent() {
  local name=$1 json=$2 expected=$3 status=0
  k3d_image_present "$expected" "$json" || status=$?
  if ((status == 1)); then
    pass "$name"
  else
    fail "$name" "expected $expected absent, exit $status"
  fi
}

expect_error() {
  local name=$1 json=$2 expected=$3 status=0
  # The matcher reports the parse error on stderr; this case expects it.
  k3d_image_present "$expected" "$json" 2>/dev/null || status=$?
  if ((status == 2)); then
    pass "$name"
  else
    fail "$name" "expected unreadable JSON, exit $status"
  fi
}

# One node with the short name that a local docker build produces.
node_short='{"images":[{"id":"sha256:1111","repoTags":["mayflyd:e2e"],"repoDigests":[]}]}'
# One node with containerd's docker.io/library normalization.
node_library='{"images":[{"id":"sha256:2222","repoTags":["docker.io/library/mayflyd:e2e"],"repoDigests":["docker.io/library/mayflyd@sha256:aaaa"]}]}'
# Same node shape, but the other tag.
node_dev='{"images":[{"id":"sha256:3333","repoTags":["docker.io/library/mayflyd:dev"]}]}'
node_empty='{"images":[]}'
node_null='{"images":null}'
node_digest_only='{"images":[{"id":"sha256:4444","repoTags":[],"repoDigests":["docker.io/library/mayflyd@sha256:bbbb"]}]}'
node_ghcr='{"images":[{"id":"sha256:5555","repoTags":["ghcr.io/hayfever/mayflyd:e2e"]}]}'
node_other_namespace='{"images":[{"id":"sha256:6666","repoTags":["docker.io/other/mayflyd:e2e"]}]}'
node_local_registry='{"images":[{"id":"sha256:7777","repoTags":["localhost:5000/mayflyd:e2e"]}]}'
node_two_images='{"images":[{"repoTags":["docker.io/library/mayflyd:dev"]},{"repoTags":["docker.io/library/mayflyd:e2e"]}]}'
node_broken='{"images": ['

expect_present "short name is present" "$node_short" "mayflyd:e2e"
expect_present "docker.io/library name matches a short request" \
  "$node_library" "mayflyd:e2e"
expect_present "a fully qualified request matches a short name" \
  "$node_short" "docker.io/library/mayflyd:e2e"
expect_present "a namespaced registry name is present" \
  "$node_ghcr" "ghcr.io/hayfever/mayflyd:e2e"
expect_present "a registry with a port is present" \
  "$node_local_registry" "localhost:5000/mayflyd:e2e"
expect_present "the requested image is the second entry" \
  "$node_two_images" "mayflyd:e2e"

expect_absent "another tag is not a match" "$node_dev" "mayflyd:e2e"
expect_absent "an empty image list has no match" "$node_empty" "mayflyd:e2e"
expect_absent "a null image list has no match" "$node_null" "mayflyd:e2e"
expect_absent "a digest-only entry is not a tag match" \
  "$node_digest_only" "mayflyd:e2e"
expect_absent "another namespace is not a match" \
  "$node_other_namespace" "mayflyd:e2e"

# Two nodes: the matcher is per node, so the same request must be present on
# one node and absent on the other.
expect_present "node a has the image" "$node_library" "mayflyd:e2e"
expect_absent "node b is missing the image" "$node_dev" "mayflyd:e2e"

expect_error "invalid JSON is an error, not a missing image" \
  "$node_broken" "mayflyd:e2e"

printf '\n%d checks, %d failure(s)\n' "$checks" "$failures"
if ((failures > 0)); then
  exit 1
fi
