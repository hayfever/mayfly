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
./hack/surface-diff.sh  # CLI surface gate (docs/deprecation.md)
make image     # four Docker targets
make run-local # control plane + stubs, token dev-token
make cluster-up   # k3d cluster, images, manifests
make e2e       # end-to-end suite on k3d (docs/testing.md)
make cluster-down
```

## Test layers

| Layer | What it proves |
|---|---|
| `cmd/mayfly` | `--json` output, flag parsing, and the CLI surface golden file; `hack/surface-diff.sh` fails a change that removes or renames a command, flag, or environment variable without a recorded break |
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

## Persisted formats and migrations

Every store carries a format version. A store reads version 1 and rejects a newer version with an error that names the store and both versions.

| Store | File or key | Stamp | Migration case |
|---|---|---|---|
| Queue, bbolt | `queue.db` | `meta` / `format_version` | `internal/queue/format.go`, `migrateFormat` |
| Queue, redis | `{<key_prefix>}:format_version` | the key value | `internal/queue/format.go`, `migrateFormat` |
| Journal | `journal.db` | `\x00meta` / `format_version` | `internal/journal/bbolt.go`, `migrateFormat` |
| Schedule watermark | `state_path` | `meta` / `format_version` | `internal/state/state.go`, `migrateFormat` |
| Session records | `sessions.db` | `meta` / `format_version` | `internal/session/records_bolt.go`, `migrateRecordsFormat` |
| Queue job payload | `jobs` bucket or hash | JSON `version` | `internal/queue/envelope.go`, `fromEnvelope` |
| Session record JSON | `sessions` bucket | JSON `version` | `internal/session/records.go`, `checkRecordVersion` |

A store with no stamp holds the version 1 layout and loads unchanged. The store writes the stamp on the next open. A missing stamp is not an error.

To change a persisted layout:

1. Raise the version constant in the file that owns the layout.
2. Add a case for the old version to that store's migration function. The case moves stored records to the new layout in the open transaction and runs once for each old file.
3. Add a fixture set for the release that writes the new layout. Keep every earlier set.
4. Update the table above, [configuration.md](configuration.md#persisted-formats), and the changelog.

`internal/queue/format_test.go` covers an unstamped store and a newer stamp for both queue backends. A migration is one way: a later build reads the new layout and an older build refuses it.

### Compatibility fixtures

`test/compat/testdata/vX.Y.Z` holds one fixture set for each release that wrote a persisted format. The tests in `test/compat` copy each file, open it with the current stores, and check jobs, states, attempts, payloads, journal steps, the watermark, session records, the artifact index, and the stored version. A build must read every set.

A set holds `queue.db`, `queue-redis.json`, `journal.db`, `state.db`, and `sessions.db`. The generator writes the current layout:

```bash
go run ./test/compat/gen -out test/compat/testdata/v0.3.0
```

Run this command from the release build before the release ships, and commit the output. Do not regenerate a set whose release has shipped: the committed bytes are the contract. The command refuses to replace an existing set unless you add `-force`. A bbolt file holds page layout and transaction identifiers, so a regenerated database is byte-different but carries the same data. The redis dump is byte-identical.

The tests also write a newer version into a copy of each store and check the open error. Run them with `go test ./test/compat`, or through `make test`.

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
