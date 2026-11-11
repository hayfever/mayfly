#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

make build >/dev/null
mkdir -p .mayfly

export MAYFLY_API_TOKEN="${MAYFLY_API_TOKEN:-dev-token}"
export MAYFLY_URL="${MAYFLY_URL:-http://localhost:8080}"
export MAYFLY_TOKEN="$MAYFLY_API_TOKEN"
export MAYFLY_LOG_LEVEL="${MAYFLY_LOG_LEVEL:-info}"

./bin/llmstub --addr :9099 >.mayfly/llmstub.log 2>&1 &
LLM_PID=$!
./bin/mcpstub --addr :9100 >.mayfly/mcpstub.log 2>&1 &
MCP_PID=$!
cleanup() {
  kill "$LLM_PID" "$MCP_PID" 2>/dev/null || true
}
trap cleanup EXIT

echo "llmstub: http://localhost:9099"
echo "mcpstub: http://localhost:9100"
echo "mayflyd: http://localhost:8080"
echo "token:   $MAYFLY_API_TOKEN"
echo

exec ./bin/mayflyd --config examples/config.local.yaml
