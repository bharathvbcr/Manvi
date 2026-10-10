#!/usr/bin/env bash
# Refuse to release a commit the verify workflow has not passed.
#
# A tag push builds whatever commit it names. Nothing else ties that commit to
# a green run of verify.yml, so a red main could still ship binaries. This
# asks GitHub for completed runs of verify.yml at exactly RELEASE_COMMIT and
# requires one that succeeded. No run, a run still in progress, and a failed
# run all refuse: a check that could not be answered is not a pass.
#
# Needs GH_TOKEN and GH_REPO (the job grants actions: read). Prints the
# accepted run's URL on stdout's last line so the publish job can record it.
set -euo pipefail

: "${RELEASE_COMMIT:?RELEASE_COMMIT is required}"
: "${GH_REPO:?GH_REPO is required}"
if [[ ! "$RELEASE_COMMIT" =~ ^[0-9a-f]{40}$ ]]; then
  echo "RELEASE_COMMIT is not a full commit id: $RELEASE_COMMIT" >&2
  exit 1
fi

# A bounded page: the newest runs at this commit are the ones that matter.
runs="$(gh run list --workflow verify.yml --commit "$RELEASE_COMMIT" --limit 50 \
  --json databaseId,headSha,status,conclusion,url \
  --jq '.[] | [.databaseId, .headSha, .status, .conclusion, .url] | @tsv')" || {
  echo "could not list verify runs for $RELEASE_COMMIT; not releasing" >&2
  exit 1
}

url=""
seen=0
while IFS=$'\t' read -r id sha status conclusion run_url; do
  [[ -z "${id:-}" ]] && continue
  seen=$((seen + 1))
  if [[ "$sha" == "$RELEASE_COMMIT" && "$status" == "completed" && "$conclusion" == "success" ]]; then
    url="$run_url"
    break
  fi
done <<< "$runs"

if [[ -z "$url" ]]; then
  echo "no successful verify run at $RELEASE_COMMIT ($seen run(s) found); push the tag after verify is green on this commit" >&2
  exit 1
fi
echo "verify passed at $RELEASE_COMMIT"
echo "$url"
if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  printf 'run_url=%s\n' "$url" >> "$GITHUB_OUTPUT"
fi
