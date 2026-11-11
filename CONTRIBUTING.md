# Contributing

## Development

Requirements: Go 1.26+, Docker, kind, kubectl, Helm, jq, and openssl for the full test path.

```bash
make build
make test
make run-local
```

`make run-local` starts the control plane and deterministic LLM and MCP stubs. The token is `dev-token` unless `MAYFLY_API_TOKEN` is set. Port 8080 conflicts with the kind port mapping; stop kind or change the local config.

Full cluster test:

```bash
make e2e
```

## What to run before a pull request

```bash
gofmt -w cmd internal test
go vet ./...
go test ./...
helm lint deploy/helm/mayfly --set existingSecret=mayfly-secrets
goreleaser check
```

`make e2e` ([docs/testing.md](docs/testing.md)) is required for changes to session lifecycle, Kubernetes objects, networking, artifacts, or the agent image.

## Architecture rules

- Keep the control plane and session runtime separated by the `/internal/v1` HTTP contract.
- Credentials stay in the control plane. Do not add provider or object-store keys to session pods.
- Keep isolation defaults intact. Document and justify any exception.
- Lifecycle transitions must be idempotent; runtime and control plane races are expected.
- Prefer standard library APIs. Current external dependencies are client-go, gorilla/websocket, yaml.v3, and the AWS SDK only if a provider adapter needs it.

## Commits

Use focused commits with a one-line summary and a short body explaining why. Keep formatting-only changes separate. Do not mix generated files, dependency bumps, and behavior changes.

## Security

Do not include real credentials, tokens, private keys, or customer data in issues, commits, tests, or fixtures. Report vulnerabilities through [SECURITY.md](SECURITY.md), not a public issue.

## Pull requests

Explain the behavior change, the failure modes, and how you verified it. Add or update tests for lifecycle, boundaries, and error paths. Update docs and the changelog for user-visible changes.
