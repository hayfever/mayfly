#!/usr/bin/env bash
# Publish the draft GitHub release of one tag and prove its latest marker.
#
#   hack/publish-release.sh [tag]
#
# The tag is the first argument, or GITHUB_REF_NAME when the script gets no
# argument, which is what a tag push sets in Actions. GH_REPO must name
# owner/name and GH_TOKEN must allow contents: write. Every gh call names the
# repository through GH_REPO, so the job needs no checkout.
#
# A tag that contains `-` is a release candidate. The script publishes it with
# --latest=false and fails when the repository's latest release is that tag.
# Any other tag is a final release: --latest=true, and the repository's latest
# release must be that tag. The latest release is
# GET /repos/{owner}/{repo}/releases/latest, read through `gh api`. There is no
# `--json isLatest` field in `gh release view`; asking for it fails the run.
#
# The script fails when gh fails, when the draft state is unreadable, when the
# release is still a draft after the edit, or when the latest marker is wrong.
# It prints one line on success.
set -euo pipefail

usage() {
	cat <<'EOF'
usage: hack/publish-release.sh [tag]

Environment:
  GH_REPO           owner/name of the repository (required)
  GH_TOKEN          token that allows contents: write (required)
  GITHUB_REF_NAME   tag to publish when no argument is given
EOF
}

die() {
	echo "::error::publish release: $1" >&2
	exit 1
}

tag="${1:-${GITHUB_REF_NAME:-}}"
if [[ -z "${GH_REPO:-}" ]]; then
	echo "hack/publish-release.sh: GH_REPO is required" >&2
	usage >&2
	exit 2
fi
if [[ -z "${GH_TOKEN:-}" ]]; then
	echo "hack/publish-release.sh: GH_TOKEN is required" >&2
	usage >&2
	exit 2
fi
if [[ -z "$tag" ]]; then
	echo "hack/publish-release.sh: a tag argument or GITHUB_REF_NAME is required" >&2
	usage >&2
	exit 2
fi

state="$(gh release view "$tag" --json isDraft --jq .isDraft)"
case "$state" in
true | false) ;;
*) die "could not read the draft state of $tag (got '$state')" ;;
esac

# An RC tag contains `-` and must not take the latest marker; a final tag
# must. --latest=false is explicit so a rerun of publish cannot leave an RC
# marked latest.
latest=false
if [[ "$tag" != *-* ]]; then
	latest=true
fi

gh release edit "$tag" --draft=false --latest="$latest"

state="$(gh release view "$tag" --json isDraft --jq .isDraft)"
if [[ "$state" != "false" ]]; then
	die "$tag is still a draft"
fi

latest_tag="$(gh api "repos/${GH_REPO}/releases/latest" --jq .tag_name)"
if [[ "$latest" == "false" ]]; then
	if [[ "$latest_tag" == "$tag" ]]; then
		die "$tag is a release candidate but the repository's latest release is $latest_tag"
	fi
elif [[ "$latest_tag" != "$tag" ]]; then
	die "$tag is a final release but the repository's latest release is ${latest_tag:-<none>}"
fi

echo "published $tag (latest=$latest)"
