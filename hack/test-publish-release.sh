#!/usr/bin/env bash
# Test hack/publish-release.sh without a network, a GitHub release, or a real
# gh.
#
# A fake gh on PATH records its arguments and answers from FAKE_GH_* variables,
# so the checks prove: an RC is published with --latest=false and fails when it
# is the repository's latest release; a final tag is published with
# --latest=true and fails when the latest release is another tag; a failed
# view, edit, API read, or a release that stays a draft fails; and a missing
# GH_REPO or GH_TOKEN fails before any call.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
publish="$repo_root/hack/publish-release.sh"

work="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/publish-release-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/bin"
cat >"$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
# Records every call and answers from FAKE_GH_* variables. Never contacts
# GitHub.
set -euo pipefail
printf '%s\n' "$*" >>"${FAKE_GH_LOG:?FAKE_GH_LOG is required}"

case "${1:-}" in
release)
	case "${2:-}" in
	view)
		if [[ -n "${FAKE_GH_VIEW_FAIL:-}" ]]; then
			echo "HTTP 404: Not Found" >&2
			exit 1
		fi
		if grep -q '^release edit ' "$FAKE_GH_LOG" && [[ -n "${FAKE_GH_DRAFT_AFTER:-}" ]]; then
			printf '%s\n' "$FAKE_GH_DRAFT_AFTER"
		else
			printf '%s\n' "${FAKE_GH_DRAFT:-true}"
		fi
		;;
	edit)
		if [[ -n "${FAKE_GH_EDIT_FAIL:-}" ]]; then
			echo "HTTP 500: Internal Server Error" >&2
			exit 1
		fi
		;;
	*)
		echo "fake gh: unknown release command ${2:-}" >&2
		exit 2
		;;
	esac
	;;
api)
	if [[ "${2:-}" != "repos/${GH_REPO:?GH_REPO is required}/releases/latest" ]]; then
		echo "fake gh: unexpected api path ${2:-}" >&2
		exit 2
	fi
	if [[ -n "${FAKE_GH_API_FAIL:-}" ]]; then
		echo "HTTP 404: Not Found" >&2
		exit 1
	fi
	printf '%s\n' "${FAKE_GH_LATEST_TAG:-}"
	;;
*)
	echo "fake gh: unknown command ${1:-}" >&2
	exit 2
	;;
esac
EOF
chmod +x "$work/bin/gh"
export PATH="$work/bin:$PATH"

failed=0
pass() { echo "PASS $1"; }
fail() {
	echo "FAIL $1" >&2
	failed=1
}

# run_publish runs the script with a fresh fake-gh log. The env assignments
# arrive as VAR=value arguments after the first one, and ARGS holds the script
# arguments. status and output describe the run.
run_publish() {
	log="$work/gh.log"
	: >"$log"
	status=0
	output="$(env FAKE_GH_LOG="$log" "$@" "$publish" "${ARGS[@]}" 2>&1)" || status=$?
}

ARGS=()

# --- an RC is published without the latest marker --------------------------

run_publish GH_TOKEN=test GH_REPO=hayfever/mayfly GITHUB_REF_NAME=v1.0.0-rc.4 \
	FAKE_GH_DRAFT=true FAKE_GH_DRAFT_AFTER=false FAKE_GH_LATEST_TAG=v0.3.0
if [ "$status" -ne 0 ]; then
	fail "an RC release exits $status, want 0: $output"
elif ! grep -q "published v1.0.0-rc.4 (latest=false)" <<<"$output"; then
	fail "the RC success line is missing: $output"
elif ! grep -qx -- "release edit v1.0.0-rc.4 --draft=false --latest=false" "$log"; then
	fail "the RC edit does not pass --latest=false --draft=false: $(cat "$log")"
elif ! grep -qx -- "api repos/hayfever/mayfly/releases/latest --jq .tag_name" "$log"; then
	fail "the latest read is not the repo-qualified releases/latest API: $(cat "$log")"
else
	pass "an RC is published with --latest=false and a repo-qualified latest read"
fi

# --- an RC that is its own repository's latest fails -----------------------

run_publish GH_TOKEN=test GH_REPO=hayfever/mayfly GITHUB_REF_NAME=v1.0.0-rc.4 \
	FAKE_GH_DRAFT=true FAKE_GH_DRAFT_AFTER=false FAKE_GH_LATEST_TAG=v1.0.0-rc.4
if [ "$status" -eq 0 ]; then
	fail "an RC reported as the repository's latest release exited 0"
elif ! grep -q "is a release candidate but the repository's latest release is v1.0.0-rc.4" <<<"$output"; then
	fail "the RC-as-latest failure is not reported: $output"
else
	pass "an RC that is the repository's latest release fails"
fi

# --- a final tag is published with the latest marker -----------------------

ARGS=(v1.0.0)
run_publish GH_TOKEN=test GH_REPO=hayfever/mayfly \
	FAKE_GH_DRAFT=true FAKE_GH_DRAFT_AFTER=false FAKE_GH_LATEST_TAG=v1.0.0
if [ "$status" -ne 0 ]; then
	fail "a final release exits $status, want 0: $output"
elif ! grep -q "published v1.0.0 (latest=true)" <<<"$output"; then
	fail "the final success line is missing: $output"
elif ! grep -qx -- "release edit v1.0.0 --draft=false --latest=true" "$log"; then
	fail "the final edit does not pass --latest=true: $(cat "$log")"
else
	pass "a final tag is published with --latest=true"
fi
ARGS=()

# --- a final tag that is not the repository's latest fails -----------------

ARGS=(v1.0.0)
run_publish GH_TOKEN=test GH_REPO=hayfever/mayfly \
	FAKE_GH_DRAFT=true FAKE_GH_DRAFT_AFTER=false FAKE_GH_LATEST_TAG=v0.9.0
if [ "$status" -eq 0 ]; then
	fail "a final release that is not the repository's latest exited 0"
elif ! grep -q "is a final release but the repository's latest release is v0.9.0" <<<"$output"; then
	fail "the final-not-latest failure is not reported: $output"
else
	pass "a final tag that is not the repository's latest release fails"
fi
ARGS=()

# --- a failed draft-state read stops before the edit -----------------------

run_publish GH_TOKEN=test GH_REPO=hayfever/mayfly GITHUB_REF_NAME=v1.0.0 \
	FAKE_GH_VIEW_FAIL=1
if [ "$status" -eq 0 ]; then
	fail "a failed draft-state read exited 0"
elif [ "$(wc -l <"$log")" -ne 1 ]; then
	fail "a failed draft-state read still ran $(wc -l <"$log") gh calls"
else
	pass "a failed draft-state read fails before the edit"
fi

# --- an unreadable draft state fails ---------------------------------------

run_publish GH_TOKEN=test GH_REPO=hayfever/mayfly GITHUB_REF_NAME=v1.0.0 \
	FAKE_GH_DRAFT=maybe
if [ "$status" -eq 0 ]; then
	fail "an unreadable draft state exited 0"
elif ! grep -q "could not read the draft state of v1.0.0 (got 'maybe')" <<<"$output"; then
	fail "the unreadable draft state is not reported: $output"
else
	pass "an unreadable draft state fails"
fi

# --- a failed edit fails ---------------------------------------------------

run_publish GH_TOKEN=test GH_REPO=hayfever/mayfly GITHUB_REF_NAME=v1.0.0 \
	FAKE_GH_EDIT_FAIL=1
if [ "$status" -eq 0 ]; then
	fail "a failed release edit exited 0"
elif ! grep -q "HTTP 500" <<<"$output"; then
	fail "the failed edit is not reported: $output"
else
	pass "a failed release edit fails"
fi

# --- a release that stays a draft fails ------------------------------------

run_publish GH_TOKEN=test GH_REPO=hayfever/mayfly GITHUB_REF_NAME=v1.0.0 \
	FAKE_GH_DRAFT=true FAKE_GH_DRAFT_AFTER=true FAKE_GH_LATEST_TAG=v1.0.0
if [ "$status" -eq 0 ]; then
	fail "a release that is still a draft exited 0"
elif ! grep -q "v1.0.0 is still a draft" <<<"$output"; then
	fail "the still-a-draft failure is not reported: $output"
else
	pass "a release that is still a draft fails"
fi

# --- a failed latest-release read fails ------------------------------------

run_publish GH_TOKEN=test GH_REPO=hayfever/mayfly GITHUB_REF_NAME=v1.0.0 \
	FAKE_GH_DRAFT=true FAKE_GH_DRAFT_AFTER=false FAKE_GH_API_FAIL=1
if [ "$status" -eq 0 ]; then
	fail "a failed latest-release read exited 0"
elif ! grep -q "HTTP 404" <<<"$output"; then
	fail "the failed latest-release read is not reported: $output"
else
	pass "a failed latest-release read fails"
fi

# --- missing GH_REPO or GH_TOKEN fails before any call ---------------------

run_publish GH_TOKEN=test GITHUB_REF_NAME=v1.0.0 FAKE_GH_LATEST_TAG=v1.0.0
if [ "$status" -eq 0 ]; then
	fail "a missing GH_REPO exited 0"
elif ! grep -q "GH_REPO is required" <<<"$output"; then
	fail "the missing GH_REPO is not reported: $output"
elif [ -s "$log" ]; then
	fail "a missing GH_REPO still ran gh: $(cat "$log")"
else
	pass "a missing GH_REPO fails before any gh call"
fi

run_publish GH_REPO=hayfever/mayfly GITHUB_REF_NAME=v1.0.0 FAKE_GH_LATEST_TAG=v1.0.0
if [ "$status" -eq 0 ]; then
	fail "a missing GH_TOKEN exited 0"
elif ! grep -q "GH_TOKEN is required" <<<"$output"; then
	fail "the missing GH_TOKEN is not reported: $output"
elif [ -s "$log" ]; then
	fail "a missing GH_TOKEN still ran gh: $(cat "$log")"
else
	pass "a missing GH_TOKEN fails before any gh call"
fi

if [ "$failed" -ne 0 ]; then
	echo "hack/test-publish-release.sh: at least one check failed" >&2
	exit 1
fi
echo "hack/test-publish-release.sh: all checks passed"
