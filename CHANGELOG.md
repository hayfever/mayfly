# Changelog

All notable changes are recorded here. The project follows semantic versioning once it reaches 1.0.0. Before that, minor versions may contain breaking changes; patch versions should not.

## v0.1.0 - Unreleased (release candidate v0.1.0-rc.1)

First public release.

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

### Changed

- `sessions.allow_shell` defaults to `false`. Enable the shell tool per deployment or per agent manifest.
- A session becomes `running` when its runtime connects, not when the container starts.
- The LLM broker accepts only the model assigned to the session and fills an empty model field with it.
- The LLM and MCP proxy clients bound connection setup and response-header wait with `llm.timeout`.
- Webhook rate limits count only verified requests.
- Webhooks now accept and return before any pod exists; a queue worker creates the session. Responses carry `job_id`, `delivery`, and `duplicate` instead of a session id.
- Slack slash commands answer `Mayfly session queued.` because the session link does not exist yet.
- `/readyz` no longer calls the artifact store, and the probes set `timeoutSeconds: 3`.
- The module path and published images use `github.com/hayfever/mayfly` and `ghcr.io/hayfever`.
- The S3 store sends checksums only when required, for compatibility with R2, B2, and older MinIO.
- `mayflyd` rejects unknown configuration fields.
- `conversation.jsonl` is loaded on runtime start instead of being truncated by a fresh process.
- The Kubernetes agent writes artifacts under `/artifacts` on the session workspace claim; the old `/var/lib/mayfly` EmptyDir mount is gone.
- The stub MCP server counts `tools/call` executions and exposes `GET /counts`; the stub LLM calls the MCP tool before writing its report.
- An assistant message and a tool result are published only when their step is journaled. The live tool call is still emitted, because it is deterministic once the model step is journaled, and the journal publication's copy is deduplicated by its step key.
- On SIGTERM the runtime stops the turn, closes its logs, and exits non-zero without finalizing unless a close control event was received. Drains, evictions, preemptions, and `kubectl delete` now start a replacement instead of finalizing the session as completed. A session that cannot resume (`sessions.max_restarts: 0`, or a Kubernetes session without a persistent workspace) still finalizes on SIGTERM so its full bundle is exported.
- A journal apply failure or a `conversation.jsonl` length that does not match its journaled `ConvLen` ends the process so the replacement repairs the conversation from the journal.
- The journal is deleted when its session finalizes; the artifact bundle is the durable history.
- Control-plane persistence is on by default in the Helm chart, and the kustomize base mounts `/var/lib/mayfly` from the `mayfly-state` claim, so a restarted control plane keeps its queue, journal, and local artifacts.
- If the control-plane journal is lost anyway (persistence disabled), adoption of a running session seeds its event sequence at the current time in microseconds, above every key the lost journal could have handed out, and logs a warning. The in-flight turn finishes, and a message posted before its next step can no longer get the same turn key.
- The journal rejects a step for a turn older than the session's last journaled turn, so a stale container cannot move the resume point back.
- A replacement retries deleting the pod it replaces (four attempts over about three seconds) before it starts, instead of leaving a live old pod on the workspace claim until cleanup.
- Session cleanup deletes the ConfigMap before the pod, and `mayflyd` shutdown waits for cleanups in progress. Adoption deletes a session pod whose ConfigMap is gone instead of keeping it, so a control plane that stops mid-cleanup cannot bring a finalized session back as a pod-less replacement.
- A replayed step records its reply or tool result in `events.jsonl` when the crashed process applied it without logging it. Events already in the log for that step are not written again.
- The internal events endpoint no longer accepts a batch-level `step_key`. Only the per-event key is used.
- `sessions.persistent_workspace: false` now disables replacement containers instead of resuming onto an empty workspace.
- The idle timer no longer finalizes a session while a turn is running, and only resets between turns.
- Pod failure detection treats a pod that is terminating or Running but not ready as missing, and stops waiting on container configuration errors that cannot recover.
- Every plugin call and every `mcp.servers` tool call now goes through the tool gateway under the built-in harness. Tool names do not change. `* /internal/v1/mcp/{server}` remains for alternate harnesses.
- Resume no longer repeats a tool call that was in flight when the container died, unless the tool is marked idempotent. A completed call whose journal step did not land replays its recorded result instead of running again. Before this change an in-flight `mcp_*` call ran again after resume; now it returns an error result that says the effect may already have happened.
- `mayflyd -check` prints the resolved plugins. It fails on an unknown or invisible plugin, an alternate harness that selects plugins, an inline plugin with secrets under `allowShell`, an image outside `agents.allowed_images`, and a `service.mcpServer` that is missing, not HTTP, or not visible to the agent.
- The Helm chart renders a second Role and RoleBinding in `config.plugins.namespace` when that namespace differs from the release namespace, and `config.plugins` and `config.runtime.plugin_volume_limit` are chart values.

### Fixed

- `runtime.image_pull_secrets` now reaches session pods, so private agent images pull correctly.
- `runtime.env` and agent `runtime.env` cannot set `MAYFLY_CONFIG` or `MAYFLY_SESSION_TOKEN`; startup fails and the pod builder drops them.
- GitHub triggers no longer accept the static `X-Mayfly-Token` header, which could be replayed.
- `mayflyd -check` now builds the session manager as startup does, so a missing model, a model outside `llm.allow_models`, or an unknown MCP server fails the check instead of the first run.
- Adopted sessions keep their agent, artifacts, and idempotency key after a control-plane restart.
- `read_artifact` checks the declared SHA-256 and never splits a UTF-8 character at a window edge.
- `mayfly transcript` reads only the top-level `conversation.jsonl` and strips terminal control sequences from untrusted text.
- The `local` runtime starts the agent with a minimal environment instead of mayflyd's.
- Fanout dedupes identical requests, fails fast on `401` and `403`, and drops filtered-out responses. `maxItems` counts items after `filter`. A `removed` page without a diff counts as changed. `sources.timeout` is enforced.
- A scheduled slot comes from the cron time, not the wake time, so a late tick keeps the same delivery key.
- A private definition can no longer extend another agent's private definition.

### Known limitations

- The control plane runs one replica. Session records and thread history are in memory; artifacts are durable. Finished sessions are dropped after `sessions.retention`.
- Single tenant. No SSO, per-user authorization, or audit log.
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
