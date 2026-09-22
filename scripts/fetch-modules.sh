#!/usr/bin/env bash
# Check out the module replacements manvi/go.mod names, at the pinned commits.
#
# From manvi/ the replace lines are ../../DevCouncil and ../../gusset. From
# this repository root, and from crates/, that is ../DevCouncil and ../gusset.
# On a GitHub runner the workspace is /home/runner/work/Manvi/Manvi, so the
# siblings land in /home/runner/work/Manvi/. A checkout already at the pin is
# left alone. Any other commit is a refusal: this script will not move a
# working tree.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
pins="$root/scripts/module-pins.txt"

if [[ ! -f "$pins" ]]; then
  echo "missing module pins: $pins" >&2
  exit 1
fi

fetch_one() {
  local name="$1" url="$2" sha="$3"
  local dest="$root/../$name"
  if [[ -e "$dest" && ! -d "$dest/.git" ]]; then
    echo "refusing $dest: it exists and is not a git checkout" >&2
    exit 1
  fi
  if [[ -d "$dest/.git" ]]; then
    local have
    have="$(git -C "$dest" rev-parse HEAD)"
    if [[ "$have" != "$sha" ]]; then
      echo "refusing to move $dest" >&2
      echo "  checkout is ${have}" >&2
      echo "  pin is      ${sha}" >&2
      echo "The release builds the pin. Update scripts/module-pins.txt or check out that commit." >&2
      exit 1
    fi
    echo "$name already at ${sha:0:12}"
    return 0
  fi
  if ! (
    set -euo pipefail
    mkdir -p "$dest"
    git -C "$dest" init --quiet
    git -C "$dest" remote add origin "$url"
    git -C "$dest" fetch --depth 1 origin "$sha"
    git -C "$dest" checkout --detach --quiet FETCH_HEAD
  ); then
    rm -rf "$dest"
    echo "failed to fetch $name at $sha" >&2
    exit 1
  fi
  local have
  have="$(git -C "$dest" rev-parse HEAD)"
  if [[ "$have" != "$sha" ]]; then
    echo "fetched $name at $have, pin is $sha" >&2
    exit 1
  fi
  echo "fetched $name at ${sha:0:12}"
}

count=0
while IFS=$'\t' read -r name url sha || [[ -n "${name:-}" ]]; do
  [[ -z "${name//[[:space:]]/}" ]] && continue
  [[ "$name" == \#* ]] && continue
  if [[ ! "$name" =~ ^[A-Za-z0-9._-]+$ ]]; then
    echo "bad module name: $name" >&2
    exit 1
  fi
  if [[ ! "$url" =~ ^https://github\.com/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+\.git$ ]]; then
    echo "bad module url: $url" >&2
    exit 1
  fi
  if [[ ! "$sha" =~ ^[0-9a-f]{40}$ ]]; then
    echo "bad module sha for $name: $sha" >&2
    exit 1
  fi
  fetch_one "$name" "$url" "$sha"
  count=$((count + 1))
done < "$pins"

if [[ "$count" -lt 1 ]]; then
  echo "module pin file listed nothing: $pins" >&2
  exit 1
fi
