# Changelog

All notable changes are recorded here. The project follows semantic versioning once it reaches 1.0.0. Before that, minor versions may contain breaking changes; patch versions should not.

## v0.1.0 - Unreleased (release candidate v0.1.0-rc.1)

First public release.

### Added

- Ephemeral agent sessions created by API, generic webhook, GitHub, Slack, or Datadog triggers.
- One hardened Kubernetes pod per session, with a local process backend for development.
- Interactive shared thread over WebSocket, with a web UI and `mayfly attach` CLI.
- MCP servers: stdio inside the session image and HTTP through the control-plane credential proxy.
- Markdown skills from local directories and SHA-256-pinned remote archives or repository links.
- Selectable agent manifests with prompt templates, model, skills, MCP servers, limits, and shell policy.
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
- Exactly-once external side effects are not provided. A tool that was executing when the container died can run again; `run_command` in particular may repeat.
- Streaming assistant deltas are not journaled, so a retried step can show duplicate deltas.
- The journal keeps every turn of an active session until it finalizes, up to `journal.max_session_bytes`. A session that reaches the quota can no longer journal steps, so a crash after that point cannot resume the turn.
- An MCP tool with external effects, such as a Slack post, can run twice if the container dies after the call and before the step is journaled.

### Security

- Sessions receive one scoped token that expires with the session.
- LLM and MCP credentials are injected by the control plane.
- The default NetworkPolicy requires a CNI that enforces it. The e2e suite verifies enforcement.
