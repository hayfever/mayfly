#!/usr/bin/env bash
# End-to-end suite. See docs/testing.md.
#
#   hack/e2e.sh [all|up|test|down]     (default: all = up, test, down)
#
# By default it creates a k3d cluster, builds and imports local images, and
# installs the local Helm chart. Set E2E_KUBECONTEXT to run against an existing
# cluster instead; images are then pushed to E2E_REGISTRY.
#
# MAYFLY_E2E_UPGRADE_FROM and MAYFLY_E2E_PULL_SECRET reach the tests unchanged
# and enable the upgrade test; docs/testing.md describes that run.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# shellcheck source=hack/lib/k3d-images.sh
. "$ROOT/hack/lib/k3d-images.sh"

CLUSTER="${E2E_CLUSTER:-mayfly-e2e}"
NAMESPACE="${E2E_NAMESPACE:-mayfly-e2e}"
RELEASE="${E2E_RELEASE:-mayfly}"
QUEUE="${E2E_QUEUE:-bbolt}"                 # bbolt or redis
KEEP="${E2E_KEEP:-0}"                       # 1 keeps the cluster after "all"
ARTIFACTS="${E2E_ARTIFACTS:-$ROOT/.e2e}"    # logs and diagnostics
GO_TEST_FLAGS="${E2E_GO_TEST_FLAGS:-}"      # e.g. -run TestNetworkIsolation
SKIP_BUILD="${E2E_SKIP_BUILD:-0}"           # 1: images are already built (k3d) or pushed (E2E_REGISTRY)
VERSION="${VERSION:-0.1.0}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"

if [[ -n "${E2E_KUBECONTEXT:-}" ]]; then
  MODE=existing
  CONTEXT="$E2E_KUBECONTEXT"
  REGISTRY="${E2E_REGISTRY:?E2E_REGISTRY is required with E2E_KUBECONTEXT (images are pushed there)}"
  TAG="${E2E_TAG:-e2e-$COMMIT}"
  PREFIX="$REGISTRY/"
else
  MODE=k3d
  CONTEXT="k3d-$CLUSTER"
  TAG="${E2E_TAG:-e2e}"
  PREFIX=""
fi
IMG_MAYFLYD="${PREFIX}mayflyd:$TAG"
IMG_AGENT="${PREFIX}mayfly-agent:$TAG"
IMG_LLMSTUB="${PREFIX}mayfly-llmstub:$TAG"
IMG_MCPSTUB="${PREFIX}mayfly-mcpstub:$TAG"
IMG_PLUGIN="${PREFIX}mayfly-e2e-plugin:$TAG"
IMG_PLUGIN_MISSING="${PREFIX}mayfly-e2e-plugin-missing:$TAG"

log() { printf '\n==> %s\n' "$*"; }
die() { echo "e2e: $*" >&2; exit 1; }
k() { kubectl --context "$CONTEXT" "$@"; }
kn() { kubectl --context "$CONTEXT" -n "$NAMESPACE" "$@"; }

CONTROL_PLANE_SELECTOR="app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=control-plane"
LOG_STREAM_PID=""
LOG_STREAM_PIDFILE=""

# stop_log_stream_children ends the kubectl log processes the watcher started.
stop_log_stream_children() {
  if [[ -n "$LOG_STREAM_PIDFILE" && -f "$LOG_STREAM_PIDFILE" ]]; then
    while read -r pid; do
      if [[ -n "$pid" ]]; then
        kill "$pid" 2>/dev/null || true
      fi
    done <"$LOG_STREAM_PIDFILE"
  fi
}

# start_log_streams follows every control-plane pod into $ARTIFACTS/pods. The
# restart and upgrade tests delete the control-plane pod, and a deleted pod's
# log is gone with it, so each pod is streamed from its first line for as long
# as it exists. The streams only write under $ARTIFACTS and cannot change the
# suite's outcome; stop_log_streams ends them on every exit path.
start_log_streams() {
  local dir="$ARTIFACTS/pods"
  local seen="$dir/.seen"
  mkdir -p "$dir"
  : >"$seen"
  LOG_STREAM_PIDFILE="$dir/.pids"
  : >"$LOG_STREAM_PIDFILE"
  (
    trap 'stop_log_stream_children; exit 0' TERM INT
    while :; do
      pods="$(kn get pods -l "$CONTROL_PLANE_SELECTOR" \
        -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)"
      # shellcheck disable=SC2086 # the jsonpath output is one name per line.
      for pod in $pods; do
        if ! grep -qxF "$pod" "$seen"; then
          printf '%s\n' "$pod" >>"$seen"
          # Call kubectl directly, not through kn: $! must name the streaming
          # process so stop_log_stream_children can end it.
          kubectl --context "$CONTEXT" -n "$NAMESPACE" logs -f "$pod" --all-containers --tail=-1 \
            >"$dir/$pod.log" 2>&1 &
          printf '%s\n' "$!" >>"$LOG_STREAM_PIDFILE"
        fi
      done
      sleep 2
    done
  ) &
  LOG_STREAM_PID=$!
}

stop_log_streams() {
  if [[ -n "$LOG_STREAM_PID" ]]; then
    kill "$LOG_STREAM_PID" 2>/dev/null || true
    wait "$LOG_STREAM_PID" 2>/dev/null || true
    LOG_STREAM_PID=""
  fi
  stop_log_stream_children
  if [[ -n "$LOG_STREAM_PIDFILE" ]]; then
    rm -f "$LOG_STREAM_PIDFILE"
  fi
}

need() {
  for tool in "$@"; do
    command -v "$tool" >/dev/null || die "$tool is required"
  done
}

build_images() {
  log "building images ($TAG)"
  local args=(--build-arg "VERSION=$VERSION" --build-arg "COMMIT=$COMMIT" --build-arg "DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)")
  docker build "${args[@]}" --target mayflyd -t "$IMG_MAYFLYD" .
  docker build "${args[@]}" --target mayfly-agent -t "$IMG_AGENT" .
  docker build "${args[@]}" --target llmstub -t "$IMG_LLMSTUB" .
  docker build "${args[@]}" --target mcpstub -t "$IMG_MCPSTUB" .
  docker build --target plugin -t "$IMG_PLUGIN" e2e/plugins
  docker build --target missing -t "$IMG_PLUGIN_MISSING" e2e/plugins
}

create_cluster() {
  if k3d cluster list 2>/dev/null | awk 'NR > 1 { print $1 }' | grep -qx "$CLUSTER"; then
    log "reusing k3d cluster $CLUSTER"
    # A reused cluster can be missing from this kubeconfig (another
    # KUBECONFIG, a deleted context); merge it back without touching the
    # user's current context.
    if ! kubectl config get-contexts -o name 2>/dev/null | grep -qx "$CONTEXT"; then
      k3d kubeconfig merge "$CLUSTER" -d --kubeconfig-switch-context=false
    fi
  else
    log "creating k3d cluster $CLUSTER"
    k3d cluster create "$CLUSTER" --config e2e/k3d.yaml --wait --kubeconfig-switch-context=false
  fi
  log "waiting for nodes"
  k wait --for=condition=Ready nodes --all --timeout=300s
  log "importing images into k3d"
  mkdir -p "$ARTIFACTS"
  if ! k3d_import_images "$CLUSTER" \
    "$IMG_MAYFLYD" "$IMG_AGENT" "$IMG_LLMSTUB" "$IMG_MCPSTUB" \
    "$IMG_PLUGIN" "$IMG_PLUGIN_MISSING"; then
    k3d_node_image_report "$CLUSTER" >"$ARTIFACTS/k3d-images.txt" 2>&1 || true
    die "image import failed; per-node image list in $ARTIFACTS/k3d-images.txt"
  fi
}

push_images() {
  log "pushing images to $REGISTRY"
  for img in "$IMG_MAYFLYD" "$IMG_AGENT" "$IMG_LLMSTUB" "$IMG_MCPSTUB" \
    "$IMG_PLUGIN" "$IMG_PLUGIN_MISSING"; do
    docker push "$img"
  done
}

deploy() {
  log "deploying $RELEASE to $CONTEXT/$NAMESPACE (queue: $QUEUE)"
  k create namespace "$NAMESPACE" --dry-run=client -o yaml | k apply -f - >/dev/null
  if ! kn get secret mayfly-e2e-secrets >/dev/null 2>&1; then
    kn create secret generic mayfly-e2e-secrets \
      --from-literal=MAYFLY_API_TOKEN="$(openssl rand -hex 24)" \
      --from-literal=MAYFLY_WEBHOOK_SECRET="$(openssl rand -hex 24)" \
      --from-literal=MAYFLY_SLACK_SECRET="$(openssl rand -hex 24)" \
      --from-literal=OPENAI_API_KEY=stub >/dev/null
  fi
  sed -e "s|LLMSTUB_IMAGE|$IMG_LLMSTUB|" -e "s|MCPSTUB_IMAGE|$IMG_MCPSTUB|" e2e/manifests/stubs.yaml | kn apply -f - >/dev/null
  sed -e "s|E2E_NAMESPACE|$NAMESPACE|" e2e/manifests/deny-pods.yaml | k apply -f - >/dev/null

  local queue_args=()
  if [[ "$QUEUE" == redis ]]; then
    kn apply -f e2e/manifests/redis.yaml >/dev/null
    kn rollout status deployment/redis --timeout=180s
    queue_args=(--set config.queue.backend=redis --set config.queue.redis.url=redis://redis:6379)
  fi
  kn rollout status deployment/llmstub --timeout=180s
  kn rollout status deployment/mcpstub --timeout=180s

  # The plugin manifests carry the fixture image through a placeholder, so the
  # same files work on k3d (bare image name) and against a registry.
  local plugin_dir="$ARTIFACTS/plugins" name
  mkdir -p "$plugin_dir"
  for name in inline sidecar counter missing; do
    sed -e "s|PLUGIN_MISSING_IMAGE|$IMG_PLUGIN_MISSING|g" \
      -e "s|PLUGIN_IMAGE|$IMG_PLUGIN|g" \
      "e2e/plugins/$name.plugin.yaml" >"$plugin_dir/$name.plugin.yaml"
  done

  helm --kube-context "$CONTEXT" upgrade --install "$RELEASE" deploy/helm/mayfly \
    --namespace "$NAMESPACE" \
    -f e2e/values.yaml \
    --set image.repository="${IMG_MAYFLYD%:*}" --set image.tag="$TAG" \
    --set agentImage.repository="${IMG_AGENT%:*}" --set agentImage.tag="$TAG" \
    --set-file 'skills.files.incident-triage\.md=examples/skills/incident-triage.md' \
    --set-file 'agents.files.e2e\.yaml=e2e/agents.yaml' \
    --set-file "agents.files.inline\\.plugin\\.yaml=$plugin_dir/inline.plugin.yaml" \
    --set-file "agents.files.sidecar\\.plugin\\.yaml=$plugin_dir/sidecar.plugin.yaml" \
    --set-file "agents.files.counter\\.plugin\\.yaml=$plugin_dir/counter.plugin.yaml" \
    --set-file "agents.files.missing\\.plugin\\.yaml=$plugin_dir/missing.plugin.yaml" \
    "${queue_args[@]}" \
    --wait --timeout 5m
  # A reused cluster keeps the pod when only the image content changed.
  kn rollout restart "deployment/$RELEASE" >/dev/null
  kn rollout status "deployment/$RELEASE" --timeout=180s
}

up() {
  need kubectl helm openssl
  if [[ "$SKIP_BUILD" != 1 ]]; then
    need docker
    build_images
  fi
  if [[ "$MODE" == k3d ]]; then
    need k3d docker jq
    create_cluster
  elif [[ "$SKIP_BUILD" != 1 ]]; then
    push_images
  fi
  deploy
}

diagnostics() {
  mkdir -p "$ARTIFACTS"
  log "collecting diagnostics in $ARTIFACTS"
  kn get all,networkpolicies,secrets,configmaps,pvc -o wide >"$ARTIFACTS/resources.txt" 2>&1 || true
  kn get events --sort-by=.lastTimestamp >"$ARTIFACTS/events.txt" 2>&1 || true
  kn describe pods >"$ARTIFACTS/pods.txt" 2>&1 || true
  kn logs "deployment/$RELEASE" --all-containers --tail=-1 >"$ARTIFACTS/mayflyd.log" 2>&1 || true
  kn logs "deployment/$RELEASE" --all-containers --previous --tail=-1 >"$ARTIFACTS/mayflyd.previous.log" 2>&1 || true
  if [[ "$MODE" == k3d ]]; then
    printf '\n==> k3d node images\n'
    k3d_node_image_report "$CLUSTER" 2>&1 | tee "$ARTIFACTS/k3d-images.txt" || true
  fi
}

run_tests() {
  need go kubectl
  mkdir -p "$ARTIFACTS"
  start_log_streams
  log "running the e2e suite against $CONTEXT/$NAMESPACE"
  # shellcheck disable=SC2086
  # E2E_ARTIFACTS reaches the suite, which copies each kubectl port-forward's
  # stderr into it, so a dropped call is provable after the run.
  if MAYFLY_E2E_KUBECONTEXT="$CONTEXT" MAYFLY_E2E_NAMESPACE="$NAMESPACE" MAYFLY_E2E_RELEASE="$RELEASE" \
    E2E_ARTIFACTS="$ARTIFACTS" \
    MAYFLY_E2E_PLUGIN_IMAGE="$IMG_PLUGIN" MAYFLY_E2E_PLUGIN_MISSING_IMAGE="$IMG_PLUGIN_MISSING" \
    go test -tags e2e -count=1 -timeout 30m -v $GO_TEST_FLAGS ./e2e 2>&1 | tee "$ARTIFACTS/go-test.log"; then
    kn logs "deployment/$RELEASE" --tail=-1 >"$ARTIFACTS/mayflyd.log" 2>&1 || true
    stop_log_streams
    return 0
  fi
  diagnostics
  stop_log_streams
  return 1
}

down() {
  if [[ "$MODE" == k3d ]]; then
    log "deleting k3d cluster $CLUSTER"
    k3d cluster delete "$CLUSTER"
  else
    log "removing $RELEASE and $NAMESPACE from $CONTEXT"
    helm --kube-context "$CONTEXT" uninstall "$RELEASE" -n "$NAMESPACE" --wait || true
    k delete namespace "$NAMESPACE" --wait=true || true
    k delete validatingadmissionpolicybinding,validatingadmissionpolicy mayfly-e2e-deny-pods --ignore-not-found
  fi
}

case "${1:-all}" in
  up) up ;;
  test)
    trap stop_log_streams EXIT
    trap 'exit 130' INT TERM
    run_tests
    ;;
  down) down ;;
  all)
    if [[ "$KEEP" != 1 ]]; then
      trap 'stop_log_streams; down' EXIT
    else
      trap stop_log_streams EXIT
    fi
    trap 'exit 130' INT TERM
    up
    run_tests
    ;;
  *) die "usage: $0 [all|up|test|down]" ;;
esac
