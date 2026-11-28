#!/usr/bin/env bash
# Scan one Mayfly image for HIGH or CRITICAL vulnerabilities that have a fix.
#
#   hack/image-scan.sh <image-ref>
#   hack/image-scan.sh --dir <directory>
#
# The first form scans a registry reference for linux/amd64 and linux/arm64.
# The second scans <directory>/linux-amd64.tar and <directory>/linux-arm64.tar,
# the docker archives that `docker buildx build --output type=docker,dest=...`
# writes, so a workflow can scan an image it built without pushing it.
#
# This script is the one place the image scan policy lives. Every workflow
# calls it and no workflow runs trivy directly. A fixed HIGH or CRITICAL
# vulnerability fails the run, and so does a trivy error such as a failed
# database download. Both platforms always run; a failure on any platform
# fails the script. The release workflow scans, signs, and tags in that order.
#
# Trivy comes from PATH, or TRIVY names another binary. The version is pinned
# in .github/trivy-version; every workflow installs that version.
set -euo pipefail

# shellcheck disable=SC2054 # the comma is part of the flag value.
readonly TRIVY_ARGS=(--quiet --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1)
readonly PLATFORMS=(linux/amd64 linux/arm64)

usage() {
	cat <<'EOF'
usage: hack/image-scan.sh <image-ref>
       hack/image-scan.sh --dir <directory>

  <image-ref>   registry reference, for example ghcr.io/owner/mayflyd:1.0.0
  --dir DIR     directory with linux-amd64.tar and linux-arm64.tar

Environment:
  TRIVY   trivy binary to run (default: trivy on PATH)
EOF
}

usage_error() {
	echo "hack/image-scan.sh: $1" >&2
	usage >&2
	exit 2
}

case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
--dir)
	if [ "$#" -ne 2 ]; then
		usage_error "--dir takes exactly one directory"
	fi
	mode=dir
	dir="$2"
	target="$dir"
	;;
-*)
	usage_error "unknown option $1"
	;;
"")
	usage_error "an image reference or --dir is required"
	;;
*)
	if [ "$#" -ne 1 ]; then
		usage_error "exactly one image reference is required"
	fi
	mode=image
	image="$1"
	target="$image"
	;;
esac

trivy_bin="${TRIVY:-trivy}"
if ! command -v "$trivy_bin" >/dev/null 2>&1; then
	echo "::error::image scan: trivy not found (TRIVY=${trivy_bin}); install the version pinned in .github/trivy-version" >&2
	exit 1
fi

work="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/image-scan.XXXXXX")"
trap 'rm -rf "$work"' EXIT

failed=0
results=()
details=""

for platform in "${PLATFORMS[@]}"; do
	log="$work/${platform//\//-}.log"
	result="pass"
	scan=(image "${TRIVY_ARGS[@]}")
	if [ "$mode" = image ]; then
		scan+=(--platform "$platform" "$image")
	else
		archive="$dir/${platform//\//-}.tar"
		if [ ! -f "$archive" ]; then
			echo "::error::image scan: no archive for ${platform} at ${archive}" >&2
			results+=("missing archive")
			details+="no ${platform} archive at ${archive}"$'\n'
			failed=1
			continue
		fi
		scan+=(--input "$archive")
	fi

	if "$trivy_bin" "${scan[@]}" >"$log" 2>&1; then
		cat "$log"
	else
		status=$?
		cat "$log" >&2
		echo "::error::image scan failed for ${target} on ${platform}: trivy found a fixed HIGH or CRITICAL vulnerability, or trivy itself failed (exit status ${status})" >&2
		result="fail"
		details+="$(tail -n 200 "$log")"$'\n'
		failed=1
	fi
	results+=("$result")
done

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo "### image scan: ${target}"
		echo
		echo "| platform | result |"
		echo "| --- | --- |"
		for i in "${!PLATFORMS[@]}"; do
			echo "| ${PLATFORMS[$i]} | ${results[$i]} |"
		done
		if [ "$failed" -ne 0 ]; then
			echo
			echo "<details><summary>trivy output for the failed scan</summary>"
			echo
			echo '```'
			printf '%s' "$details"
			echo '```'
			echo "</details>"
		fi
	} >>"$GITHUB_STEP_SUMMARY"
fi

if [ "$failed" -ne 0 ]; then
	echo "::error::image scan failed: ${target}" >&2
	exit 1
fi
echo "image scan passed: ${target}"
