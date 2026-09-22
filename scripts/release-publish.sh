#!/usr/bin/env bash
# Publish the GitHub Release for the tag that triggered this workflow.
#
# Notes are read from docs/releases/<tag>.md on disk. They are not assigned
# to an environment variable: GitHub caps those at 48 KB, and a notes file
# past that cap fails after the binaries have already been built.
#
# A second run edits the existing release and re-uploads artifacts. Creating
# again is what fails a re-run after the assets already exist.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

: "${RELEASE_TAG:?RELEASE_TAG is required}"
: "${RELEASE_COMMIT:?RELEASE_COMMIT is required}"

node scripts/check-release.mjs --tag "$RELEASE_TAG" --require-head --assets dist

notes="docs/releases/${RELEASE_TAG}.md"
mapfile -t files < <(find dist -maxdepth 1 -type f \( -name 'manvi-*' -o -name 'SHA256SUMS' \) | sort)
if [[ "${#files[@]}" -ne 5 ]]; then
  echo "expected 4 binaries and SHA256SUMS, found ${#files[@]}" >&2
  printf '  %s\n' "${files[@]}" >&2
  exit 1
fi

title="MANVI ${RELEASE_TAG}"
if gh release view "$RELEASE_TAG" >/dev/null 2>&1; then
  echo "Release $RELEASE_TAG already exists; uploading artifacts and replacing notes."
  gh release upload "$RELEASE_TAG" "${files[@]}" --clobber
  gh release edit "$RELEASE_TAG" --title "$title" --notes-file "$notes" --target "$RELEASE_COMMIT"
else
  gh release create "$RELEASE_TAG" \
    --target "$RELEASE_COMMIT" \
    --title "$title" \
    --notes-file "$notes" \
    "${files[@]}"
fi
