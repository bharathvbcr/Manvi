#!/usr/bin/env bash
# Withdraw a bad GitHub Release and put the previous one back as "latest".
#
# Inputs (environment; rollback-release.yml fills them from workflow_dispatch):
#   ROLLBACK_TAG    the tag to withdraw, vX.Y.Z
#   CONFIRM_TAG     must equal ROLLBACK_TAG exactly
#   MODE            prerelease (default) or draft
#   DELETE_ASSETS   true to also delete the release's assets (default false)
#   DELETE_TAG      true to also delete the git tag (default false); never implied
#   PREVIOUS_TAG    release to restore as latest; default is the highest-semver published
#                   non-draft, non-pre-release release other than ROLLBACK_TAG
#   ROLLBACK_DRY_RUN=1 prints the mutating commands instead of running them
#   GH_TOKEN, GH_REPO as for any gh call
#
# The release is marked first, so users stop seeing it as latest before
# anything irreversible happens. Assets and the tag are removed only on
# request, and the tag only by its own input.
set -euo pipefail

tag="${ROLLBACK_TAG:-}"
confirm="${CONFIRM_TAG:-}"
mode="${MODE:-prerelease}"
delete_assets="${DELETE_ASSETS:-false}"
delete_tag="${DELETE_TAG:-false}"
previous="${PREVIOUS_TAG:-}"
dry_run="${ROLLBACK_DRY_RUN:-0}"

fail() { echo "release-rollback: $*" >&2; exit 1; }

[[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || fail "tag '${tag}' must be vX.Y.Z"
[[ "$confirm" == "$tag" ]] || fail "confirmation '${confirm}' does not equal the tag '${tag}'; nothing was changed"
case "$mode" in prerelease|draft) ;; *) fail "MODE must be prerelease or draft, not '${mode}'" ;; esac
case "$delete_assets" in true|false) ;; *) fail "DELETE_ASSETS must be true or false" ;; esac
case "$delete_tag" in true|false) ;; *) fail "DELETE_TAG must be true or false" ;; esac
if [[ -n "$previous" ]]; then
  [[ "$previous" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || fail "PREVIOUS_TAG '${previous}' must be vX.Y.Z"
  [[ "$previous" != "$tag" ]] || fail "PREVIOUS_TAG must differ from the tag being withdrawn"
fi
command -v gh >/dev/null || fail "gh is required"
command -v jq >/dev/null || fail "jq is required"

run() {
  if [[ "$dry_run" == "1" ]]; then
    printf 'dry run, would run:'
    printf ' %q' "$@"
    printf '\n'
  else
    "$@"
  fi
}

gh release view "$tag" >/dev/null 2>&1 || fail "release $tag does not exist"

# Resolve the replacement before changing anything, so a missing one is
# reported while the release is still untouched.
if [[ -n "$previous" ]]; then
  state="$(gh release view "$previous" --json isDraft,isPrerelease)" || fail "release $previous does not exist"
  [[ "$(jq -r '.isDraft or .isPrerelease' <<<"$state")" == "false" ]] || fail "$previous is a draft or pre-release and cannot be latest"
else
  # Highest semver among the published, non-pre-release tags, not the most
  # recently published: a patch release of an old line published today must
  # not displace a newer line as latest.
  previous="$(gh release list --exclude-drafts --exclude-pre-releases --limit 100 --json tagName \
    | jq -r --arg tag "$tag" '
        [.[] | select(.tagName != $tag and (.tagName | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")))
             | {tagName, k: (.tagName | ltrimstr("v") | split(".") | map(tonumber))}]
        | sort_by(.k) | last | .tagName // empty')"
fi

echo "Withdrawing $tag (mode: $mode, delete assets: $delete_assets, delete tag: $delete_tag)"
if [[ "$mode" == "draft" ]]; then
  run gh release edit "$tag" --draft --latest=false
else
  run gh release edit "$tag" --prerelease --latest=false
fi

if [[ "$delete_assets" == "true" ]]; then
  while IFS= read -r asset; do
    [[ -n "$asset" ]] || continue
    run gh release delete-asset "$tag" "$asset" --yes
  done < <(gh release view "$tag" --json assets --jq '.assets[].name')
fi

if [[ -n "$previous" ]]; then
  echo "Restoring $previous as the latest release"
  run gh release edit "$previous" --latest
else
  echo "release-rollback: WARNING: no earlier published release exists to restore as latest" >&2
fi

if [[ "$delete_tag" == "true" ]]; then
  : "${GH_REPO:?GH_REPO is required to delete the tag}"
  echo "Deleting git tag $tag"
  run gh api --method DELETE "repos/${GH_REPO}/git/refs/tags/${tag}"
else
  echo "Git tag $tag left in place."
fi
