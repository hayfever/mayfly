# Contributing

## Development

Requirements: Go 1.26+, Docker, k3d, kubectl, Helm, jq, and openssl. The quickstart cluster and the end-to-end suite both run on k3d.

```bash
make build
make test
make run-local
```

`make run-local` starts the control plane and deterministic LLM and MCP stubs. The token is `dev-token` unless `MAYFLY_API_TOKEN` is set. Port 8080 conflicts with the quickstart cluster's port mapping; stop the cluster or change the local config.

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

Check the API gate when you touch the spec, a handler, or a response shape:

```bash
hack/api-diff.sh
```

`make e2e` ([docs/testing.md](docs/testing.md)) is required for changes to session lifecycle, Kubernetes objects, networking, artifacts, or the agent image.

## API changes

[`api/openapi.yaml`](api/openapi.yaml) is the source of truth for `/api/v1` and `/hooks/{name}`. Start a change in the spec.

- Every `/api/v1` route needs an operation. The spec test fails when the router and the spec disagree in either direction.
- Every failed request uses the error envelope with a code from the documented set.
- Every list route returns `items` and an optional `next_cursor`.
- `/internal/v1` is the session runtime protocol. It is not in the spec and keeps its own shapes.

`hack/api-diff.sh` compares the spec with the latest `v*` tag and fails a breaking change. A deliberate break before 1.0.0 still needs a changelog entry.

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
