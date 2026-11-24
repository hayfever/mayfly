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

The `api-diff` CI job runs `hack/api-diff.sh` on every pull request. The script uses a pinned oasdiff release and fails on a breaking change between the latest `v*` tag and the working tree. A new value in a response enum, such as a new error `code`, is a warning, not a failure; a removed value still fails.

```bash
hack/api-diff.sh            # latest v* tag
hack/api-diff.sh v0.2.0     # a tag, branch, or commit
```

If the base ref has no `api/openapi.yaml`, the script prints a notice and exits 0. That is the case for a tag that predates the spec.

## Release gate

Every `v*` tag runs the `gate` job before any other job, release candidates included. The gate passes only when the tagged commit has both of these:

- A successful `ci` workflow run for the commit.
- A successful `e2e-stability` workflow run with `runs=20` for the same commit.

The gate reads the commit's workflow history from the GitHub API. It derives the `runs` value from the e2e job names, `e2e (1/bbolt)` to `e2e (20/redis)`, because the workflow-run API does not return the dispatch inputs. A successful rerun attempt counts on its own: when attempt 1 failed on an infrastructure glitch and attempt 2 passed, the commit is gated.

A tag push on a commit without the gate fails in the `gate` job. Nothing is built, signed, tagged, or published.

The decision is `hack/release-gate`, with the logic in `internal/releasegate`. It reads the `gh api` payloads and makes no network call. Unit tests and `FuzzDecide` cover a missing run, a failed run, a run with fewer than 20 e2e jobs, a failed `ci`, and the rerun case. Run it locally against saved `gh api` payloads with `go run ./hack/release-gate -sha <sha> -runs runs.json -jobs jobs.json`.

Test the gate against a commit without publishing anything:

```bash
gh workflow run release.yml -f dry_run=true -f sha=<sha>
run_id="$(gh run list --workflow=release.yml --limit=1 --json databaseId --jq '.[0].databaseId')"
gh run watch "$run_id"
```

A dry run runs only the `gate` job against the given 40-character commit SHA. It creates no release, image tag, or chart.

## Supply chain

CI runs `govulncheck ./...` in the `vulncheck` job. The job fails when the module graph reaches a known vulnerability in our code paths. Fix it by bumping the dependency in `go.mod`. The scan runs with the latest vulnerability database, so a new advisory can fail a pull request that changes nothing.

The release workflow builds each image, scans it, signs it, and tags it in that order:

1. Docker Buildx builds the multi-arch image and pushes the manifest and layers by digest, without a tag.
2. `trivy image` scans that digest for `linux/amd64` and `linux/arm64` with `--severity HIGH,CRITICAL --ignore-unfixed --exit-code 1`. A HIGH or CRITICAL vulnerability that has a fixed version fails the job. A finding without a fix is reported and does not block.
3. `cosign sign` signs the digest with the workflow's GitHub OIDC identity. No key and no stored secret is involved.
4. Only a signed digest gets the image tags with `docker buildx imagetools create`: the version tag, and for a final release `major.minor` and `latest`.
5. The chart job waits for both images, then pushes the chart to GHCR, uploads it to the draft release, and signs it with the same identity.

A failed scan fails the job before any tag exists. A failed signature fails the job before the tag step runs. Either failure leaves no tag for the digest, so no tag names an unsigned image. The untagged manifest is not a release artifact, and the registry garbage collector removes it. A tag that does not verify is not a release artifact.

GoReleaser signs the CLI release too. It builds the archives and `checksums.txt`, runs `cosign sign-blob --bundle checksums.txt.sigstore.json checksums.txt` with the same keyless identity, and attaches the bundle to the release. The goreleaser job has `id-token: write` for that token.

The GitHub release is a draft from creation until the `publish` job, which needs `goreleaser`, both `images`, and `chart`. A failed image scan, signature, or chart push therefore leaves only a draft release. `publish` runs `gh release edit <tag> --draft=false` after every job succeeded.

### Verify a published release

The workflow signs the digest of each image and the chart. A tag can move; a digest cannot. Resolve the tag first with `crane` (from go-containerregistry), then verify the digest:

```bash
owner=hayfever
tag=vX.Y.Z
version="${tag#v}"
identity="https://github.com/${owner}/mayfly/.github/workflows/release.yml@refs/tags/${tag}"

digest="$(crane digest ghcr.io/${owner}/mayflyd:${version})"
cosign verify \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "ghcr.io/${owner}/mayflyd@${digest}"

digest="$(crane digest ghcr.io/${owner}/mayfly-agent:${version})"
cosign verify \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "ghcr.io/${owner}/mayfly-agent@${digest}"

digest="$(crane digest ghcr.io/${owner}/charts/mayfly:${version})"
cosign verify \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "ghcr.io/${owner}/charts/mayfly@${digest}"

# Download checksums.txt and checksums.txt.sigstore.json from the release page.
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

sha256sum -c checksums.txt
```

Each `cosign` command must print the workflow identity and the tag. The OIDC issuer is the only trust anchor; there is no public key to fetch. `sha256sum -c` reads the archive names in `checksums.txt` from the current directory, so download the archives there too. Both images also carry BuildKit SBOM and provenance attestations. Verify them with the same identity flags and `cosign verify-attestation --type slsaprovenance`. The chart has no attestation; its signature covers the chart digest.

## Pre-release checklist

1. Update `CHANGELOG.md` and the chart `version` and `appVersion` if they differ from the tag. Replace `Unreleased` in the release heading with the tag date, for example `## [1.0.0-rc.1] - 2026-11-20`, in the commit you tag. Do this for an RC too.
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
5. Run the e2e stability gate on the release commit, before you tag an RC or a final release. The gate needs 20 of 20 e2e runs green and the unit stress green:

```bash
gh workflow run e2e-stability.yml -f runs=20 -f ref=<sha>
```

The gate and its failure modes are in [testing.md](testing.md#stability-gate). A green run on another commit does not cover this one. The release workflow checks the same gate for the tagged commit and fails the tag without it. Test the gate without publishing with `gh workflow run release.yml -f dry_run=true -f sha=<sha>`.
6. Review the security notes and known limitations for accuracy.

## Tag

```bash
git tag -a vX.Y.Z -m "Mayfly vX.Y.Z"
git push origin main --tags
```

The release workflow runs on `v*` tags in this order:

1. `gate` reads the tagged commit's workflow history. It fails the workflow unless `ci` succeeded and a successful `e2e-stability` attempt with 20 runs exists for that commit.
2. `goreleaser` builds the CLI archives and `checksums.txt`, signs `checksums.txt` with keyless cosign, and creates the GitHub release as a draft. The job fails when its Go toolchain is older than 1.27.4 (GO-2026-6859).
3. `images` builds each control-plane and agent image with SBOM and provenance, pushes it by digest without a tag, scans both architectures, signs the digest, and only then creates the version tags.
4. `chart` pushes and signs the chart, and uploads it to the draft release.
5. `publish` turns the draft into a public release. It runs only when `goreleaser`, `images`, and `chart` all succeeded.

A failed job leaves the GitHub release a draft, and later jobs do not run. Rerun the failed job; `publish` makes the release public when the rerun succeeds.

## Release candidates

Tag a candidate to exercise the full release path before the final tag:

```bash
git tag -a vX.Y.Z-rc.1 -m "Mayfly vX.Y.Z-rc.1"
git push origin vX.Y.Z-rc.1
```

The same gated workflow runs. GoReleaser marks the GitHub release as a pre-release (`prerelease: auto`) and `release.make_latest: auto` keeps the `latest` marker on the newest final release, so an RC never becomes latest. The release stays a draft until `publish` runs. The image tags come from the semver patterns in `release.yml`: an RC tag like `v1.0.0-rc.1` publishes only `1.0.0-rc.1`. `docker/metadata-action` skips the `{{major}}.{{minor}}` pattern for a pre-release, and `latest` is enabled only for a tag with no `-`. The chart is pushed as `1.0.0-rc.1`. Install it with `--version 1.0.0-rc.1`. When the candidate passes, tag `v1.0.0` on the same commit; that tag publishes `1.0.0`, `1.0`, and `latest`.

## Post-release verification

Run the commands in [Verify a published release](#verify-a-published-release) first. Every `cosign` command must print the workflow identity and the tag. Check that `publish` made the release public:

```bash
gh release view vX.Y.Z --json isDraft --jq '.isDraft'   # false
```

```bash
docker pull ghcr.io/<owner>/mayflyd:<version>
docker pull ghcr.io/<owner>/mayfly-agent:<version>
helm show chart oci://ghcr.io/<owner>/charts/mayfly --version <version>
```

Install from the published chart in a clean namespace, create a session, attach, close it, download the artifact, and verify the session pod is gone.

## Rollback

Images and charts are immutable by version. If a release is broken, publish a new patch version and update the chart. Do not retag an existing release.
