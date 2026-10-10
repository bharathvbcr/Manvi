#!/usr/bin/env bash
# Publish the GitHub Release for the tag that triggered this workflow.
#
# Notes are read from docs/releases/<tag>.md on disk. They are not assigned
# to an environment variable: GitHub caps those at 48 KB, and a notes file
# past that cap fails after the binaries have already been built.
#
# A second run edits the existing release and re-uploads artifacts. Creating
# again is what fails a re-run after the assets already exist.
#
# Besides the binaries the release carries SHA256SUMS and PROVENANCE.json.
# The provenance names the commit, workflow run, verify run, pinned modules and
# toolchain, and repeats each binary's digest, so a downloader can compare it
# with `gh run view` and `shasum -c`. It is a record, not a signature; see the
# release notes for what it does and does not prove.
#
# RELEASE_DRY_RUN=1 (workflow_dispatch) does every check, builds the files and
# prints the gh commands instead of running them. The tag need not exist then.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

: "${RELEASE_TAG:?RELEASE_TAG is required}"
: "${RELEASE_COMMIT:?RELEASE_COMMIT is required}"
dry_run="${RELEASE_DRY_RUN:-0}"

check_args=(--tag "$RELEASE_TAG" --assets dist)
if [[ "$dry_run" != "1" ]]; then
  check_args+=(--require-head)
fi
node scripts/check-release.mjs "${check_args[@]}"

notes="docs/releases/${RELEASE_TAG}.md"
binaries=()
while IFS= read -r file; do binaries+=("$file"); done < <(find dist -maxdepth 1 -type f -name 'manvi-*' | sort)
if [[ "${#binaries[@]}" -ne 4 || ! -s dist/SHA256SUMS ]]; then
  echo "expected 4 binaries and SHA256SUMS, found ${#binaries[@]} binaries" >&2
  printf '  %s\n' "${binaries[@]}" >&2
  exit 1
fi

command -v jq >/dev/null || { echo "jq is required to write PROVENANCE.json" >&2; exit 1; }
module_pins="$(awk -F'\t' '!/^#/ && NF >= 3 {printf "%s=%s\n", $1, $3}' scripts/module-pins.txt)"
jq -n \
  --arg tag "$RELEASE_TAG" \
  --arg commit "$RELEASE_COMMIT" \
  --arg repository "${GITHUB_REPOSITORY:-}" \
  --arg workflow "${GITHUB_WORKFLOW_REF:-}" \
  --arg run_id "${GITHUB_RUN_ID:-}" \
  --arg run_attempt "${GITHUB_RUN_ATTEMPT:-}" \
  --arg server "${GITHUB_SERVER_URL:-https://github.com}" \
  --arg verify_run "${VERIFY_RUN_URL:-}" \
  --arg rust "${RUST_TOOLCHAIN:-}" \
  --arg node "$(node --version)" \
  --arg modules "$module_pins" \
  --rawfile sums dist/SHA256SUMS \
  '{
    tag: $tag,
    commit: $commit,
    repository: $repository,
    workflow: $workflow,
    run: ($server + "/" + $repository + "/actions/runs/" + $run_id),
    run_attempt: $run_attempt,
    verify_run: $verify_run,
    toolchain: {rust: $rust, node: $node},
    modules: ($modules | split("\n") | map(select(length > 0) | split("=") | {(.[0]): .[1]}) | add),
    sha256: ($sums | split("\n") | map(select(length > 0) | capture("^(?<digest>[0-9a-f]{64})  (?<name>.+)$")) | map({(.name): .digest}) | add)
  }' > dist/PROVENANCE.json
test -s dist/PROVENANCE.json

files=("${binaries[@]}" dist/SHA256SUMS dist/PROVENANCE.json)
title="MANVI ${RELEASE_TAG}"

run() {
  if [[ "$dry_run" == "1" ]]; then
    printf 'dry run, would run:'
    printf ' %q' "$@"
    printf '\n'
  else
    "$@"
  fi
}

if [[ "$dry_run" != "1" ]] && gh release view "$RELEASE_TAG" >/dev/null 2>&1; then
  echo "Release $RELEASE_TAG already exists; uploading artifacts and replacing notes."
  gh release upload "$RELEASE_TAG" "${files[@]}" --clobber
  gh release edit "$RELEASE_TAG" --title "$title" --notes-file "$notes" --target "$RELEASE_COMMIT"
else
  run gh release create "$RELEASE_TAG" \
    --target "$RELEASE_COMMIT" \
    --title "$title" \
    --notes-file "$notes" \
    "${files[@]}"
fi
