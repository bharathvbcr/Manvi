#!/usr/bin/env bash
# Builds the known downstream consumers of manvi/ against THIS checkout, so an
# exported symbol cannot be removed while a consumer still calls it.
#
# Neither `deadcode` nor `devmap dead` can see a caller in another repository,
# which is how cab5166 and 995027c removed symbols Jarvis still used and left
# it unbuildable (ft-8e5a8922). Consumers named here are checked with the
# compiler, the only check that sees them.
#
#   scripts/check-consumers.sh
#
# Environment:
#   MANVI_CONSUMER_JARVIS      Jarvis checkout (default: ../Jarvis beside this repo)
#   MANVI_CONSUMER_REPLACES    file of extra `replace` lines for the scratch
#                              go.work, for a checkout whose siblings are not
#                              where manvi/go.mod expects them
#
# Exit: 0 every consumer builds and vets; 1 a consumer fails; 2 the check could
# not run (consumer missing). 2 is not a pass: a gate that cannot reach its
# consumer must not read as one that checked it.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
jarvis=${MANVI_CONSUMER_JARVIS:-$root/../Jarvis}

if [ ! -f "$jarvis/go.mod" ]; then
	echo "check-consumers: Jarvis checkout not found at $jarvis (set MANVI_CONSUMER_JARVIS); consumers NOT checked" >&2
	exit 2
fi
jarvis=$(cd "$jarvis" && pwd)

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
{
	echo "go $(awk '/^go /{print $2; exit}' "$root/manvi/go.mod")"
	printf 'use (\n\t%s\n\t%s\n)\n' "$root/manvi" "$jarvis"
	if [ -n "${MANVI_CONSUMER_REPLACES:-}" ]; then
		cat "$MANVI_CONSUMER_REPLACES"
	fi
} >"$scratch/go.work"

cd "$jarvis"
export GOWORK="$scratch/go.work" CGO_ENABLED=0
echo "check-consumers: Jarvis ($jarvis) against Manvi ($root)"
go build ./...
go vet ./...
echo "check-consumers: PASS"
