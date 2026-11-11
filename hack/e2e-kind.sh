#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

export MAYFLY_URL="${MAYFLY_URL:-http://localhost:8080}"
if [[ -z "${MAYFLY_TOKEN:-}" ]]; then
  MAYFLY_TOKEN="$(cat .mayfly/admin-token)"
  export MAYFLY_TOKEN
fi
export MAYFLY_API_TOKEN="$MAYFLY_TOKEN"
NAMESPACE="${NAMESPACE:-mayfly}"

command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
make build >/dev/null

fail() { echo "FAIL: $*" >&2; exit 1; }
auth=(-H "Authorization: Bearer $MAYFLY_TOKEN")

for _ in $(seq 1 60); do
  curl -fsS "$MAYFLY_URL/healthz" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "$MAYFLY_URL/healthz" >/dev/null || fail "control plane is not reachable"

echo "==> trigger a session through the generic webhook"
response="$(curl -fsS -X POST "$MAYFLY_URL/hooks/generic" \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Write a short report about the kind pipeline.","title":"kind e2e","agent":"triage"}')"
id="$(jq -r '.session_id' <<<"$response")"
[[ "$id" != "null" && -n "$id" ]] || fail "no session id in $response"
echo "session: $id"

for _ in $(seq 1 90); do
  session="$(curl -fsS "${auth[@]}" "$MAYFLY_URL/api/v1/sessions/$id")"
  status="$(jq -r '.session.status' <<<"$session")"
  [[ "$status" == "running" ]] && break
  [[ "$status" == "failed" ]] && fail "session failed: $session"
  sleep 1
done
[[ "$status" == "running" ]] || fail "session did not start (status: $status)"
jq -e '.session.agent == "triage" and .session.harness == "mayfly"' <<<"$session" >/dev/null || fail "agent or harness selection was not recorded"
pod="mayfly-$id"
kubectl -n "$NAMESPACE" wait --for=condition=Ready "pod/$pod" --timeout=90s >/dev/null

echo "==> verify pod isolation defaults"
pod_json="$(kubectl -n "$NAMESPACE" get pod "$pod" -o json)"
jq -e '
  .spec.securityContext.runAsNonRoot == true and
  .spec.containers[0].securityContext.readOnlyRootFilesystem == true and
  .spec.containers[0].securityContext.allowPrivilegeEscalation == false and
  (.spec.containers[0].securityContext.capabilities.drop | index("ALL")) != null and
  .spec.containers[0].securityContext.runAsUser == 65532 and
  .spec.automountServiceAccountToken == false and
  .spec.serviceAccountName == "mayfly-agent" and
  (.spec.containers[0].env | any(.name == "MAYFLY_AGENT_EXAMPLE" and .value == "triage"))
' <<<"$pod_json" >/dev/null || fail "pod security context is not hardened or agent runtime env is missing"
kubectl -n "$NAMESPACE" get networkpolicy "$pod" >/dev/null || fail "network policy missing"

echo "==> verify DNS works and egress outside the allowlist is blocked"
kubectl -n "$NAMESPACE" exec "$pod" -- getent hosts kubernetes.default.svc >/dev/null \
  || fail "DNS resolution failed inside the session"
blocked=false
for _ in $(seq 1 15); do
  if kubectl -n "$NAMESPACE" exec "$pod" -- curl -ksS -m 3 -o /dev/null https://kubernetes.default.svc/api 2>/dev/null; then
    sleep 1
    continue
  fi
  blocked=true
  break
done
[[ "$blocked" == "true" ]] || fail "kubernetes API is reachable from the session (NetworkPolicy not enforced?)"
if kubectl -n "$NAMESPACE" exec "$pod" -- curl -sS -m 3 -o /dev/null http://169.254.169.254/latest/meta-data 2>/dev/null; then
  fail "cloud metadata endpoint is reachable from the session"
fi

echo "==> wait for the agent turn"
events='[]'
for _ in $(seq 1 120); do
  events="$(curl -fsS "${auth[@]}" "$MAYFLY_URL/api/v1/sessions/$id/events")"
  if jq -e 'any(.events[]; .type == "assistant_message" and (.text | length > 0))' <<<"$events" >/dev/null; then
    break
  fi
  sleep 1
done
jq -e 'any(.events[]; .type == "assistant_message" and (.text | length > 0))' <<<"$events" >/dev/null \
  || fail "no assistant reply"

jq -e 'any(.events[]; .type == "tool_result" and (.data.tool | startswith("mcp_")) and (.text | contains("echo: hello from mayfly")))' <<<"$events" >/dev/null \
  || fail "MCP tool result missing"

echo "==> verify websocket attach"
timeout 30 ./bin/mayfly attach "$id" --once </dev/null >/tmp/mayfly-attach.out 2>/dev/null || true
grep -q "Completed." /tmp/mayfly-attach.out || fail "websocket replay did not show the assistant reply"

echo "==> close and export"
curl -fsS -X POST "${auth[@]}" -H 'Content-Type: application/json' \
  -d '{"reason":"e2e"}' "$MAYFLY_URL/api/v1/sessions/$id/close" >/dev/null
for _ in $(seq 1 90); do
  session="$(curl -fsS "${auth[@]}" "$MAYFLY_URL/api/v1/sessions/$id")"
  status="$(jq -r '.session.status' <<<"$session")"
  [[ "$status" == "completed" ]] && break
  [[ "$status" == "failed" || "$status" == "expired" ]] && fail "session ended as $status: $session"
  sleep 1
done
[[ "$status" == "completed" ]] || fail "session did not complete (status: $status)"

artifact="/tmp/mayfly-e2e-$id.tar.gz"
curl -fsS "${auth[@]}" "$MAYFLY_URL/api/v1/sessions/$id/artifact" -o "$artifact"
tar -tzf "$artifact" | grep -qx 'files/report.md' || fail "workspace file missing from bundle"
tar -xzf "$artifact" -O files/report.md >/tmp/mayfly-report.md
grep -q "Mayfly session report" /tmp/mayfly-report.md || fail "report content missing"
tar -xzf "$artifact" -O traces.jsonl >/tmp/mayfly-traces.jsonl
grep -q "echo: hello from mayfly" /tmp/mayfly-traces.jsonl || fail "MCP trace missing"
tar -xzf "$artifact" -O manifest.json >/tmp/mayfly-manifest.json
jq -e '.status == "completed" and .partial == false and .agent == "triage" and .harness == "mayfly"' /tmp/mayfly-manifest.json >/dev/null \
  || fail "manifest is not a complete export"
echo "artifact: $artifact"

echo "==> verify TTL expiry"
response="$(curl -fsS "${auth[@]}" -H 'Content-Type: application/json' \
  -d '{"prompt":"TTL test","ttl":"10s"}' "$MAYFLY_URL/api/v1/sessions")"
ttl_id="$(jq -r '.session.id' <<<"$response")"
for _ in $(seq 1 90); do
  session="$(curl -fsS "${auth[@]}" "$MAYFLY_URL/api/v1/sessions/$ttl_id")"
  status="$(jq -r '.session.status' <<<"$session")"
  [[ "$status" == "expired" || "$status" == "completed" ]] && break
  sleep 1
done
[[ "$status" == "expired" ]] || fail "TTL session ended as $status, wanted expired"
jq -e '.session.artifact != null' <<<"$session" >/dev/null || fail "TTL session has no artifact"
ttl_artifact="/tmp/mayfly-e2e-ttl-$ttl_id.tar.gz"
curl -fsS "${auth[@]}" "$MAYFLY_URL/api/v1/sessions/$ttl_id/artifact" -o "$ttl_artifact"
tar -xzf "$ttl_artifact" -O manifest.json | jq -e '.reason == "expired"' >/dev/null \
  || fail "TTL bundle did not record the expired reason"

echo "PASS"
