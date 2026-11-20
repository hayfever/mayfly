#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CLUSTER="${CLUSTER:-mayfly}"
NAMESPACE="${NAMESPACE:-mayfly}"
CONTEXT="k3d-$CLUSTER"

command -v k3d >/dev/null || { echo "k3d is required" >&2; exit 1; }
command -v kubectl >/dev/null || { echo "kubectl is required" >&2; exit 1; }
command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }

echo "==> building images"
make image

if ! k3d cluster list 2>/dev/null | awk 'NR > 1 { print $1 }' | grep -qx "$CLUSTER"; then
  echo "==> creating k3d cluster $CLUSTER"
  # Leave the current kubeconfig context alone: the quickstart should not
  # change what the user's kubectl points at.
  k3d cluster create "$CLUSTER" --config deploy/k3d.yaml --wait --kubeconfig-switch-context=false
else
  echo "==> reusing k3d cluster $CLUSTER"
  # A reused cluster can be missing from this kubeconfig (another KUBECONFIG,
  # a deleted context); merge it back without switching.
  if ! kubectl config get-contexts -o name 2>/dev/null | grep -qx "$CONTEXT"; then
    k3d kubeconfig merge "$CLUSTER" -d --kubeconfig-switch-context=false
  fi
fi

echo "==> importing images into k3d"
k3d image import --cluster "$CLUSTER" \
  mayflyd:dev mayfly-llmstub:dev mayfly-mcpstub:dev mayfly-agent:dev

kubectl --context "$CONTEXT" create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl --context "$CONTEXT" apply -f - >/dev/null
mkdir -p .mayfly
if [[ -n "${MAYFLY_API_TOKEN:-}" ]]; then
  TOKEN="$MAYFLY_API_TOKEN"
elif [[ -f .mayfly/admin-token ]]; then
  TOKEN="$(cat .mayfly/admin-token)"
else
  TOKEN="$(openssl rand -hex 24)"
fi
printf '%s' "$TOKEN" > .mayfly/admin-token
chmod 600 .mayfly/admin-token

kubectl --context "$CONTEXT" -n "$NAMESPACE" create secret generic mayfly-secrets \
  --from-literal=MAYFLY_API_TOKEN="$TOKEN" \
  --from-literal=MAYFLY_WEBHOOK_SECRET="$(openssl rand -hex 24)" \
  --from-literal=OPENAI_API_KEY="${OPENAI_API_KEY:-}" \
  --dry-run=client -o yaml | kubectl --context "$CONTEXT" apply -f - >/dev/null

echo "==> applying manifests"
kubectl --context "$CONTEXT" kustomize --load-restrictor=LoadRestrictionsNone deploy/k8s/overlays/local | kubectl --context "$CONTEXT" apply -f -

kubectl --context "$CONTEXT" -n "$NAMESPACE" rollout restart deployment/mayflyd deployment/llmstub deployment/mcpstub >/dev/null
kubectl --context "$CONTEXT" -n "$NAMESPACE" rollout status deployment/mayflyd --timeout=180s
kubectl --context "$CONTEXT" -n "$NAMESPACE" rollout status deployment/llmstub --timeout=120s
kubectl --context "$CONTEXT" -n "$NAMESPACE" rollout status deployment/mcpstub --timeout=120s

cat <<EOF

Mayfly is running.
  URL:     http://localhost:8080
  Token:   $TOKEN
  Context: $CONTEXT (not selected; pass --context $CONTEXT)

Use it:
  export MAYFLY_URL=http://localhost:8080
  export MAYFLY_API_TOKEN=$TOKEN
  ./bin/mayfly run --prompt "Investigate the latest deploy failure"

EOF
