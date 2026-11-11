# Release process

## Version policy

- Tags are `vMAJOR.MINOR.PATCH`.
- The chart version and app version match the tag without the `v`.
- Images are published as `ghcr.io/<owner>/mayflyd:<version>` and `ghcr.io/<owner>/mayfly-agent:<version>`.
- The chart is published to `oci://ghcr.io/<owner>/charts/mayfly`.
- 0.x minor releases may contain breaking changes. Patch releases should not.

## Pre-release checklist

1. Update `CHANGELOG.md` and the chart `version` and `appVersion` if they differ from the tag.
2. Run the full test path:

```bash
gofmt -l cmd internal test
go vet ./...
go test ./...
helm lint deploy/helm/mayfly --set existingSecret=mayfly-secrets
helm template mayfly deploy/helm/mayfly -n mayfly --set existingSecret=mayfly-secrets >/dev/null
goreleaser check
actionlint .github/workflows/*.yml
./hack/kind-up.sh
./hack/e2e-kind.sh
```

3. Build both images locally. Check that `mayfly version`, `mayflyd --version`, and `mayfly-agent --version` show the release version, commit, and date.
4. Verify the chart with a real install, including ingress/TLS or port-forward, secret mount, and a complete session.
5. Review the security notes and known limitations for accuracy.

## Tag

```bash
git tag -a v0.1.0 -m "Mayfly v0.1.0"
git push origin main --tags
```

The release workflow runs on `v*` tags:

- GoReleaser builds CLI archives, checksums, and the GitHub release.
- Docker Buildx publishes multi-arch control-plane and agent images with SBOM and provenance.
- Helm packages the chart, pushes it to GHCR OCI, and attaches it to the release.

## Post-release verification

```bash
docker pull ghcr.io/<owner>/mayflyd:0.1.0
docker pull ghcr.io/<owner>/mayfly-agent:0.1.0
helm show chart oci://ghcr.io/<owner>/charts/mayfly --version 0.1.0
```

Install from the published chart in a clean namespace, create a session, attach, close it, download the artifact, and verify the session pod is gone.

## Rollback

Images and charts are immutable by version. If a release is broken, publish a new patch version and update the chart. Do not retag an existing release.
