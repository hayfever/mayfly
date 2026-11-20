#!/usr/bin/env bash
# Fail when a change to the mayfly CLI surface breaks an existing caller.
#
# Usage: hack/surface-diff.sh [base-ref]
#
# The gate has two parts. The golden test proves that
# cmd/mayfly/testdata/cli-surface.txt matches the flag sets the commands parse.
# The comparison proves that the golden file against the base ref only gains
# lines. A removed or changed line is a rename or a removal, which needs a
# "Breaking changes" entry in CHANGELOG.md that names the CLI.
#
# The base ref defaults to $SURFACE_DIFF_BASE, then origin/main, then the
# newest v* tag. When the base has no golden file, the check prints a notice
# and passes.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

golden="cmd/mayfly/testdata/cli-surface.txt"
regenerate="go test ./cmd/mayfly -run TestCLISurface -update"

base="${1:-${SURFACE_DIFF_BASE:-}}"
if [ -z "$base" ]; then
  if git rev-parse --verify --quiet "origin/main^{commit}" >/dev/null; then
    base="origin/main"
  else
    # Version sort ranks v0.2.0 above v0.2.0-rc.1; git describe would pick by
    # commit ancestry instead.
    base="$(git -c versionsort.suffix=-rc tag --list 'v*' --sort=-version:refname | head -n 1)"
  fi
fi
if [ -z "$base" ]; then
  echo "surface-diff: no base ref found; skipping the surface comparison" >&2
  exit 0
fi
if ! git rev-parse --verify --quiet "$base^{commit}" >/dev/null; then
  echo "surface-diff: base ref $base does not exist" >&2
  exit 2
fi

# A change to a flag or a command changes the surface, and the golden test
# turns that into a failure until the golden file records it.
echo "surface-diff: checking $golden against the code"
go test ./cmd/mayfly -run TestCLISurface -count=1

if ! git cat-file -e "$base:$golden" 2>/dev/null; then
  echo "surface-diff: $base has no $golden; skipping the surface comparison" >&2
  exit 0
fi

old="$(mktemp)"
trap 'rm -f "$old"' EXIT
git show "$base:$golden" > "$old"

if cmp -s "$old" "$golden"; then
  echo "surface-diff: the CLI surface is unchanged"
  exit 0
fi

removed="$(LC_ALL=C comm -23 <(LC_ALL=C sort "$old") <(LC_ALL=C sort "$golden") || true)"
added="$(LC_ALL=C comm -13 <(LC_ALL=C sort "$old") <(LC_ALL=C sort "$golden") || true)"

if [ -n "$added" ]; then
  echo "surface-diff: added lines:"
  printf '%s\n' "$added" | sed 's/^/  + /'
fi

if [ -z "$removed" ]; then
  echo "surface-diff: the change only adds to the CLI surface" >&2
  exit 0
fi

echo "surface-diff: the CLI surface lost or changed these lines:" >&2
printf '%s\n' "$removed" | sed 's/^/  - /' >&2

# changelog_records_cli_break reports whether the change against the base adds
# a bullet under a "Breaking changes" heading that names the CLI: the word
# "CLI" or a `mayfly ...` command. The diff carries the whole file as context
# so a bullet far below its heading still sees the heading.
changelog_records_cli_break() {
  git diff --no-color --unified=100000 "$base" -- CHANGELOG.md | awk '
    /^[+ ]##/ { heading = ($0 ~ /Breaking changes/); next }
    heading && /^\+[-*] / {
      line = tolower($0)
      if (line ~ /(^|[^a-z])cli([^a-z]|$)/ || line ~ /`mayfly[ `]/) { found = 1 }
    }
    END { exit(found ? 0 : 1) }
  '
}

if changelog_records_cli_break; then
  echo "surface-diff: CHANGELOG.md records a CLI break under \"Breaking changes\"; allowing the change" >&2
  exit 0
fi

echo "surface-diff: record the new surface with \`$regenerate\`, then add a bullet" >&2
echo "surface-diff: under \"Breaking changes\" in CHANGELOG.md that names the CLI change" >&2
exit 1
