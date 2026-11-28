#!/usr/bin/env bash
# Test hack/image-scan.sh without a network, a registry, or a real trivy.
#
# A fake trivy on PATH records its arguments and fails on request, so the
# checks prove: both platforms always run with the policy flags; one failing
# platform fails the whole scan and does not stop the other platform; a trivy
# error fails the scan; a clean scan passes; --dir mode scans both archives and
# fails when one is missing; and the step summary lists both platforms.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scan="$repo_root/hack/image-scan.sh"

work="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/image-scan-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT

fake="$work/trivy"
cat >"$fake" <<'EOF'
#!/usr/bin/env bash
# Records every call and fails on request. Never contacts a registry.
set -euo pipefail
printf '%s\n' "$*" >>"${FAKE_TRIVY_LOG:?FAKE_TRIVY_LOG is required}"
platform=""
input=""
while [ "$#" -gt 0 ]; do
	case "$1" in
	--platform)
		platform="$2"
		shift 2
		;;
	--input)
		input="$2"
		shift 2
		;;
	*) shift ;;
	esac
done
if [ -n "${FAKE_TRIVY_FINDING_PLATFORM:-}" ] && [ "$platform" = "$FAKE_TRIVY_FINDING_PLATFORM" ]; then
	echo "libpam0g CVE-2025-6020 HIGH fixed 1.5.2-6+deb12u3"
	exit 1
fi
if [ -n "${FAKE_TRIVY_ERROR_PLATFORM:-}" ] && [ "$platform" = "$FAKE_TRIVY_ERROR_PLATFORM" ]; then
	echo "FATAL failed to download the vulnerability database"
	exit 2
fi
echo "no vulnerabilities found"
EOF
chmod +x "$fake"

failed=0
pass() { echo "PASS $1"; }
fail() {
	echo "FAIL $1" >&2
	failed=1
}

# run_scan sets status and output for one invocation of the script.
run_scan() {
	status=0
	output="$(env "$@" "$scan" "${ARGS[@]}" 2>&1)" || status=$?
}

# --- a clean scan passes, and both platforms carry the policy flags ---------

log="$work/clean.log"
: >"$log"
ARGS=(example.invalid/mayflyd:test)
run_scan "TRIVY=$fake" "FAKE_TRIVY_LOG=$log"
if [ "$status" -ne 0 ]; then
	fail "clean image scan exits $status, want 0: $output"
elif [ "$(wc -l <"$log")" -ne 2 ]; then
	fail "clean image scan ran $(wc -l <"$log") trivy calls, want 2"
else
	policy="image --quiet --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1"
	if ! grep -qx -- "$policy --platform linux/amd64 example.invalid/mayflyd:test" "$log"; then
		fail "linux/amd64 call lacks the policy flags: $(cat "$log")"
	elif ! grep -qx -- "$policy --platform linux/arm64 example.invalid/mayflyd:test" "$log"; then
		fail "linux/arm64 call lacks the policy flags: $(cat "$log")"
	else
		pass "clean scan passes and both platforms use the policy flags"
	fi
fi

# --- a finding on arm64 fails the scan, and amd64 still runs ----------------

log="$work/arm64.log"
: >"$log"
run_scan "TRIVY=$fake" "FAKE_TRIVY_LOG=$log" "FAKE_TRIVY_FINDING_PLATFORM=linux/arm64"
if [ "$status" -eq 0 ]; then
	fail "a finding on linux/arm64 exited 0"
elif [ "$(wc -l <"$log")" -ne 2 ]; then
	fail "a finding on linux/arm64 stopped the scan after $(wc -l <"$log") call(s)"
elif ! grep -q "::error::image scan failed for example.invalid/mayflyd:test on linux/arm64" <<<"$output"; then
	fail "the failure does not name the image and platform: $output"
else
	pass "a finding on linux/arm64 fails the scan after both platforms run"
fi

# --- a trivy error fails the scan -------------------------------------------

log="$work/error.log"
: >"$log"
run_scan "TRIVY=$fake" "FAKE_TRIVY_LOG=$log" "FAKE_TRIVY_ERROR_PLATFORM=linux/amd64"
if [ "$status" -eq 0 ]; then
	fail "a trivy error exited 0"
elif ! grep -q "exit status 2" <<<"$output"; then
	fail "the trivy error is not reported: $output"
else
	pass "a trivy error fails the scan"
fi

# --- --dir mode scans both archives ----------------------------------------

mkdir -p "$work/archives"
: >"$work/archives/linux-amd64.tar"
: >"$work/archives/linux-arm64.tar"
log="$work/dir.log"
: >"$log"
ARGS=(--dir "$work/archives")
run_scan "TRIVY=$fake" "FAKE_TRIVY_LOG=$log"
if [ "$status" -ne 0 ]; then
	fail "--dir scan exits $status, want 0: $output"
elif [ "$(wc -l <"$log")" -ne 2 ]; then
	fail "--dir scan ran $(wc -l <"$log") trivy calls, want 2"
elif ! grep -q -- "--input $work/archives/linux-amd64.tar" "$log" ||
	! grep -q -- "--input $work/archives/linux-arm64.tar" "$log"; then
	fail "--dir scan did not pass both archives: $(cat "$log")"
else
	pass "--dir scans both archives"
fi

# --- --dir mode fails when an archive is missing ---------------------------

mkdir -p "$work/incomplete"
: >"$work/incomplete/linux-amd64.tar"
log="$work/incomplete.log"
: >"$log"
ARGS=(--dir "$work/incomplete")
run_scan "TRIVY=$fake" "FAKE_TRIVY_LOG=$log"
if [ "$status" -eq 0 ]; then
	fail "--dir with a missing arm64 archive exited 0"
elif ! grep -q "no archive for linux/arm64" <<<"$output"; then
	fail "the missing archive is not reported: $output"
elif [ "$(wc -l <"$log")" -ne 1 ]; then
	fail "the present archive was not scanned: $(cat "$log")"
else
	pass "--dir fails when an archive is missing"
fi

# --- the step summary lists both platforms ---------------------------------

summary="$work/summary.md"
log="$work/pass.log"
: >"$log"
ARGS=(example.invalid/mayflyd:test)
run_scan "TRIVY=$fake" "FAKE_TRIVY_LOG=$log" "GITHUB_STEP_SUMMARY=$summary"
if [ "$status" -ne 0 ]; then
	fail "the summary scan exits $status: $output"
elif ! grep -q "| linux/amd64 | pass |" "$summary" ||
	! grep -q "| linux/arm64 | pass |" "$summary"; then
	fail "the step summary lacks a platform row: $(cat "$summary" 2>/dev/null)"
else
	pass "the step summary lists both platforms"
fi

# --- a missing trivy fails the scan ----------------------------------------

ARGS=(example.invalid/mayflyd:test)
run_scan "TRIVY=$work/no-such-trivy"
if [ "$status" -eq 0 ]; then
	fail "a missing trivy exited 0"
elif ! grep -q "trivy not found" <<<"$output"; then
	fail "the missing trivy is not reported: $output"
else
	pass "a missing trivy fails the scan"
fi

if [ "$failed" -ne 0 ]; then
	echo "hack/test-image-scan.sh: at least one check failed" >&2
	exit 1
fi
echo "hack/test-image-scan.sh: all checks passed"
