# Security policy

## Supported versions

| Version | Supported |
|---|---|
| 0.1.x | Security fixes |

## Reporting a vulnerability

Report vulnerabilities through GitHub private vulnerability reporting:

https://github.com/hayfever/mayfly/security/advisories/new

Do not open a public issue for an undisclosed vulnerability. Include:

- affected version or commit,
- deployment backend (kind, other Kubernetes, local),
- reproduction steps or proof of concept,
- impact and any known mitigations.

You should receive an acknowledgement within a few business days. This is a volunteer project; there is no paid incident response or SLA.

## Scope

In scope: control-plane API, session isolation, credential handling, webhook verification, artifact store access, WebSocket authorization, and container escape through the documented configuration.

Out of scope: vulnerabilities in Kubernetes, the CNI, the container runtime, LiteLLM, model providers, MCP servers, or third-party harness images.

## Security model

Mayfly is single-tenant. Anyone with the admin token can create sessions, read threads, and download artifacts. Deploy it behind TLS and an authenticating ingress.

The control plane holds LLM, MCP, and object-store credentials. Sessions receive only a scoped session token. The token authorizes that session's inbox, events, artifact upload, finalize, LLM proxy, and assigned MCP servers.

Session pods run as non-root with a read-only root filesystem, no added capabilities, no service-account token, and a NetworkPolicy that denies ingress. Verify that your CNI enforces NetworkPolicy.

Remote skills, agent manifests, harness images, and MCP servers are trusted operator configuration. Review them before use. Mayfly does not install harness packages or run plugin code at session start.

Known gaps are documented in [docs/security.md](docs/security.md) and the v0.1.0 changelog.
