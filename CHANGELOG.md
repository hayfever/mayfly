# Changelog

All notable changes are recorded here. The project follows semantic versioning once it reaches 1.0.0. Before that, minor versions may contain breaking changes; patch versions should not.

## v0.1.0 - 2026-11-11

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
- Versioned CLI, control-plane, and agent builds with commit and date metadata.
- Kind e2e test covering webhook creation, pod isolation, MCP tools, WebSocket replay, close, artifacts, and TTL expiry.
- CI and release workflows: tests, Helm lint, kind e2e, multi-arch GHCR images, SBOMs, provenance, CLI archives, and OCI chart publishing.
- S3 artifact export can use the default AWS credential chain, including IAM roles for service accounts.

### Changed

- The module path and published images use `github.com/hayfever/mayfly` and `ghcr.io/hayfever`.
- The S3 store sends checksums only when required, for compatibility with R2, B2, and older MinIO.

### Known limitations

- The control plane runs one replica. Session records and thread history are in memory; artifacts are durable.
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

### Security

- Sessions receive one scoped token that expires with the session.
- LLM and MCP credentials are injected by the control plane.
- The default NetworkPolicy requires a CNI that enforces it. The kind e2e test verifies enforcement.
