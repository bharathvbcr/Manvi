#!/usr/bin/env bash
# Throwaway: the darwin/arm64 branch of release-build.sh without a tag stamp.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
rev="$(git rev-parse --short HEAD)"
out="dist/manvi-${rev}-darwin-arm64"
export MACOSX_DEPLOYMENT_TARGET=12.0
export CC="clang -arch arm64"
exports="$("$root/../DevCouncil/rust/gusset-engine/cgo-env.sh" --export --target=aarch64-apple-darwin)"
eval "$exports"
go -C manvi build -trimpath -ldflags "-s -w" -o "../${out}" ./cmd/manvi
test -s "$out"
minos="$(otool -l "$out" | awk '/LC_BUILD_VERSION/{f=1} f&&/minos/{print $2; exit}')"
echo "$out: minimum macOS ${minos:-unknown}"
"$out" gusset-check
echo "built $out"
