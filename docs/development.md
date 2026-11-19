# Development

## Requirements

- Go 1.26+
- Docker for images
- k3d, kubectl, Helm, openssl for the quickstart cluster and the end-to-end suite
- Node is not required on the host; stdio MCP servers run inside the agent image

## Commands

```bash
make build     # bin/mayflyd, mayfly-agent, mayfly, llmstub, mcpstub
make test      # unit and integration tests
make vet
make image     # four Docker targets
make run-local # control plane + stubs, token dev-token
make cluster-up   # k3d cluster, images, manifests
make e2e       # end-to-end suite on k3d (docs/testing.md)
make cluster-down
```

## Test layers

| Layer | What it proves |
|---|---|
| `internal/config` | defaults, duration parsing, allowlist |
| `internal/agents` | manifest parsing, prompt files, duplicates, harness, protocol |
| `internal/harness` | in-image harness resolution and Mayfly fallback |
| `internal/k8s` | pod hardening, mounts, NetworkPolicy |
| `internal/agent` | system prompt composition |
| `internal/artifact` | S3 SigV4 known vector, local store, key handling, optional live S3 test |
| `internal/bundle` | tar.gz format, manifest, truncation |
| `internal/triggers` | GitHub, Slack, Datadog, generic verification and prompts |
| `internal/mcp` | stdio helper process, HTTP transport, tool naming |
| `internal/broker` | model allowlist, streaming passthrough, MCP header injection |
| `examples/litellm` | LiteLLM gateway configuration and deployment example |
| `test/integration` | webhook → queue → worker → real agent process → MCP → WebSocket → close → artifact |
| `e2e` (`make e2e`) | released images and chart on a real cluster: webhooks, queue durability, dead letters, isolation, lifecycle; see [testing.md](testing.md) |
| `e2e` upgrade test | the last published release upgraded to HEAD; set `MAYFLY_E2E_UPGRADE_FROM` and see [testing.md](testing.md#upgrade-test) |

Run one test:

```bash
go test ./internal/triggers -run TestSlack -v
go test ./test/integration -v
```

The integration test builds `cmd/mayfly-agent` into a temporary directory. It needs a working Go toolchain and takes a few seconds.

## Local stubs

`cmd/llmstub` is an OpenAI-compatible server with a deterministic policy:

1. Call `write_file` to create `report.md`.
2. Call the first available `mcp_*` tool.
3. Reply with a summary.

`cmd/mcpstub` is a Streamable HTTP MCP server with one `echo` tool.

They exist to make the full lifecycle testable without external services. They are not part of the runtime path.

## Adding an artifact store

Implement [artifact.Store](../internal/artifact/store.go):

```go
type Store interface {
    Put(ctx context.Context, key string, r io.Reader, size int64, sha256Hex, contentType string) (string, error)
    Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
}
```

Wire it into `artifact.New` and add configuration. Return a stable URI for the manifest. Keep credentials on the control plane.

## Adding a runner

Implement [runner.Runner](../internal/runner/runner.go). `Start` receives the session config and token; `Stop` must remove all resources; `Logs` feeds the fallback bundle. If the backend has a natural completion channel, set `Handle.Done`; otherwise the control plane polls through `k8s.Client`.

## Adding a trigger

Add verification and prompt construction in [internal/triggers](../internal/triggers). Return `session.CreateOptions`; do not call the manager. The HTTP layer owns creation and response formatting. Reject unsigned payloads unless the trigger config allows it.

## Credential rule

New tools or integrations must not put long-lived credentials in the session container. Add an HTTP route under `/internal/v1/` that authenticates the session token, checks that the resource is assigned to the session, and injects the credential on the control-plane side. Update [docs/security.md](security.md) with any new boundary.

## Style

- Keep the control plane and runtime separated by the HTTP protocol.
- Prefer standard library APIs; dependencies are limited to client-go, gorilla/websocket, and yaml.v3.
- Log structured JSON with `log/slog`.
- Make lifecycle transitions idempotent. The runtime and control plane can race on close, crash, and timeout.


## Optional live S3 test

`TestS3CompatibleStore` is skipped unless an endpoint and credentials are set. It creates the bucket, uploads, downloads, and verifies the payload.

```bash
docker run -d --name mayfly-s3 -p 19000:9000 \
  -e RUSTFS_ACCESS_KEY=testaccess -e RUSTFS_SECRET_KEY=testsecret \
  -e RUSTFS_VOLUMES=/data ghcr.io/rustfs/rustfs:latest

MAYFLY_S3_TEST_ENDPOINT=http://127.0.0.1:19000 \
MAYFLY_S3_TEST_ACCESS_KEY=testaccess \
MAYFLY_S3_TEST_SECRET_KEY=testsecret \
MAYFLY_S3_TEST_BUCKET=mayfly-live-test \
go test ./internal/artifact -run TestS3CompatibleStore -v

docker rm -f mayfly-s3
```
