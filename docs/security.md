# Security

## Trust boundaries

1. **Trigger/API caller** — authenticated by a scoped API token or a per-trigger secret.
2. **Control plane** — holds the API token secrets, LLM key, MCP credentials, and object-store credentials.
3. **Session container** — untrusted by default. It holds one scoped session token and no long-lived credentials.
4. **Plugin workload** — trusted operator code, but not trusted with the agent's data: an inline plugin shares the agent container, a sidecar shares the session pod, and a service is its own Deployment.
5. **Artifact store** — receives bundles from the control plane only.

## Threat model

This model covers one deployment in one namespace. Kubernetes RBAC and the
namespace are the administrative boundary: a subject that can read Secrets or
exec into pods there is outside every control below. Accepted risks are marked
**Accepted**.

### Assets

| Asset | Where it lives | Impact if disclosed |
|---|---|---|
| API tokens | Secrets read into `server.tokens` at startup | Use of the token's scopes on every session |
| Session tokens | Per-session Secret, mounted only into the session container | Control of one session: events, inbox, artifact upload, granted tools, model, MCP calls |
| LLM and MCP credentials | Control-plane environment; LiteLLM holds provider keys | Provider spend and data access, and MCP tool access |
| Plugin gateway keys | Per-session Secret for a sidecar; `mayfly-gateway-<name>` for a service | Calls the plugin accepts as gateway calls |
| Workspace data | Session PVC or EmptyDir, shared with attach clients | Session files, including tool output |
| Artifacts | Bundle in the local volume or S3 bucket | Conversation, events, traces, files, and any secret a tool returned |
| Control-plane state | `queue.db`, `journal.db`, `sessions.db`, `schedule.db` on the state volume | Prompts, options, event history, call records, session metadata |

### Attackers

| Attacker | Entry point |
|---|---|
| External webhook sender | `POST /hooks/{name}`, with a trigger secret or without one |
| Holder of a scoped API token | `Authorization: Bearer` on `/api/v1` |
| Compromised or malicious agent session | Code in the session container, including a prompt that turns the agent |
| Malicious plugin | Operator-declared plugin code in any placement |
| Cluster user with namespace access | Secret read, pod exec, or Deployment patch in the namespace |
| Network attacker inside the cluster | Packets to Services and traffic that policy does not block |

### External webhook sender

A verified delivery becomes one queue job. A repeated delivery key is deduped.
The sender cannot address a session, artifact, or Secret.

| Asset | Control today | Residual risk |
|---|---|---|
| API tokens | Webhook routes do not accept or return API tokens (`internal/httpapi/server.go`). | None beyond a compromised control plane. |
| Session tokens | Created when the job runs and kept in a per-session Secret. The accepted response is `{accepted, duplicate, job_id, delivery, status}`. | A sender who can read the queue or state volume gets the prompt and options, not the token. |
| LLM and MCP credentials | Not in the webhook path. The broker injects them. | None. |
| Plugin gateway keys | Created with the session. | None. |
| Workspace and artifacts | Not addressable by the webhook. | A verified delivery starts real work with an arbitrary prompt. Prompt injection is not a webhook defect. |
| Control-plane state | Body capped by `server.webhook_max_body_bytes` (default 1 MiB) or the trigger's `max_body_bytes`, checked before verification; the rate limit spends only after verification; `queue.max_jobs` rejects a full queue (`internal/httpapi/webhook.go`). | Each accepted job uses queue space and a session slot. `sessions.max_concurrent` bounds live sessions. |

- The generic receiver accepts `agent`, `model`, `ttl`, `skills`, and
  `mcp_servers` from the body. Values must resolve against configured agents,
  models, skills, and MCP servers, and `ttl` is clamped to `sessions.max_ttl`
  (`internal/triggers/triggers.go`, `internal/session/manager.go`). A generic
  trigger secret therefore starts work with any configured combination, not
  only the trigger defaults. **Accepted**: the trigger secret is the
  credential.
- Generic and Datadog signatures cover the body, not a timestamp. A captured
  request creates another session unless the sender sets `Idempotency-Key`.
  GitHub replay is deduped by `X-GitHub-Delivery`, and Slack rejects a request
  older than five minutes (`internal/triggers/triggers.go`). Use TLS at the
  ingress.
- `allow_unsigned: true` disables verification. The chart ships no trigger,
  and the local overlay uses the flag on localhost only.

### Holder of a scoped API token

A token acts on every session its scopes reach. There is no user identity.

| Asset | Control today | Residual risk |
|---|---|---|
| API tokens | Each route requires one scope (`internal/httpapi/server.go`). Secrets compare in constant time, and an expired token is refused (`internal/httpapi/auth.go`, `internal/config/tokens.go`). A mutating call writes one audit line with the token name. | No revocation API. Change the Secret and restart the control plane. `expires_at` is optional. Reads are not audited. |
| Browser session | `POST /api/v1/auth/session` exchanges a token for an in-memory session. The cookie is HttpOnly and SameSite=Strict, and Secure over https. A mutating cookie request needs `X-Mayfly-CSRF` and a matching Origin. The session lasts at most 12 hours, capped by the token expiry (`internal/httpapi/websession.go`). | Script in the page can act for the session. A restart ends every session. Sign-out is the only per-session revocation. |
| Session tokens | Not in any API response. Attach uses a one-time ticket that expires after `server.attach_ticket_ttl` (60s default) (`internal/httpapi/server.go`). | A `sessions:write` token mints a ticket for any session. |
| LLM and MCP credentials | Not in any API response. A token can create a session with a configured model and MCP server. | No per-token budget or quota. A `sessions:write` token spends the deployment LLM budget. |
| Plugin gateway keys | Not exposed. | None. |
| Workspace and artifacts | `sessions:read` downloads any bundle. `sessions:write` plus a ticket joins any session and can drive its workspace. | Scopes apply across sessions; there is no per-session ACL. **Accepted** for a single tenant. |
| Control-plane state | `queue:read` returns a job prompt on a single lookup. `queue:write` retries or deletes dead jobs. `schedules:write` starts a schedule. | Scope, not identity, decides access. |

- The `admin` scope grants every scope. The legacy `server.token_env` token is
  admin. Prefer named tokens with narrow scopes and `expires_at`.
- The web UI exchanges the token for an `HttpOnly`, `SameSite=Strict` session
  cookie, so page script cannot read the token or the session id. A mutating
  cookie request must echo the CSRF cookie and pass the Origin check. An XSS
  can still act through the page until the session ends, and a shared browser
  profile shares the session.

### Compromised or malicious agent session

The container is untrusted. Its token is scoped to its own session.

| Asset | Control today | Residual risk |
|---|---|---|
| API tokens | `/api/v1` authenticates only configured API tokens, so a session token is not accepted there (`internal/httpapi/auth.go`). | None, unless an operator puts an API token in `runtime.env`. Do not do that. |
| Session tokens | 32 random bytes, indexed by SHA-256, mapped to one session, and removed on finalize or delete (`internal/session/manager.go`). Each internal handler that names a session compares it with the path id. | It can write false events, upload an arbitrary artifact, and finalize early for its own session. |
| LLM and MCP credentials | Not in the container. The broker pins the model and injects the key. The MCP proxy checks that the server is assigned (`internal/broker/broker.go`, `internal/httpapi/internal.go`). | It spends its assigned model and uses its assigned MCP servers, and reads every tool result. |
| Plugin gateway keys | No gateway key in the agent container. A sidecar key is mounted only into its own sidecar. An inline plugin secret is a file in the agent container. | Any code in the agent container reads inline plugin secrets. `allowShell` with an inline plugin that declares secrets is refused (`internal/session/agents.go`). |
| Workspace and artifacts | It owns its workspace and can upload one artifact for its own session. Artifact reads are scoped to the session (`internal/session/manager.go`). | It can send anything it reads to the control plane and the `runtime.egress` CIDRs, and can put secrets in the bundle. |
| Control-plane state | It writes events and journal entries for its own session. | The control plane cannot tell a compromised session's events from honest ones. |

- The pod runs non-root with a read-only root filesystem, no service account
  token, no capabilities, and `RuntimeDefault` seccomp. When
  `runtime.network_policy` is on, the NetworkPolicy denies ingress and permits
  egress to the control plane, cluster DNS, and `runtime.egress` only
  (`internal/k8s/spec.go`). Enforcement depends on the CNI.
- `run_command` is off unless `sessions.allow_shell` or an agent `allowShell`
  enables it. The container is the boundary, not the tool.
- There is no sandbox beyond the Kubernetes security context and seccomp. A
  kernel or container runtime escape is out of scope.
- Attach clients share the workspace with the agent. A compromised session can
  influence them, and they can influence it.

### Malicious plugin

A plugin is trusted operator code. Its placement decides what it can reach.

| Asset | Control today | Residual risk |
|---|---|---|
| API tokens | Not in scope. | None. |
| Session tokens | Not passed to a plugin process. Inline plugins share the UID, but the runtime clears its dumpable flag, so a plugin cannot read `/proc/<pid>/environ` or ptrace the runtime (`cmd/mayfly-agent/main.go`). Sidecar and service plugins run in their own containers. | Inline code reads any file the runtime reads. The token is in the environment, not a file. |
| LLM and MCP credentials | Only the plugin's declared secrets: sidecar and service environment from `secretKeyRef`, inline files under `/var/run/mayfly/plugin-secrets/<name>/` with mode 0400. | An inline plugin reads a sibling inline plugin's secret files. Use `placement: sidecar` to separate secrets. |
| Plugin gateway keys | One derived key per sidecar, mounted only into that sidecar. One Secret per service. A call token names the session, plugin, tool, argument hash, and expiry (`internal/gateway/token.go`). | A service key is created once and not rotated. |
| Workspace and artifacts | An inline plugin runs in the agent container. A sidecar mounts the workspace only when the manifest sets `sidecar.workspace`; a service never mounts it. | An inline plugin has full access to the session data. |
| Control-plane state | Not mounted. | None. |

- Mayfly does not sandbox plugin code or verify image signatures. It can
  enforce `agents.allowed_images`; pin plugin images by digest.
- A service plugin has its own credentials and egress network. The gateway
  applies the grant and ledger, but the plugin runs outside the session pod.
- An alternate harness bypasses the tool gateway, so its MCP calls have no
  ledger record and no resume protection.

### Cluster user with namespace access

Kubernetes RBAC is the boundary. Mayfly has no control above it.

| Asset | Control today | Residual risk |
|---|---|---|
| All assets above | The control-plane Role grants only the namespace objects it manages (`deploy/k8s/base/rbac.yaml`). The session ServiceAccount has no bindings and no mounted token. | A subject that can read Secrets, read the state PVC, or exec into a control-plane pod has every asset: API tokens and webhook secrets, LLM and MCP keys, session tokens, and workspace or artifact data. Exec into a session pod gives that session's token and workspace. |
| Configuration and images | `agents.allowed_images` limits agent and plugin images. | A subject that can patch the control-plane Deployment can change the image, the config, and which Secrets the pod mounts. |

- A subject that can read a session Secret can act as that session.
- **Accepted**: cluster administrators and every permission the cluster grants
  them are trusted. Run one control plane per namespace and per trust
  boundary. Do not colocate unrelated tenants in that namespace.

### Network attacker inside the cluster

Bearer tokens cross the internal listener without TLS by default.

| Asset | Control today | Residual risk |
|---|---|---|
| API tokens, session tokens | Token authentication on every route. When `runtime.network_policy` is on, session pods have default-deny ingress. | Mayfly does not terminate TLS. A network attacker that can observe the internal listener captures any bearer token in it. |
| Plugin gateway keys | A gateway key stays in the control plane or the plugin workload. The gateway sends a signed call token, not the key. | A captured call token is bound to one plugin, tool, argument hash, and expiry. The shim rejects a second use while the first call is in flight, and replays the stored result for a completed key. |
| LLM and MCP credentials | Held in the control plane. Model calls go to `llm.base_url` (HTTPS in the base config). HTTP MCP headers are resolved in the control plane. | If `llm.base_url` or an `mcp.servers` URL is plain HTTP, an in-cluster attacker can capture the credential the control plane sends to it. |
| Workspace and artifacts | Session egress is limited to the control plane, DNS, and `runtime.egress` (`internal/k8s/spec.go`). | Artifact upload crosses the internal listener. A network attacker can read artifact bytes and tool traffic in flight. |
| Service plugin calls | The service NetworkPolicy admits only control-plane pods on the plugin port. | A captured call token is bound to one plugin, tool, argument hash, and expiry. It cannot call another tool or another plugin. |

- Neither the base manifests nor the chart define a NetworkPolicy for
  `mayflyd`. No cluster policy restricts the Service, so any pod that the
  cluster network allows can reach the API and must present a token. Add a
  policy that admits only session pods, the ingress controller, and your
  operators.
- Session pods have default-deny ingress and restricted egress, but the CNI
  must enforce the policy. Verify it with the end-to-end isolation test
  ([testing.md](testing.md#against-a-real-cluster)).
- Use TLS at the ingress for external clients. Inside the cluster, run on a
  network you trust or add mTLS or a service mesh. Mayfly does not provide it.

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

## API authentication

The `/api/v1` surface uses named bearer tokens. Declare them under `server.tokens`:

```yaml
server:
  tokens:
    - name: ci
      scopes: [sessions:read, sessions:write]
      secret_env: MAYFLY_CI_TOKEN
      expires_at: "2027-01-01T00:00:00Z"
```

The control plane reads each secret from `secret_env` at startup. A missing variable stops startup. `expires_at` is optional, and an expired token is refused.

When `server.tokens` is empty, the control plane uses `server.token_env` (`MAYFLY_API_TOKEN` by default) as one token named `default` with the `admin` scope. A non-empty `server.tokens` replaces that token. Do not configure both and expect two credentials.

Each route requires one scope. The `admin` scope grants every scope.

| Scope | Routes |
|---|---|
| `sessions:read` | `GET /api/v1/sessions`, `GET /api/v1/sessions/{id}`, `GET /api/v1/sessions/{id}/events`, `GET /api/v1/sessions/{id}/artifact`, `GET /api/v1/config` |
| `sessions:write` | `POST /api/v1/sessions`, `DELETE /api/v1/sessions/{id}`, `POST /api/v1/sessions/{id}/messages`, `POST /api/v1/sessions/{id}/close`, `POST /api/v1/sessions/{id}/attach-ticket` |
| `queue:read` | `GET /api/v1/queue`, `GET /api/v1/queue/jobs/{id}`, `GET /api/v1/queue/dead` |
| `queue:write` | `POST /api/v1/queue/dead/{id}/retry`, `DELETE /api/v1/queue/dead/{id}` |
| `schedules:read` | `GET /api/v1/schedules` |
| `schedules:write` | `POST /api/v1/schedules/{name}/run` |

The `/api/v1/auth/session` routes take no token scope. `POST` exchanges a token for the session cookie. `GET` and `DELETE` act on the cookie alone.

The control plane compares every configured token with the presented bearer value in constant time. It never logs a token secret. The token name appears in the audit log.

A request without a token, or with an unknown or expired token, gets `401` with code `unauthenticated`. A valid token without the scope of the route gets `403` with code `forbidden`. The attach WebSocket uses a one-time ticket instead of a bearer token.

## Web UI session

The web UI sends its API token once to `POST /api/v1/auth/session` and receives two cookies. The token secret is never written to browser storage, and the page keeps no copy after the form closes.

| Cookie | Attributes | Purpose |
|---|---|---|
| `mayfly_session` | `Path=/`, `HttpOnly`, `SameSite=Strict`, `Secure` over https, `Max-Age` up to 12 hours | Identifies the session. JavaScript cannot read it. |
| `mayfly_csrf` | `Path=/`, `SameSite=Strict`, `Secure` over https, `Max-Age` up to 12 hours | Double-submit value. JavaScript reads it and copies it into the request header. |

- The store keeps sessions in memory, keyed by the SHA-256 of a 32-byte random id. The raw id exists only in the cookie. The map holds at most 1024 sessions and evicts the oldest.
- A session expires after 12 hours, or earlier when the token's `expires_at` is sooner. A control-plane restart drops every session, so the operator signs in again. The chart runs one replica.
- `GET /api/v1/auth/session` reports the session name, scopes, and expiry. `DELETE /api/v1/auth/session` ends it and expires both cookies.
- `GET /api/v1/auth/session` uses only the cookie. A bearer token does not authenticate it, and a bearer token alone cannot end a session.

The cookie is accepted in place of `Authorization: Bearer` on every `/api/v1` operation; the scopes of the token that created the session still apply. A request that carries a Bearer header is authenticated by that header alone, so a wrong header is not rescued by a valid cookie.

A cookie-authenticated mutating request (`POST`, `PUT`, `PATCH`, `DELETE`) must send `X-Mayfly-CSRF` with the `mayfly_csrf` value. When the request carries an `Origin` header, its host must match the configured `server.public_url` or the request host, and an https `server.public_url` requires an https origin. A failure returns `403` with code `forbidden` and message `csrf check failed`. Bearer requests are exempt, because a browser cannot attach the header on its own.

## Audit log

The control plane writes one audit line for every authenticated, mutating `/api/v1` request: `POST`, `PUT`, `PATCH`, and `DELETE`. This includes a request that a valid token is not allowed to make, which is logged with status `403`. The operator actions in this API are all mutating requests.

The line uses the message `audit` at info level. Its fields are:

- `token_name`: the configured token name. The secret never appears.
- `via`: `bearer` for an `Authorization` header, or `web` for a browser session cookie.
- `method` and `route`: the HTTP method and the route pattern, for example `/api/v1/sessions/{id}`.
- `target`: the session id, schedule name, or job id. A failed request can have an empty target.
- `status`: the response status.
- `request_id`: the caller `X-Request-Id`, or a generated id. The response echoes it in `X-Request-Id`.

Read requests are not audited. A request with an unknown token is not audited, because it has no token name. A failed `POST /api/v1/auth/session` is audited with `token_name` set to `unknown`, and a successful one with the token name and status `201`.

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
- The workspace is shared by all attach clients and the agent. Anyone with an attach ticket or a token with `sessions:write` can influence the session.
- The web UI keeps its session on the control plane, in memory. A restart ends
  every browser session, so the operator signs in again. The API token is not
  stored in the browser.

## Known gaps before production

- Single tenant. There is no user identity or per-user authorization. Tokens carry scopes, not users. The tool gateway logs one record per non-built-in tool call, without arguments or results. Built-in tool calls are not audited.
- No TLS termination or authentication proxy. Put Mayfly behind an ingress with OIDC/SSO and TLS.
- The token from `server.token_env` has the admin scope. Prefer named tokens with narrow scopes, rotate each secret, and mount the secrets from a Secret.
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
