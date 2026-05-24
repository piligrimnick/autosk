#!/usr/bin/env bash
# Print one version's section from CHANGELOG.md to stdout.
#
# Usage:
#   scripts/changelog-section.sh <version>
#
# Examples:
#   scripts/changelog-section.sh 0.1.4
#   scripts/changelog-section.sh v0.1.4   # leading 'v' is stripped
#
# The section is identified by the Keep a Changelog 1.1.0 heading
#   "## [<version>]"
# and the body is everything between that heading and the next "## ["
# heading, with surrounding blank lines trimmed.
#
# Pre-release tags (e.g. v0.2.0-rc1) need their own "## [0.2.0-rc1]" section
# too — this script doesn't special-case them.
#
# Exits non-zero (and prints to stderr) if the section is missing or empty,
# which is exactly what the release workflow wants: no CHANGELOG entry, no
# release.

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <version>" >&2
  exit 64
fi

version="${1#v}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
changelog="${script_dir}/../CHANGELOG.md"

if [[ ! -f "$changelog" ]]; then
  echo "error: $changelog not found" >&2
  exit 66
fi

# awk walks the file once:
#   - looks for the heading "## [<version>]" (matched as a literal substring
#     including the brackets, so 0.1.4 doesn't accidentally hit 0.1.40),
#   - then captures every following line until the next "## [" heading,
#   - and finally strips leading and trailing blank lines from the buffer.
body="$(awk -v marker="[${version}]" '
  /^## \[/ {
    if (in_section) { exit }
    if (index($0, marker) > 0) {
      in_section = 1
      next
    }
  }
  in_section {
    lines[++n] = $0
    if (NF) last = n
  }
  END {
    if (last == 0) exit
    for (first = 1; first <= last; first++) {
      if (lines[first] ~ /[^[:space:]]/) break
    }
    for (i = first; i <= last; i++) print lines[i]
  }
' "$changelog")"

if [[ -z "$body" ]]; then
  echo "error: no non-empty '## [${version}]' section found in ${changelog}" >&2
  exit 2
fi

printf '%s\n' "$body"
