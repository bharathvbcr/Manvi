#!/usr/bin/env bash
# Stamp one release binary. The version is the tag, checked before it is
# interpolated into -ldflags. workflow_dispatch may stamp a tag that is not
# HEAD; a tag push may not.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

event="${EVENT_NAME:-}"
if [[ "$event" == "workflow_dispatch" ]]; then
  version="${INPUT_TAG:-}"
else
  version="${REF_NAME:-}"
fi
goos="${GOOS:-}"
goarch="${GOARCH:-}"

case "${goos}/${goarch}" in
  darwin/amd64|darwin/arm64|linux/amd64|linux/arm64) ;;
  *)
    echo "unsupported target ${goos}/${goarch}" >&2
    exit 1
    ;;
esac

args=(--tag "$version")
if [[ "$event" != "workflow_dispatch" ]]; then
  args+=(--require-head)
fi
node scripts/check-release.mjs "${args[@]}"

out="manvi-${version}-${goos}-${goarch}"
mkdir -p dist
go -C manvi build \
  -trimpath \
  -ldflags "-s -w -X main.stampedVersion=${version}" \
  -o "../dist/${out}" \
  ./cmd/manvi
test -s "dist/${out}"
if [[ -n "${GITHUB_ENV:-}" ]]; then
  printf 'artifact=%s\n' "$out" >> "$GITHUB_ENV"
fi
echo "built dist/${out}"
