# Security policy

## Supported versions

| Version | Supported |
|---|---|
| 0.3.x | Security fixes |
| 0.2.x | Security fixes |

0.1.0 was never released; it shipped only as release candidates.

## Reporting a vulnerability

Report vulnerabilities through GitHub private vulnerability reporting:

https://github.com/hayfever/mayfly/security/advisories/new

Do not open a public issue for an undisclosed vulnerability. Include:

- affected version or commit,
- deployment backend (k3d, other Kubernetes, local),
- reproduction steps or proof of concept,
- impact and any known mitigations.

You should receive an acknowledgement within a few business days. This is a volunteer project; there is no paid incident response or SLA.

## Scope

In scope: control-plane API, session isolation, credential handling, webhook verification, artifact store access, WebSocket authorization, and container escape through the documented configuration.

Out of scope: vulnerabilities in Kubernetes, the CNI, the container runtime, LiteLLM, model providers, MCP servers, or third-party harness images.

## Security model

Mayfly is single-tenant. The `/api/v1` surface uses named bearer tokens under `server.tokens`, each with one or more scopes. A token with the `admin` scope can create sessions, read threads, and download artifacts. A scoped token can use only the routes for its scopes. Deploy Mayfly behind TLS and an authenticating ingress.

When `server.tokens` is empty, `server.token_env` supplies one token named `default` with the `admin` scope. A non-empty `server.tokens` list replaces that token. The control plane refuses to start when no token is configured. Each route requires one scope: `sessions:read`, `sessions:write`, `queue:read`, `queue:write`, `schedules:read`, or `schedules:write`. The `admin` scope grants every scope. The control plane compares secrets in constant time and never logs them.

Every authenticated, mutating `/api/v1` request writes one audit line with the token name, method, route, target, status, and request id. A mutating request that the token is not allowed to make is audited with status `403`. Read requests and requests with an unknown token are not audited.

The control plane holds LLM, MCP, and object-store credentials. Sessions receive only a scoped session token. The token authorizes that session's inbox, events, turn journal, mounted input artifacts, artifact upload, finalize, LLM proxy, granted tool calls through the tool gateway, and assigned MCP servers.

Session pods run as non-root with a read-only root filesystem, no added capabilities, no service-account token, and a NetworkPolicy that denies ingress. Verify that your CNI enforces NetworkPolicy.

Remote skills, agent manifests, harness images, plugin images, and MCP servers are trusted operator configuration. Review them before use. Mayfly does not install harness packages at session start; plugin code comes only from the images that plugin manifests declare.

Known gaps are documented in [docs/security.md](docs/security.md) and [CHANGELOG.md](CHANGELOG.md).
