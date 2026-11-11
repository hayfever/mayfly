#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CLUSTER="${CLUSTER:-mayfly}"
NAMESPACE="${NAMESPACE:-mayfly}"

command -v kind >/dev/null || { echo "kind is required" >&2; exit 1; }
command -v kubectl >/dev/null || { echo "kubectl is required" >&2; exit 1; }
command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }

echo "==> building images"
make image

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "==> creating kind cluster $CLUSTER"
  kind create cluster --config deploy/kind.yaml
fi

echo "==> loading images into kind"
kind load docker-image --name "$CLUSTER" \
  mayflyd:dev mayfly-llmstub:dev mayfly-mcpstub:dev mayfly-agent:dev

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
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

kubectl -n "$NAMESPACE" create secret generic mayfly-secrets \
  --from-literal=MAYFLY_API_TOKEN="$TOKEN" \
  --from-literal=MAYFLY_WEBHOOK_SECRET="$(openssl rand -hex 24)" \
  --from-literal=OPENAI_API_KEY="${OPENAI_API_KEY:-}" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

echo "==> applying manifests"
kubectl kustomize --load-restrictor=LoadRestrictionsNone deploy/k8s/overlays/kind | kubectl apply -f -

kubectl -n "$NAMESPACE" rollout restart deployment/mayflyd deployment/llmstub deployment/mcpstub >/dev/null
kubectl -n "$NAMESPACE" rollout status deployment/mayflyd --timeout=180s
kubectl -n "$NAMESPACE" rollout status deployment/llmstub --timeout=120s
kubectl -n "$NAMESPACE" rollout status deployment/mcpstub --timeout=120s

cat <<EOF

Mayfly is running.
  URL:   http://localhost:8080
  Token: $TOKEN

Use it:
  export MAYFLY_URL=http://localhost:8080
  export MAYFLY_TOKEN=$TOKEN
  ./bin/mayfly run --prompt "Investigate the latest deploy failure"

EOF
