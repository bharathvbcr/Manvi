#!/usr/bin/env bash
# Check out the module replacements manvi/go.mod names, at the pinned commits.
#
# From manvi/ the replace lines are ../../DevCouncil and ../../gusset. From
# this repository root, and from crates/, that is ../DevCouncil and ../gusset.
# On a GitHub runner the workspace is /home/runner/work/Manvi/Manvi, so the
# siblings land in /home/runner/work/Manvi/. A checkout already at the pin is
# left alone. Any other commit is a refusal: this script will not move a
# working tree.
#
# A linked worktree that is not itself a sibling of DevCouncil and gusset (one
# nested under the main checkout, such as .claude/worktrees/<name> or
# .gitpulse/worktrees/<name>) resolves ../DevCouncil to a directory that does
# not exist. `fetch-modules.sh --link-main` fixes that without a second clone:
# it symlinks <worktree>/../<name> to the sibling of the main worktree, which
# must itself be at the pin. It never replaces or moves anything that exists.
# Run plain `fetch-modules.sh` in the main checkout first if it has none.
set -euo pipefail

link_main=0
case "${1:-}" in
  "") ;;
  --link-main) link_main=1 ;;
  *)
    echo "usage: fetch-modules.sh [--link-main]" >&2
    exit 2
    ;;
esac

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

# The main worktree's directory: the parent of the shared git dir.
main_worktree() {
  local common
  common="$(git -C "$root" rev-parse --path-format=absolute --git-common-dir)"
  dirname "$common"
}

link_one() {
  local name="$1" sha="$3"
  local dest="$root/../$name" main source have
  main="$(main_worktree)"
  if [[ "$(cd "$main" && pwd -P)" == "$(cd "$root" && pwd -P)" ]]; then
    echo "$root is the main worktree; run scripts/fetch-modules.sh without --link-main" >&2
    exit 1
  fi
  source="$(cd "$main/.." && pwd -P)/$name"
  if [[ ! -d "$source/.git" ]]; then
    echo "no $name checkout beside the main worktree at $source; run scripts/fetch-modules.sh there first" >&2
    exit 1
  fi
  have="$(git -C "$source" rev-parse HEAD)"
  if [[ "$have" != "$sha" ]]; then
    echo "refusing to link $source: it is at ${have}, pin is ${sha}" >&2
    exit 1
  fi
  if [[ -e "$dest" || -L "$dest" ]]; then
    if [[ "$(cd "$dest" 2>/dev/null && pwd -P)" == "$(cd "$source" && pwd -P)" ]]; then
      echo "$name already resolves to ${sha:0:12} at $dest"
      return 0
    fi
    echo "refusing to replace $dest: it exists and is not $source" >&2
    exit 1
  fi
  ln -s "$source" "$dest"
  echo "linked $dest -> $source at ${sha:0:12}"
}

count=0
while IFS=$'\t' read -r name url sha || [[ -n "${name:-}" ]]; do
  # A Windows checkout writes CRLF. `read` leaves the CR on the last field, and
  # the 40-hex check then rejects a pin that is otherwise exact: the message
  # prints the sha with the CR invisible, which is what the windows-latest job
  # reported for every commit it fetched. Strip it before any check.
  name="${name//$'\r'/}"
  url="${url//$'\r'/}"
  sha="${sha//$'\r'/}"
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
  if [[ "$link_main" == 1 ]]; then
    link_one "$name" "$url" "$sha"
  else
    fetch_one "$name" "$url" "$sha"
  fi
  count=$((count + 1))
done < "$pins"

if [[ "$count" -lt 1 ]]; then
  echo "module pin file listed nothing: $pins" >&2
  exit 1
fi
