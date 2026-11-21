#!/usr/bin/env bash
# Run every native Go fuzz target once, for a fixed time each.
#
# Targets are discovered with `go test -list '^Fuzz' ./...`. Each target runs
# as
#
#     go test -run '^$' -fuzz '^<name>$' -fuzztime <duration> <package>
#
# with go test's default GOCACHE, so a CI job can cache the corpus at
# $(go env GOCACHE)/fuzz. Every target runs even after one fails. The script
# exits non-zero when any target failed. With no targets it exits 0: a branch
# that has not added a fuzz package yet is not an error.
set -euo pipefail

FUZZTIME="${FUZZTIME:-60s}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if ! command -v go >/dev/null 2>&1; then
	echo "hack/fuzz.sh: go not found on PATH" >&2
	exit 1
fi

# Discovery. `go test -list` prints one name per line and then a package
# summary line: "ok <pkg> <time>" or "? <pkg> [no test files]". Names belong
# to the summary line that follows them, so buffer names until it arrives.
list_output="$(go test -list '^Fuzz' ./... 2>&1)" || {
	echo "$list_output" >&2
	echo "hack/fuzz.sh: discovery failed" >&2
	exit 1
}

targets=()
pending=()
while IFS= read -r line; do
	case "$line" in
	Fuzz*)
		pending+=("$line")
		;;
	ok\ * | ok$'\t'*)
		pkg="$(printf '%s\n' "$line" | awk '{print $2}')"
		for name in "${pending[@]}"; do
			targets+=("${pkg} ${name}")
		done
		pending=()
		;;
	'?'* | '')
		pending=()
		;;
	FAIL*)
		echo "hack/fuzz.sh: package failed to list fuzz targets:" >&2
		echo "$line" >&2
		exit 1
		;;
	esac
done <<<"$list_output"

if [ ${#targets[@]} -eq 0 ]; then
	echo "hack/fuzz.sh: no fuzz targets found; nothing to run"
	exit 0
fi

echo "hack/fuzz.sh: ${#targets[@]} target(s), ${FUZZTIME} each"

failed=0
for target in "${targets[@]}"; do
	pkg="${target%% *}"
	name="${target##* }"
	echo
	echo "=== ${name} (${pkg})"
	if go test -run '^$' -fuzz "^${name}\$" -fuzztime "${FUZZTIME}" "${pkg}"; then
		echo "PASS ${name}"
	else
		echo "FAIL ${name}"
		# Go writes a failing input to testdata/fuzz/<Name>/ in the package.
		if [ -d "${pkg}/testdata/fuzz/${name}" ]; then
			echo "crashing input(s) under ${pkg}/testdata/fuzz/${name}:"
			ls -1 "${pkg}/testdata/fuzz/${name}"
		fi
		failed=1
	fi
done

echo
if [ "$failed" -ne 0 ]; then
	echo "hack/fuzz.sh: at least one fuzz target failed"
	exit 1
fi
echo "hack/fuzz.sh: all fuzz targets passed"
