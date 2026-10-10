#!/usr/bin/env bash
# The two guards over a verify.sh log that every verify.yml job runs after
# verify.sh, so that no job can pass on a gate that did not run.
#
#   usage: check-verify-log.sh <verify.log> <runner os label> [default|race|fuzz]
#
# verify.sh degrades a gate it cannot run to NOT COVERED and still exits 0,
# which is the honest answer on a laptop and the wrong one on a runner:
#   1. a missing analysis tool is a provisioning failure, not a property of the
#      code under test, and
#   2. every other NOT COVERED line and every skipped test must be listed for
#      this runner and mode in scripts/verify-expected-gaps.json.
# The deep jobs (--race, --fuzz) ran neither guard until this was a script both
# jobs call: a fuzz leg that degraded to NOT COVERED was a green check.
set -euo pipefail

log="${1:?usage: check-verify-log.sh <verify.log> <runner os label> [mode]}"
os="${2:?usage: check-verify-log.sh <verify.log> <runner os label> [mode]}"
mode="${3:-default}"
here="$(cd "$(dirname "$0")" && pwd)"

if [[ ! -s "$log" ]]; then
  echo "verify.sh produced no output; there is no verdict to check." >&2
  exit 1
fi
if grep -n 'is not installed' "$log"; then
  echo >&2
  echo "An analysis tool was missing, so its gate did not run." >&2
  echo "This is a provisioning failure in the 'Analysis tools' step," >&2
  echo "not a property of the code under test." >&2
  exit 1
fi
echo "No analysis tool was missing."
node "$here/check-verify-gaps.mjs" "$log" "$os" "$mode"
