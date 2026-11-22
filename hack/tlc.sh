#!/usr/bin/env bash
# Run TLC on the session lifecycle and hub retention models.
#
#   hack/tlc.sh                 every *.cfg directly under spec/tla (the PR set)
#   hack/tlc.sh --deep          every *.cfg under spec/tla/deep (nightly bounds)
#   hack/tlc.sh CFG...          the named cfgs: a path, or a file name under
#                               spec/tla or spec/tla/deep
#
# The module for a cfg is its basename up to the first underscore, so
# SessionResume_buggy.cfg runs SessionResume.tla.
#
# The jar is pinned by version and SHA-256. It is cached outside the repo; the
# TLC state directory goes under a temporary directory, also outside the repo.
# Every *.cfg under spec/tla runs. A positive cfg must pass. A cfg whose name
# ends in _buggy.cfg must fail with the invariant named by its
#
#     \* expect-violation: <InvariantName>
#
# line (default NonIdempotentAtMostOnce). TLC exits non-zero for a violation,
# but so does a parse error, so the invariant name is required in the output.
set -euo pipefail

TLA_VERSION="v1.7.4"
TLA_SHA256="936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88"
TLA_URL="https://github.com/tlaplus/tlaplus/releases/download/${TLA_VERSION}/tla2tools.jar"

CACHE_DIR="${TLA_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/mayfly/tla}"
JAR="${CACHE_DIR}/tla2tools.jar"
SPEC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../spec/tla" && pwd)"
WORK_ROOT="${TMPDIR:-$HOME/tmp}"

usage() {
	cat <<EOF
usage: hack/tlc.sh [--deep | CFG...]

With no argument, runs every *.cfg directly under spec/tla. --deep runs the
larger-bound cfgs under spec/tla/deep. Named cfgs may be paths or file names
under spec/tla or spec/tla/deep.

Environment:
  JAVA           java binary to use (default: java on PATH, then \$JAVA_HOME/bin/java)
  TLA_CACHE_DIR  cache for tla2tools.jar (default: \${XDG_CACHE_HOME:-\$HOME/.cache}/mayfly/tla)
  TLC_JAVA_OPTS  extra JVM options, e.g. -Xmx8g for the deep cfgs
EOF
}

deep=0
requested=()
for arg in "$@"; do
	case "$arg" in
	-h | --help)
		usage
		exit 0
		;;
	--deep)
		deep=1
		;;
	-*)
		echo "hack/tlc.sh: unknown option $arg" >&2
		usage >&2
		exit 2
		;;
	*)
		requested+=("$arg")
		;;
	esac
done
mode=default
if [ ${#requested[@]} -gt 0 ]; then
	mode=named
fi
if [ "$deep" -eq 1 ]; then
	if [ "$mode" = named ]; then
		echo "hack/tlc.sh: pass either --deep or cfg names, not both" >&2
		exit 2
	fi
	mode=deep
fi

find_java() {
	if [ -n "${JAVA:-}" ]; then
		printf '%s\n' "$JAVA"
		return 0
	fi
	if command -v java >/dev/null 2>&1; then
		command -v java
		return 0
	fi
	if [ -n "${JAVA_HOME:-}" ] && [ -x "${JAVA_HOME}/bin/java" ]; then
		printf '%s\n' "${JAVA_HOME}/bin/java"
		return 0
	fi
	return 1
}

sha256_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

if ! JAVA_BIN="$(find_java)"; then
	echo "hack/tlc.sh: java not found; set JAVA or JAVA_HOME, or install a JDK 17 or newer" >&2
	exit 1
fi

mkdir -p "$CACHE_DIR"
if [ -f "$JAR" ] && [ "$(sha256_file "$JAR")" = "$TLA_SHA256" ]; then
	echo "tla2tools ${TLA_VERSION}: cached at $JAR"
else
	echo "tla2tools ${TLA_VERSION}: downloading from $TLA_URL"
	tmp_jar="$(mktemp "${CACHE_DIR}/tla2tools.XXXXXX")"
	if ! curl -fsSL --retry 3 --retry-delay 2 -o "$tmp_jar" "$TLA_URL"; then
		rm -f "$tmp_jar"
		echo "hack/tlc.sh: download failed" >&2
		exit 1
	fi
	got="$(sha256_file "$tmp_jar")"
	if [ "$got" != "$TLA_SHA256" ]; then
		rm -f "$tmp_jar"
		echo "hack/tlc.sh: sha256 mismatch for $TLA_URL" >&2
		echo "  want $TLA_SHA256" >&2
		echo "  got  $got" >&2
		exit 1
	fi
	mv "$tmp_jar" "$JAR"
	echo "tla2tools ${TLA_VERSION}: verified $TLA_SHA256"
fi

mkdir -p "$WORK_ROOT"
WORK="$(mktemp -d "${WORK_ROOT}/mayfly-tlc.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

cfgs=()
case "$mode" in
default)
	shopt -s nullglob
	cfgs=("$SPEC_DIR"/*.cfg)
	shopt -u nullglob
	;;
deep)
	shopt -s nullglob
	cfgs=("$SPEC_DIR"/deep/*.cfg)
	shopt -u nullglob
	;;
named)
	for want in "${requested[@]}"; do
		if [ -f "$want" ]; then
			cfgs+=("$(cd "$(dirname "$want")" && pwd)/$(basename "$want")")
		elif [ -f "$SPEC_DIR/$want" ]; then
			cfgs+=("$SPEC_DIR/$want")
		elif [ -f "$SPEC_DIR/deep/$want" ]; then
			cfgs+=("$SPEC_DIR/deep/$want")
		else
			echo "hack/tlc.sh: cfg not found: $want" >&2
			exit 1
		fi
	done
	;;
esac
if [ ${#cfgs[@]} -eq 0 ]; then
	echo "hack/tlc.sh: no .cfg files selected ($mode) under $SPEC_DIR" >&2
	exit 1
fi

failed=0
summary=()

for cfg in "${cfgs[@]}"; do
	name="$(basename "$cfg")"
	# The module is the cfg basename up to the first underscore, so
	# SessionResume_buggy.cfg runs SessionResume.tla and
	# HubRetention_evict_inbox_buggy.cfg runs HubRetention.tla.
	base="${name%.cfg}"
	module="${base%%_*}"
	expect=""
	if [[ "$name" == *_buggy.cfg ]]; then
		expect="$(sed -n 's/^\\\* expect-violation:[[:space:]]*\([A-Za-z0-9_]*\).*/\1/p' "$cfg" | head -n 1)"
		expect="${expect:-NonIdempotentAtMostOnce}"
	fi

	log="${WORK}/${name}.log"
	echo
	echo "=== ${name}"
	start="$(date +%s)"
	status=0
	(
		cd "$SPEC_DIR"
		# shellcheck disable=SC2086 # TLC_JAVA_OPTS is a list of options.
		"$JAVA_BIN" -XX:+UseParallelGC ${TLC_JAVA_OPTS:-} -cp "$JAR" tlc2.TLC \
			-workers auto -config "$cfg" -metadir "$WORK/meta-${name}" \
			"${module}.tla"
	) >"$log" 2>&1 || status=$?
	elapsed=$(($(date +%s) - start))

	result=ok
	if [ -n "$expect" ]; then
		if [ "$status" -eq 0 ]; then
			echo "FAIL ${name}: TLC passed, but this cfg must violate ${expect}"
			tail -n 5 "$log"
			result=FAIL
		elif ! grep -q "Invariant ${expect} is violated" "$log"; then
			echo "FAIL ${name}: expected an ${expect} violation, got another outcome"
			grep -E 'Error|violated|Parse|Semantic' "$log" | head -n 5 || true
			result=FAIL
		else
			echo "PASS ${name}: ${expect} violated as expected (${elapsed}s)"
		fi
	else
		if [ "$status" -ne 0 ] || ! grep -q 'No error has been found' "$log"; then
			echo "FAIL ${name}: TLC did not pass (${elapsed}s)"
			grep -E 'Error|violated|Parse|Semantic|Finished in' "$log" | head -n 10 || true
			result=FAIL
		else
			echo "PASS ${name}: no violation (${elapsed}s)"
		fi
	fi
	if [ "$result" = FAIL ]; then failed=1; fi
	summary+=("$(printf '%-32s %s %ss' "$name" "$result" "$elapsed")")
done

echo
echo "=== summary"
printf '%s\n' "${summary[@]}"
exit "$failed"
