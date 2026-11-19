# Release process

## Version policy

- Tags are `vMAJOR.MINOR.PATCH`, or `vMAJOR.MINOR.PATCH-rc.N` for a release candidate.
- The chart version and app version match the tag without the `v`.
- Images are published as `ghcr.io/<owner>/mayflyd:<version>` and `ghcr.io/<owner>/mayfly-agent:<version>`.
- The chart is published to `oci://ghcr.io/<owner>/charts/mayfly`.
- 0.x minor releases may contain breaking changes. Patch releases should not.

## API compatibility

`api/openapi.yaml` describes the admin API at `/api/v1` and the webhook receivers at `/hooks/{name}`. Before 1.0.0 a breaking change may ship in a minor release. From 1.0.0 it needs a major version. The session runtime protocol at `/internal/v1` is versioned with the runtime image and is not covered.

The `api-diff` CI job runs `hack/api-diff.sh` on every pull request. The script uses a pinned oasdiff release and fails on a breaking change between the latest `v*` tag and the working tree.

```bash
hack/api-diff.sh            # latest v* tag
hack/api-diff.sh v0.2.0     # a tag, branch, or commit
```

If the base ref has no `api/openapi.yaml`, the script prints a notice and exits 0. That is the case for a tag that predates the spec.

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
hack/api-diff.sh
make e2e
E2E_QUEUE=redis make e2e
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

## Release candidates

Tag a candidate to exercise the full release path before the final tag:

```bash
git tag -a v0.1.0-rc.1 -m "Mayfly v0.1.0-rc.1"
git push origin v0.1.0-rc.1
```

The same workflow runs. GoReleaser marks the GitHub release as a pre-release (`prerelease: auto`), images are tagged `0.1.0-rc.1` without `latest` or `0.1`, and the chart is pushed as `0.1.0-rc.1`. Install it with `--version 0.1.0-rc.1`. When the candidate passes, tag `v0.1.0` on the same commit.

## Post-release verification

```bash
docker pull ghcr.io/<owner>/mayflyd:0.1.0
docker pull ghcr.io/<owner>/mayfly-agent:0.1.0
helm show chart oci://ghcr.io/<owner>/charts/mayfly --version 0.1.0
```

Install from the published chart in a clean namespace, create a session, attach, close it, download the artifact, and verify the session pod is gone.

## Rollback

Images and charts are immutable by version. If a release is broken, publish a new patch version and update the chart. Do not retag an existing release.
