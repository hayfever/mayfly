# Release process

## Version policy

- Tags are `vMAJOR.MINOR.PATCH`, or `vMAJOR.MINOR.PATCH-rc.N` for a release candidate.
- The chart version and app version match the tag without the `v`.
- Images are published as `ghcr.io/<owner>/mayflyd:<version>` and `ghcr.io/<owner>/mayfly-agent:<version>`.
- The chart is published to `oci://ghcr.io/<owner>/charts/mayfly`.
- 0.x minor releases may contain breaking changes. Patch releases should not.
- From 1.0.0, a removal waits for a major version. See [deprecation.md](deprecation.md).
- Upgrade, backup, and recovery steps are in [runbook.md](runbook.md).

## API compatibility

`api/openapi.yaml` describes the admin API at `/api/v1` and the webhook receivers at `/hooks/{name}`. Before 1.0.0 a breaking change may ship in a minor release. From 1.0.0 it needs a major version. The session runtime protocol at `/internal/v1` is versioned with the runtime image and is not covered.

The `api-diff` CI job runs `hack/api-diff.sh` on every pull request. The script uses a pinned oasdiff release and fails on a breaking change between the latest `v*` tag and the working tree.

```bash
hack/api-diff.sh            # latest v* tag
hack/api-diff.sh v0.2.0     # a tag, branch, or commit
```

If the base ref has no `api/openapi.yaml`, the script prints a notice and exits 0. That is the case for a tag that predates the spec.

## Supply chain

CI runs `govulncheck ./...` in the `vulncheck` job. The job fails when the module graph reaches a known vulnerability in our code paths. Fix it by bumping the dependency in `go.mod`. The scan runs with the latest vulnerability database, so a new advisory can fail a pull request that changes nothing.

The release workflow builds each image, scans it, tags it, and signs it in that order:

1. Docker Buildx builds the multi-arch image and pushes the manifest and layers by digest, without a tag.
2. `trivy image` scans that digest for `linux/amd64` and `linux/arm64` with `--severity HIGH,CRITICAL --ignore-unfixed --exit-code 1`. A HIGH or CRITICAL vulnerability that has a fixed version fails the job. A finding without a fix is reported and does not block.
3. Only a clean scan creates the version, `major.minor`, and `latest` tags with `docker buildx imagetools create`.
4. `cosign sign` signs the digest with the workflow's GitHub OIDC identity. No key and no stored secret is involved.
5. The chart job waits for both images, pushes the chart to GHCR, and signs it with the same identity.

A failed scan fails the job before any tag exists, so the release has no image tag and no signature. The untagged manifest is not a release artifact, and the registry garbage collector removes it. A tag that does not verify is not a release artifact.

### Verify a published release

```bash
owner=<owner>
version=0.2.0
identity="https://github.com/${owner}/mayfly/.github/workflows/release.yml@refs/tags/v${version}"

cosign verify \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/${owner}/mayflyd:${version}

cosign verify \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/${owner}/mayfly-agent:${version}

cosign verify \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/${owner}/charts/mayfly:${version}
```

Each command must print the workflow identity and the tag. The OIDC issuer is the only trust anchor; there is no public key to fetch. The images also carry BuildKit SBOM and provenance attestations. Check the provenance with the same identity flags and `cosign verify-attestation --type slsaprovenance`.

## Pre-release checklist

1. Update `CHANGELOG.md` and the chart `version` and `appVersion` if they differ from the tag.
2. Run the full test path:

```bash
gofmt -l cmd internal test e2e
go vet ./...
go vet -tags e2e ./e2e
go test ./...
govulncheck ./...
helm lint deploy/helm/mayfly --set existingSecret=mayfly-secrets
helm template mayfly deploy/helm/mayfly -n mayfly --set existingSecret=mayfly-secrets >/dev/null
helm lint deploy/helm/mayfly -f e2e/values.yaml
goreleaser check
actionlint .github/workflows/*.yml
hack/api-diff.sh
make e2e
E2E_QUEUE=redis make e2e
```

The `vulncheck` job installs govulncheck v1.10.0. Install the same version locally for the same result:

```bash
go install golang.org/x/vuln/cmd/govulncheck@v1.10.0
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
- Docker Buildx builds multi-arch control-plane and agent images with SBOM and provenance, and pushes them by digest without tags.
- Trivy scans each digest for both architectures and fails the job on a HIGH or CRITICAL vulnerability that has a fix.
- Buildx creates the image tags after a clean scan. Cosign then signs each digest with the workflow identity, and the chart job pushes and signs the chart.

## Release candidates

Tag a candidate to exercise the full release path before the final tag:

```bash
git tag -a v0.1.0-rc.1 -m "Mayfly v0.1.0-rc.1"
git push origin v0.1.0-rc.1
```

The same workflow runs. GoReleaser marks the GitHub release as a pre-release (`prerelease: auto`), images are tagged `0.1.0-rc.1` without `latest` or `0.1`, and the chart is pushed as `0.1.0-rc.1`. Install it with `--version 0.1.0-rc.1`. When the candidate passes, tag `v0.1.0` on the same commit.

## Post-release verification

Run the commands in [Verify a published release](#verify-a-published-release) first. All three must print the workflow identity and the tag.

```bash
docker pull ghcr.io/<owner>/mayflyd:0.1.0
docker pull ghcr.io/<owner>/mayfly-agent:0.1.0
helm show chart oci://ghcr.io/<owner>/charts/mayfly --version 0.1.0
```

Install from the published chart in a clean namespace, create a session, attach, close it, download the artifact, and verify the session pod is gone.

## Rollback

Images and charts are immutable by version. If a release is broken, publish a new patch version and update the chart. Do not retag an existing release.
