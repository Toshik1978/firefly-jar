#!/usr/bin/env bash
#
# Fail unless the release tag matches the version CHANGELOG.md documents.
#
#   .github/scripts/check-release-version.sh v1.0.0
#
# WHY THIS EXISTS
#
# GoReleaser takes the release version from the git tag, and the version
# compiled *into* the binary is the same tag, stamped by the Go toolchain from
# git. Nothing else in this repository carries a version number, so
# CHANGELOG.md is the only remaining source of truth: its first "## " heading
# names the release this repository is ready to publish.
#
# This is a narrower check than extract-changelog.sh, and the two are not
# redundant. extract-changelog.sh only proves a "## <tag>" section exists
# somewhere in the file — a tag for last month's already-published release
# still passes it. This script proves the tag is the *current* entry, the
# heading at the top, so tagging an old or mistyped version fails here
# instead of publishing archives nobody expects.

set -euo pipefail

tag="${1:-}"
changelog="${2:-CHANGELOG.md}"

if [ -z "$tag" ]; then
  echo "usage: $(basename "$0") <tag> [changelog-path]" >&2
  exit 2
fi

if [ ! -f "$changelog" ]; then
  echo "error: $changelog not found" >&2
  exit 2
fi

# The first "## " heading is the current release: "## v1.0.0 — 2026-08-01".
# Its second field is the version, whichever form the heading takes.
latest=$(awk '/^## /{print $2; exit}' "$changelog")

if [ -z "$latest" ]; then
  echo "error: no '## ' release heading found in $changelog" >&2
  exit 2
fi

if [ "$tag" != "$latest" ]; then
  echo "error: tag '$tag' does not match the current changelog entry '$latest'" >&2
  echo "  tag says:                                 $tag" >&2
  echo "  $changelog's top release heading says:  $latest" >&2
  echo "hint: write the changelog entry for $tag and move it to the top" >&2
  echo "      before tagging, or tag the version the changelog documents." >&2
  exit 1
fi

echo "release version: $latest (tag $tag)"
