# Security

## Trust boundaries

1. **Trigger/API caller** — authenticated by an admin token or a per-trigger secret.
2. **Control plane** — holds the admin token, LLM key, MCP credentials, and object-store credentials.
3. **Session container** — untrusted by default. It holds one scoped session token and no long-lived credentials.
4. **Artifact store** — receives bundles from the control plane only.

## Session isolation defaults

The pod and container set:

- `runAsNonRoot`, UID/GID 65532, no privilege escalation, all capabilities dropped.
- `readOnlyRootFilesystem` with writable EmptyDir mounts at `/workspace`, `/var/lib/mayfly`, and `/tmp`.
- `seccompProfile: RuntimeDefault`.
- `automountServiceAccountToken: false`; `enableServiceLinks: false`.
- Restart policy `Never` and an `activeDeadlineSeconds` backstop.
- Resource requests/limits and EmptyDir size limits.

The NetworkPolicy sets:

- `Ingress: []` — nothing can reach the pod.
- Egress to the control plane namespace and pod labels, only on the control plane port.
- Egress to cluster DNS on UDP/TCP 53.
- Extra CIDRs only when configured under `runtime.egress`.

Kind's default CNI must be checked for NetworkPolicy enforcement. The included e2e test verifies that the cloud metadata endpoint is unreachable.

## Credential boundary

The session runtime never receives:

- the LLM API key,
- object-store credentials,
- HTTP MCP server credentials.

Model calls go through `/internal/v1/llm/chat/completions`. The broker checks that the requested model is the one assigned to the session, injects the configured key, and streams the response. A session token cannot choose a different allowlisted model. With LiteLLM, the broker holds only the LiteLLM master key; provider keys live in LiteLLM. Keep LiteLLM private and do not expose it through the Mayfly ingress. HTTP MCP calls go through `/internal/v1/mcp/{server}`. The proxy injects configured headers, including values resolved from `env:NAME`, and checks that the session was assigned that server.

The session token is a random 32-byte value. It is stored in a per-session Kubernetes Secret and indexed by SHA-256 in the control plane. It authorizes only:

- writing events for that session,
- reading that session's inbox,
- uploading that session's artifact,
- finalizing that session,
- model calls,
- HTTP MCP calls for servers assigned to that session.

The token is removed from the index when the session finalizes or is deleted.

## Agent manifests

Agent manifests are trusted control-plane configuration. The control plane parses them and builds session configuration; it never executes their code. Prompt files are confined to the manifest directory. Duplicate names and unknown fields are rejected.

A manifest image replaces the session container and must implement runtime protocol 1. Keep `agents.allowed_images` non-empty in shared clusters and pin images by digest. The image still receives the hardened pod context and only the scoped session token.

Remote skill archives are fetched by the control plane at startup, verified by SHA-256, and never fetched by sessions. `repo` + `ref` is only URL sugar for supported hosts; the control plane does not run git. Pin `ref` to a commit SHA and provide the archive digest. A missing digest or mismatch fails startup. Treat skill content as trusted instructions; it is Markdown, not executable code.

`harness` selects an executable from the image, normally `/opt/mayfly/harnesses/<name>`. Harness executables are trusted image content. A missing or non-executable selected harness fails the session; it does not fall back to Mayfly. An empty harness selects the built-in loop. Mayfly never installs harness packages at session start.

`runtime.command`, `runtime.args`, and `runtime.env` are trusted manifest configuration. They become pod settings. Do not accept them from API requests. Environment values are literal and visible in the pod spec; `MAYFLY_SESSION_TOKEN` and `MAYFLY_CONFIG` are reserved and rejected at startup if a manifest or `runtime.env` sets them. The pod builder also drops them, so a repeated env name cannot override the session token. Harness-specific data belongs in `runtime.config`, which the built-in runtime ignores. `runtime.config` is written to the session ConfigMap, so do not put secrets there either.

## Trigger authentication

- `generic` and `datadog` triggers accept an HMAC signature or the shared `X-Mayfly-Token` header.
- `github` triggers accept only `X-Hub-Signature-256`. GitHub signs each delivery, so the static token is not accepted there; a bearer token can be replayed from logs.
- `slack` triggers verify `X-Slack-Signature` inside a 5-minute timestamp window.
- `allow_unsigned: true` disables verification. Use it only for local development.
- Every trigger has a token-bucket rate limit. A request spends a token only after its signature verifies, so unauthenticated traffic cannot exhaust the trigger. Requests over the limit get `429` before a session is created.
- `sessions.max_concurrent` caps total live sessions across every creation path.

## What is intentionally permissive

- The shell tool (`run_command`) is off by default. Set `sessions.allow_shell: true`, or `allowShell` in an agent manifest, when an agent needs it. The container, not the tool, is the security boundary.
- stdio MCP servers run as child processes in the container. Their environment cannot reference control-plane secrets; use an HTTP MCP server behind the proxy when credentials are needed.
- The workspace is shared by all attach clients and the agent. Anyone with an attach ticket or the admin token can influence the session.
- The web UI stores the admin token in browser localStorage.

## Known gaps before production

- Single tenant. There is no user identity, per-user authorization, or audit log beyond events.
- No TLS termination or authentication proxy. Put Mayfly behind an ingress with OIDC/SSO and TLS.
- Admin token is a shared bearer token. Rotate it and mount it from a Secret.
- Session history is in memory and is dropped after `sessions.retention` (`30m` by default). Artifacts are durable, and their metadata stays available until the control plane restarts; the live thread does not.
- No image signature verification, admission policy, or runtime sandbox beyond the Kubernetes security context.
- No per-session egress allowlist beyond the configured static CIDRs and the control plane.
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
- Restrict admin API access at the ingress.
- Set short TTLs and enable idle timeout for untrusted triggers.
- Verify NetworkPolicy enforcement with your CNI.
- Use a private artifact bucket with encryption and lifecycle rules.
