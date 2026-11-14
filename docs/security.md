# Security

## Trust boundaries

1. **Trigger/API caller** — authenticated by an admin token or a per-trigger secret.
2. **Control plane** — holds the admin token, LLM key, MCP credentials, and object-store credentials.
3. **Session container** — untrusted by default. It holds one scoped session token and no long-lived credentials.
4. **Plugin workload** — trusted operator code, but not trusted with the agent's data: an inline plugin shares the agent container, a sidecar shares the session pod, and a service is its own Deployment.
5. **Artifact store** — receives bundles from the control plane only.

## Session isolation defaults

The pod and container set:

- `runAsNonRoot`, UID/GID 65532, no privilege escalation, all capabilities dropped.
- `readOnlyRootFilesystem` with writable mounts at `/workspace` and `/artifacts` (the per-session PVC or EmptyDirs) and `/tmp` (memory-backed EmptyDir).
- `seccompProfile: RuntimeDefault`.
- `automountServiceAccountToken: false`; `enableServiceLinks: false`.
- Restart policy `Never`. The kubelet does not restart a container; the control plane starts a replacement for an active session. `activeDeadlineSeconds` remains a hard stop for the container's own lifetime.
- Resource requests/limits and EmptyDir size limits.

The NetworkPolicy sets:

- `Ingress: []` — nothing can reach the pod.
- Egress to the control plane namespace and pod labels, only on the control plane port.
- Egress to cluster DNS on UDP/TCP 53.
- Extra CIDRs only when configured under `runtime.egress`.

The policy is only as good as the CNI that enforces it. The end-to-end suite (`TestNetworkIsolation`, [testing.md](testing.md)) runs on k3d, where k3s's kube-router controller enforces it, and checks each allowed and blocked path; run it against your cluster to confirm enforcement.

## Credential boundary

The session runtime never receives:

- the LLM API key,
- object-store credentials,
- HTTP MCP server credentials.

Model calls go through `/internal/v1/llm/chat/completions`. The broker checks that the requested model is the one assigned to the session, injects the configured key, and streams the response. A session token cannot choose a different allowlisted model. With LiteLLM, the broker holds only the LiteLLM master key; provider keys live in LiteLLM. Keep LiteLLM private and do not expose it through the Mayfly ingress.

Tool calls go through `/internal/v1/sessions/{id}/tools`. The gateway holds each session's tool grant, checks the call against it, records it in the ledger, and injects the upstream credential. HTTP `mcp.servers` credentials are read from the resolved configuration and never sent to the session. `* /internal/v1/mcp/{server}` still proxies HTTP MCP calls for alternate harnesses, which bypass the gateway and its ledger.

The session token is a random 32-byte value. It is stored in a per-session Kubernetes Secret and indexed by SHA-256 in the control plane. It authorizes only:

- writing events for that session,
- reading that session's inbox,
- uploading that session's artifact,
- finalizing that session,
- model calls,
- calling the tools granted to that session,
- HTTP MCP calls for servers assigned to that session.

The token is removed from the index when the session finalizes or is deleted.

## Agent manifests

Agent manifests are trusted control-plane configuration. The control plane parses them and builds session configuration; it never executes their code. Prompt files are confined to the manifest directory. Duplicate names and unknown fields are rejected.

A manifest image replaces the session container and must implement runtime protocol 1. Keep `agents.allowed_images` non-empty in shared clusters and pin images by digest. The image still receives the hardened pod context and only the scoped session token.

Remote skill archives are fetched by the control plane at startup, verified by SHA-256, and never fetched by sessions. `repo` + `ref` is only URL sugar for supported hosts; the control plane does not run git. Pin `ref` to a commit SHA and provide the archive digest. A missing digest or mismatch fails startup. Treat skill content as trusted instructions; it is Markdown, not executable code.

`harness` selects an executable from the image, normally `/opt/mayfly/harnesses/<name>`. Harness executables are trusted image content. A missing or non-executable selected harness fails the session; it does not fall back to Mayfly. An empty harness selects the built-in loop. Mayfly never installs harness packages at session start.

`runtime.command`, `runtime.args`, and `runtime.env` are trusted manifest configuration. They become pod settings. Do not accept them from API requests. Environment values are literal and visible in the pod spec; `MAYFLY_SESSION_TOKEN` and `MAYFLY_CONFIG` are reserved and rejected at startup if a manifest or `runtime.env` sets them. The pod builder also drops them, so a repeated env name cannot override the session token. Harness-specific data belongs in `runtime.config`, which the built-in runtime ignores. `runtime.config` is written to the session ConfigMap, so do not put secrets there either.

## Plugins

A plugin is an MCP server declared in the bundle. It is trusted operator code. The placement decides what it can reach and where its credentials live.

| Placement | Runs in | Credentials | Agent container can read them |
|---|---|---|---|
| `inline` | A child of the agent runtime | Files under `/var/run/mayfly/plugin-secrets/<name>/`, mode 0400 | Yes. The runtime passes them to that plugin only, but `run_command` runs in the same container, and a sibling inline plugin shares the UID |
| `sidecar` | Its own container in the session pod | Environment from `secretKeyRef` | No. The Secret is mounted only into the sidecar |
| `service` | Its own Deployment in `plugins.namespace` | Environment from `secretKeyRef` | No |
| `service.mcpServer` | A remote `mcp.servers` entry | Headers resolved in `mayflyd` | No |

Inline plugins share one process tree, one UID, and one container with the runtime. A plugin child gets no `MAYFLY_SESSION_TOKEN` in its environment, and the runtime clears its own dumpable flag at startup (`PR_SET_DUMPABLE` 0). A plugin therefore cannot read `/proc/<pid>/environ` or ptrace the runtime to recover the token. Two limits remain: one inline plugin can read another inline plugin's secret files, and code in the container can read any file the runtime can. Use `placement: sidecar` when two plugins' secrets must stay apart.

Call tokens:

- **Sidecar.** `mayflyd` derives one key per plugin from the session key (`HMAC(sessionKey, "mayfly/sidecar/" + plugin)`), writes it to the session Secret as `gateway-key-<plugin>`, and mounts only that item into that plugin's sidecar container. The key never reaches the agent container or another sidecar. Each sidecar also has its own socket directory at `/run/mayfly/plugin-sockets/<name>/`, so a plugin cannot unlink or listen on another plugin's socket. The token names the session, plugin, remote tool, idempotency key, argument hash, and expiry. The shim rejects a missing, expired, or mismatched token, a token minted for another plugin, and a second use of a key while the first call is in flight. It keeps the last 1024 completed keys and returns the stored result for a replay, so a dropped response does not run the tool twice.
- **Service.** The key is per plugin, in `Secret/mayfly-gateway-<name>` in `plugins.namespace`. The reconciler creates it once and does not rotate it. A `transport: http` service receives the key as `MAYFLY_GATEWAY_KEY` and may check the `Authorization: Bearer` header itself. The service NetworkPolicy is the required control in that mode. If someone deletes and recreates the Secret, the pods roll on the changed key hash, the shim re-reads its key file after a signature failure, and the gateway reads the Secret again after an upstream `401`, so a recreated key does not need a manual restart.

The `allowShell` rule: an agent with `allowShell: true` cannot select an `inline` plugin that declares `secrets`, because `run_command` runs in the same container and could read them. On the local backend the rule covers `sidecar` plugins too, because they run inline there. The error suggests `placement: sidecar` for the cluster, or `placement: service`.

Service network: a service plugin's NetworkPolicy admits ingress only from control-plane pods on the plugin port, and allows egress only to cluster DNS and `service.egress`. Session pods cannot reach a service; the gateway calls it.

Audit: the gateway writes one `call.begin` and one `call.done` record per call. Arguments and results are never logged; `plugins.audit_path` appends the records to a file with mode 0600.

What this does not do: sandbox plugin code, verify plugin image signatures, allow hostname egress, rotate the service gateway key, or apply the gateway to alternate harnesses. See [plugins.md](plugins.md) for the placement model.

## Trigger authentication

- `generic` and `datadog` triggers accept an HMAC signature or the shared `X-Mayfly-Token` header.
- `github` triggers accept only `X-Hub-Signature-256`. GitHub signs each delivery, so the static token is not accepted there; a bearer token can be replayed from logs.
- `slack` triggers verify `X-Slack-Signature` inside a 5-minute timestamp window.
- `allow_unsigned: true` disables verification. Use it only for local development.
- Every trigger has a token-bucket rate limit. A request spends a token only after its signature verifies, so unauthenticated traffic cannot exhaust the trigger. Requests over the limit get `429` before a session is created.
- `sessions.max_concurrent` caps total live sessions across every creation path.

## Webhook intake

A verified webhook becomes one queued job and is answered immediately. Replay protection uses the sender's delivery identifier (`X-GitHub-Delivery`, Slack `trigger_id`, or an `Idempotency-Key` header); a repeated delivery within `queue.dedupe_ttl` returns `202` with `duplicate: true` and stores no second job. Queued jobs are retried with backoff and buried after `queue.max_attempts`; `GET /api/v1/queue` shows the backlog and dead count. The queue stores the prompt and session options, so protect the queue backend like the rest of the session data: no public network access, authentication enabled, and no eviction policy that can drop keys.

## What is intentionally permissive

- The shell tool (`run_command`) is off by default. Set `sessions.allow_shell: true`, or `allowShell` in an agent manifest, when an agent needs it. The container, not the tool, is the security boundary.
- stdio MCP servers run as child processes in the container. Their environment cannot reference control-plane secrets; use an HTTP MCP server behind the proxy when credentials are needed.
- The workspace is shared by all attach clients and the agent. Anyone with an attach ticket or the admin token can influence the session.
- The web UI stores the admin token in browser localStorage.

## Known gaps before production

- Single tenant. There is no user identity, per-user authorization, or audit log beyond events.
- No TLS termination or authentication proxy. Put Mayfly behind an ingress with OIDC/SSO and TLS.
- Admin token is a shared bearer token. Rotate it and mount it from a Secret.
- Session history is in memory and is dropped after `sessions.retention` (`30m` by default). The journal keeps the completed steps of every turn of an active session in the control-plane database for resume, and is deleted when the session finalizes; the artifact bundle is the durable history. Artifact metadata stays available until the control plane restarts; the live thread does not.
- No image signature verification, admission policy, or runtime sandbox beyond the Kubernetes security context.
- No per-session egress allowlist beyond the configured static CIDRs and the control plane. Service plugins get their own NetworkPolicy, which also matches CIDRs only.
- Plugins are trusted code. An inline plugin runs in the agent container, a sidecar shares the session pod's kernel, and a service runs with its own credentials.
- A service plugin's gateway key is created once and not rotated. The shim accepts one key file.
- Alternate harnesses bypass the tool gateway, so their MCP calls have no ledger record and no resume protection.
- No artifact encryption at rest. Enable bucket encryption and access policies.
- NetworkPolicy enforcement depends on the CNI. Verify it in your cluster.
- No Kubernetes ResourceQuota/LimitRange management. Add them per namespace.
- Artifact bundle contents can include secrets the agent read from a tool. Treat artifacts as sensitive.

## Hardening checklist

- Run one `mayflyd` replica per cluster namespace.
- Use a dedicated namespace and a dedicated ServiceAccount for the control plane with the provided Role.
- Keep the agent ServiceAccount without RBAC bindings and with token automount disabled.
- Set `runtime.network_policy: true`.
- Store LLM, MCP, and S3 credentials in a Kubernetes Secret; never in the ConfigMap.
- Set `plugins.namespace` to a namespace you control, and keep the plugin RBAC there.
- Set `plugins.audit_path` on durable storage when you need a per-call record.
- Keep `agents.allowed_images` non-empty and pin plugin images by digest.
- Restrict admin API access at the ingress.
- Set short TTLs and enable idle timeout for untrusted triggers.
- Verify NetworkPolicy enforcement with your CNI.
- Use a private artifact bucket with encryption and lifecycle rules.
