# Changelog

All notable changes are recorded here. The project follows semantic versioning once it reaches 1.0.0. Before that, minor versions may contain breaking changes; patch versions should not.

## [0.3.0] - Unreleased

### Changed

- Install and release docs describe the public 0.2.0 images and chart, verify releases by digest with cosign, and [docs/security.md](docs/security.md) adds an attacker-by-asset threat model.

## [0.2.0] - 2026-11-19

Operators moving from 0.1.x or a release candidate should read
[docs/migrating-to-0.2.md](docs/migrating-to-0.2.md).

### Added

- Durable session records and artifact index in `sessions.db`, with a stored `format_version`. A restarted control plane lists finished sessions that retention still covers, and artifact downloads keep working across a restart.
- Bounded adopt retries. A failed adopt backs off from 15s to 2m and stops after 10 attempts instead of retrying every session.
- Shutdown budget (`shutdown.budgetSeconds`, `server.shutdown_budget`) for the HTTP drain, queue worker drain, and session manager shutdown.
- API error envelope `{"error":{"code","message","details?"}}`, cursor pagination with `items` and `next_cursor`, and an OpenAPI 3.1 spec at `api/openapi.yaml`.
- `hack/api-diff.sh`, which fails CI on a breaking API change. It accepts additive optional fields in `api/job.v1.schema.json` and rejects removals, renames, retypes, and required-field changes.
- Versioned queue job envelope `api/job.v1.schema.json`, documented in [docs/queue-jobs.md](docs/queue-jobs.md). Producers and stores encode and decode it through one codec, and the job id is derived from the idempotency key.
- Typed event payloads in `internal/model`, with a schema per type in the OpenAPI document and the contract in [docs/events.md](docs/events.md).
- Named API tokens with scopes under `server.tokens`, plus one audit line per authenticated, mutating `/api/v1` request.
- Prometheus metrics and one structured request log line per request with a request id. `GET /metrics` requires a token with the `admin` scope on the main port, or is served on a separate address.
- Release supply chain: Trivy scans both image architectures before a tag exists, and cosign signs the image digests and the chart with the workflow identity. The Helm chart validates its values with `values.schema.json`.
- Operator runbook with upgrade, state-volume backup and restore, dead-letter triage, stuck-session recovery, and restart steps. Deprecation policy in [docs/deprecation.md](docs/deprecation.md).
- An end-to-end upgrade test that runs one release against the previous release and checks that in-flight sessions resume.
- A reproducible load and soak harness at `hack/loadtest`, with `make loadtest` and [docs/loadtest.md](docs/loadtest.md). It drives N concurrent sessions and webhook triggers against Mayfly with the `llmstub` backend and reports p50, p95, and p99 latency, error rate, session throughput, and goroutine and memory growth. It runs against local k3d or any kubeconfig and does not create cloud resources or use real model keys.
- `/metrics` also exposes the standard Prometheus Go and process collectors, so a soak can watch runtime growth.
- `POST /api/v1/schedules/{name}/run` returns the full accepted-job object. The webhook, manual schedule run, and dead-job retry use one accepted-job shape.
- Documented the complete set of unauthenticated routes in [docs/api.md](docs/api.md#public).

### Breaking changes

- Every API error now uses the JSON error envelope. A missing or unknown token returns 401 with code `unauthenticated`. An unknown error returns 500 `internal` instead of a 400 body, and an unmatched `/api` path returns the envelope. A create that fails after the session exists returns 500 with `details.session_id`.
- List responses from `GET /api/v1/sessions`, `GET /api/v1/queue/dead`, and `GET /api/v1/schedules` return `items` and an optional `next_cursor` instead of the former `sessions`, `dead`, or `schedules` array. `limit` accepts 1 to 500, and any other value is `400 invalid_request`. Schedules are sorted by name.
- CLI errors print `code: message` instead of a bare message. `MAYFLY_TOKEN` is a deprecated alias for `MAYFLY_API_TOKEN` and prints one warning.
- `mayflyd` exits with code 2 when no API token is configured. It no longer generates a token and writes the secret to the log.
- A non-empty `server.tokens` list replaces `server.token_env`. Every `/api/v1` route requires a scope. The legacy `default` token keeps the `admin` scope, and `POST /api/v1/sessions/{id}/attach-ticket` requires `sessions:write`.
- Third-party producers and consumers of the raw queue payload must use the version 1 envelope. Mayfly still decodes the v0.1.0 and release-candidate shape, and rewrites it on the next record write.
- The Go API changed for importers of the internal packages: `queue.Job.Delivery` is now `queue.Job.IdempotencyKey`, `queue.KindSession` is `"session.create"`, `queue.KindSource` is `"source.run"`, and `model.Event.Data` and `journal.StepEvent.Data` are `model.EventData` instead of `map[string]any`. The HTTP and WebSocket wire format is unchanged.
- A restarted control plane now lists restored finished sessions and returns 204 for `DELETE` on them. A stored state file with a `format_version` newer than the build stops startup. The new `sessions.db` must be on the control-plane PersistentVolume.
- Helm value `replicaCount` is removed, and `values.schema.json` rejects unknown top-level values. The chart pins one replica. `shutdown.budgetSeconds` must be less than `shutdown.gracePeriodSeconds`, or `helm template` fails.
- The default LiteLLM image tag changes from `main-stable` to `v1.110.0`.
- Published images and the chart are signed from 0.2.0. Releases before 0.2.0 have no signature.
- `GET /api/v1/queue/jobs/{id}` returns `409 conflict` with the decoder reason for a job whose stored payload cannot be decoded. It returned `503 unavailable` before. A client that retried on `503` must stop retrying on `409`.
- A version 1 `session.create` without a prompt, or a `source.run` without a complete source, is buried with a reason. It is no longer claimed and retried.
- Two envelopes with the same `idempotency_key` and different ids are duplicates. A producer-supplied id does not change the dedupe key.
- `POST /api/v1/queue/dead/{id}/retry` returns the shared accepted-job object `{"accepted","duplicate","job_id","delivery","status"}`. It returned `202 {"retried":true,"id":"..."}` before.
- Session JSON no longer includes `backend`, `pod_name`, or `artifact.uri`. This covers `POST /api/v1/sessions`, `GET /api/v1/sessions`, `GET /api/v1/sessions/{id}`, and the WebSocket hello. Download the bundle through `GET /api/v1/sessions/{id}/artifact`.
- Unknown fields in `/api/v1` request bodies return `400 invalid_request`. Webhook bodies from external senders stay permissive.
- `Event.type` and `Trigger.kind` are no longer closed enums in the OpenAPI document. A new type or optional field is additive. A rename or removal is breaking.

### Fixed

- [docs/loadtest.md](docs/loadtest.md) no longer presents `e2e/values.yaml` as a load test configuration. It lists the session timeouts that complete stub sessions and the volume attach delay on cloud providers.
- Images are built with `golang.org/x/net` 0.62.0 (CVE-2026-78661) and the agent image ships npm 12.2.3 (undici and brace-expansion CVEs).
- The release workflow signs the Helm chart by digest.
- The plugin shim drops readiness as soon as a child request fails, so `/readyz` no longer passes after the child has exited.
- Adoption reports the current attempt count and the correct singular or plural in the failure message. `DELETE` is safe against an adoption retry in flight.

## [0.1.0] - Unreleased

### Added

- Ephemeral agent sessions created by API, generic webhook, GitHub, Slack, or Datadog triggers.
- One hardened Kubernetes pod per session, with a local process backend for development.
- Interactive shared thread over WebSocket, with a web UI and `mayfly attach` CLI.
- MCP servers: stdio inside the session image and HTTP through the control-plane credential proxy.
- Plugins: MCP servers declared in a bundle under `plugins/<name>/plugin.yaml`, in one of three placements. `inline` runs the plugin as a child of the runtime, `sidecar` runs it in the session pod behind a Unix-socket shim, and `service` runs a Deployment that `mayflyd` reconciles and shares across sessions. Tool names are `plugin_<plugin>_<tool>` in every placement.
- A tool gateway in `mayflyd` for every non-built-in tool call. It holds the session's grant, records each call in a durable ledger keyed by `<session>/<turn>/<step>`, attaches the idempotency key, enforces call timeouts, plugin rate limits, and an in-flight cap, signs sidecar and service call tokens, and writes an audit record without arguments or results.
- `cmd/mayfly-plugin-shim`: `install-self` and `copy` stage plugin trees into a pod; `serve` supervises a stdio plugin and serves it over streamable HTTP with token checks and replay dedupe; `probe` backs the startup and readiness probes.
- `mayfly plugin init|check|test` for offline plugin work, with Go, Python, and TypeScript scaffolds, plus the `examples/plugins/notify` (sidecar), `tickets` (service), and `word-count` (inline) plugins.
- Service plugin support in `internal/pluginsvc`: one Deployment, Service, NetworkPolicy, and gateway key Secret per plugin, reconciled at startup and on an interval, with pruning, a `resourceVersion` update on spec change, and an `http` transport for plugins that serve MCP themselves.
- Sidecar support: native sidecar containers on a shared socket volume, one `gateway-key` in the session Secret mounted only into sidecars, and call tokens that the shim verifies.
- `plugins:` configuration block (`namespace`, `reconcile_interval`, `prune`, `audit_path`, `max_inflight_calls`) and `runtime.plugin_volume_limit`.
- `mcp.servers[].tools` to allowlist a server's tools, and `mcp.servers[].idempotent_tools` to mark the tools that are safe to repeat.
- Markdown skills from local directories and SHA-256-pinned remote archives or repository links.
- Selectable agent manifests with prompt templates, model, skills, MCP servers, plugins, limits, and shell policy.
- In-image harness selection through `mayfly-harness`, with a built-in Mayfly fallback.
- Artifact export containing conversation, events, traces, workspace files, and a manifest.
- Local filesystem and S3-compatible artifact stores.
- Scoped per-session tokens. LLM, MCP, and object-store credentials stay in the control plane.
- Pod isolation defaults: non-root, read-only root filesystem, all capabilities dropped, no service-account token, and default-deny ingress with limited egress.
- LiteLLM gateway configuration and deployment examples for OpenAI, Anthropic, Bedrock, OpenRouter, and internal backends.
- Helm chart with optional LiteLLM, ingress, inline skills and agent manifests, image digests, and LiteLLM IRSA support.
- `mayfly transcript <id>` prints the session's agent prompt and model messages, including tool calls and results.
- Versioned CLI, control-plane, and agent builds with commit and date metadata.
- End-to-end suite on k3d (`make e2e`, bbolt and Redis in CI). k3s ships a kube-router controller that enforces NetworkPolicy on flannel, so no second CNI is installed: signed webhooks for every trigger type, dedupe, rate limits, queue durability across a control plane restart, dead letters, enforced network isolation, MCP, WebSocket attach, close, artifacts, TTL expiry, and cleanup. It also runs against an existing cluster.
- CI and release workflows: tests, Helm lint, k3d e2e, multi-arch GHCR images, SBOMs, provenance, CLI archives, and OCI chart publishing.
- Session cap (`sessions.max_concurrent`) and per-trigger webhook rate limits; both answer `429` with `Retry-After`.
- `sessions.retention` expires finished sessions from memory and releases their thread state.
- `/readyz` reports lifecycle state instead of always returning ready.
- S3 artifact export can use the default AWS credential chain, including IAM roles for service accounts.
- `sessions.connect_timeout` fails a session whose runtime never reaches the control plane, so a stuck container does not hold a capacity slot until TTL.
- Artifact metadata outlives session retention, so downloads keep working after a session leaves the list.
- Durable webhook queue with `memory`, `bbolt`, and Redis-protocol backends, delivery dedupe, retry backoff, dead-job counting, and `GET /api/v1/queue`.
- Dead-letter tooling: list buried jobs, inspect one, retry it with a fresh attempt count, or delete it.
- Job-to-session correlation: `trigger.job_id` on the session and `GET /api/v1/queue/jobs/{id}` backed by a `queue.tombstone_ttl` record.
- `queue` configuration block with `max_jobs`, `max_attempts`, `retry_base`, `visibility_timeout`, `dedupe_ttl`, and `poll_interval`.
- Helm `persistence` option that puts the queue database on a PersistentVolume.
- Turn step journal (`internal/journal`) with bbolt and in-memory stores. Each `user`, `llm`, `tool:<name>`, and `assistant` step records its input, output, error, and resulting `conversation.jsonl` length.
- Internal journal endpoints (`POST` and `GET /internal/v1/sessions/{id}/journal`) authenticated by the session token.
- Session resume: the runtime always checks the journal on startup, truncates `conversation.jsonl` to the last journaled step, rebuilds the turn event from the journal, and executes only missing steps. A replacement needs no configuration change, so the ConfigMap is no longer updated for it.
- Replacement containers: a non-terminal pod failure or disappearance starts a new pod with the same session, Secret, ConfigMap, NetworkPolicy, and workspace claim, up to `sessions.max_restarts` (default `2`). The local backend restarts the child in the same state directory.
- `sessions.persistent_workspace` (default `true`) and one per-session PVC, `mayfly-<id>-workspace`, mounted at `/workspace` and `/artifacts` through subPaths. The claim is deleted during cleanup.
- `sessions.max_restarts` and `journal` configuration blocks.
- Control-plane restart recovery restores the last journaled turn to the thread with its step keys and publishes step events that were recorded in the journal.
- Runtime retries transient model requests, so a control-plane replacement does not end an in-flight turn. Each step has a five-minute retry budget; after it the runtime exits and a replacement resumes.
- Optional `step_key` on session events; the hub drops duplicates so a re-executed step does not post the same tool or assistant message twice.
- Adopted sessions reserve their hub sequence in the journal, in blocks of 1000, so a restarted control plane never reuses a turn key. The runtime also rejects a journaled turn whose recorded user event is not the message it received, and compares step inputs and step numbers during replay.
- Labelled session objects whose pod is gone are garbage-collected at adoption instead of leaking their Secret, ConfigMap, network policy, and claim. A session that still has all three resumable objects gets a replacement pod instead, which covers a control plane that stops between deleting a failed pod and starting its replacement.
- `runtime.storage_class` selects the workspace claim's StorageClass. The claim is now sized as `workspace_limit + artifact_volume_limit`, because the bundle is staged on the same volume.
- End-to-end tests for forced and graceful pod deletion and control-plane replacement mid-turn, and a local SIGKILL/resume integration test.
- Declarative `http.json` sources: templated requests, pagination, fanout over a parent document's URL fields, JSON path projection with wildcards and aliases, field filters, provider change tracking with diff truncation, retries, and item limits. Provider knowledge is bundle configuration, not Go code.
- `requiredEnv` on a connection or source: a missing credential stops startup and names the definition instead of reaching the provider as an empty header.
- Project bundles: one file can hold agents, schedules, connections, and sources. `shared` and `agent:<name>` scopes, and an RFC 7386 `extends` merge with no silent duplicate names.
- Schedules: a five-field UTC cron expression on an agent manifest, `triggerOn` for `changed` and `new`, a per-schedule TTL, and a prompt template over the run totals, changed documents, and errors.
- One run artifact per scheduled run at `runs/<schedule>/<run id>.json`, readable by the session through the artifact tool, with every document and its stored diff (already cut to `maxLines` and `maxLineChars`). The prompt receives at most 8 KiB per diff and 32 KiB of changes, plus a fixed untrusted-content notice.
- Cron slot dedupe through a delivery key, a bbolt watermark at `sources.state_path`, and a single-slot catch-up within seven days.
- `sources` configuration block: `state_path`, `max_artifact_bytes`, `concurrency`, `timeout` (one source step, default 15m), and `job_timeout` (one schedule job, default 30m).
- Queue lease renewal: a running job's lease is extended every third of `queue.visibility_timeout`, and each job kind has its own deadline.
- A retried schedule job reuses its run artifact instead of fetching again, and `CreateOptions.IdempotencyKey` keeps a retry from starting a second session.
- Template `env` allowlist: a template reads only variables declared in `requiredEnv` or `optionalEnv` on its connection, source, or schedule. `MAYFLY_*` and control-plane secrets are never readable.
- `allowPrivate` on a connection. `allowHTTP` no longer turns off the private-address guard, and NAT64 prefixes are blocked.
- `journal.max_entry_bytes` and `journal.max_session_bytes`, and a per-turn step bound.
- An optional `mayfly-bundles` ConfigMap mounted at `/etc/mayfly-bundles` for operator-owned bundles that a redeploy does not replace.
- `GET /api/v1/schedules` and `POST /api/v1/schedules/{name}/run`. A queued run is polled with `GET /api/v1/queue/jobs/{id}`.
- `mayflyd -check` loads the config and project bundle, prints the resolved agents, schedules, and source plans, and exits without starting servers.
- Kubernetes and Helm options for the project bundle and the `mayfly-sources` provider secret.

### Known limitations

- The control plane runs one replica. Session records and thread history are in memory; artifacts are durable. Finished sessions are dropped after `sessions.retention`.
- Single tenant. There is no SSO or per-user authorization, and no general audit log. The tool gateway logs one record per non-built-in tool call (session, agent, tool, placement, idempotency key, argument hash, outcome), without arguments or results. Built-in tools, API requests, and operator actions are not audited.
- The admin token is a shared bearer token. The web UI stores it in browser localStorage.
- No TLS or ingress authentication is provided; put Mayfly behind an authenticated ingress.
- Native Anthropic and Bedrock APIs are not implemented. Use LiteLLM.
- Per-agent provider credentials and LiteLLM virtual keys are not supported.
- Remote skill archives are fetched at control-plane startup. There is no cache; private GitHub archive redirects may drop authorization headers.
- Harness images and the session runtime protocol are documented but should be treated as experimental in 0.x.
- Image pull secrets must be configured explicitly (`runtime.image_pull_secrets`); no registry credentials are discovered automatically.
- No Prometheus metrics or tracing endpoint; structured logs only.
- No OpenAPI schema. The HTTP API is documented manually and may change in 0.x.
- No artifact encryption, image signing, or admission policy is provided by the platform itself.
- Exactly-once external side effects hold for tools the gateway grants. Built-in tools are outside the ledger: a `run_command` that was executing when the container died can run again.
- Streaming assistant deltas are not journaled, so a retried step can show duplicate deltas.
- The journal keeps every turn of an active session until it finalizes, up to `journal.max_session_bytes`. A session that reaches the quota can no longer journal steps, so a crash after that point cannot resume the turn.
- An alternate harness bypasses the tool gateway through `* /internal/v1/mcp/{server}`, so its MCP calls have no ledger record and can repeat after a crash.
- Plugins are trusted operator code and are not sandboxed beyond the pod security context. Do not load a plugin you would not run as a cluster workload.

### Security

- Sessions receive one scoped token that expires with the session.
- LLM and MCP credentials are injected by the control plane.
- Sidecar and service plugin secrets stay outside the agent container. An inline plugin's secrets are files in the agent container, so an agent with `allowShell` cannot select such a plugin.
- Sidecar calls carry a short-lived token signed with the session's `gateway-key`, which is mounted only into sidecars. Service calls carry a token signed with the per-plugin key in `mayfly-gateway-<name>`.
- The default NetworkPolicy requires a CNI that enforces it. The e2e suite verifies enforcement.
