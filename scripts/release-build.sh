#!/usr/bin/env bash
# Stamp one release binary. The version is the tag, checked before it is
# interpolated into -ldflags. workflow_dispatch may stamp a tag that is not
# HEAD; a tag push may not.
#
# The binary links the Gusset engine, which makes every policy decision in a
# cgo build (DevCouncil's policy.Matcher). That takes a native job per target:
# Linux is linked fully static against musl, so it runs on any distribution;
# darwin links libSystem as every macOS binary does. The binary runs its own
# `gusset-check` before it is accepted: parity through the boundary and a real
# Rust panic that must come back as ErrPanic (I2). A static build without an
# unwind table links and matches and then aborts on that panic, which is the
# failure this step exists to catch.
#
# MANVI_RELEASE_ENGINE=0 builds the old CGO_ENABLED=0 binary, which decides
# with Go's fnmatch. For a local dry run only; the workflow never sets it.
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
ldflags="-s -w -X main.stampedVersion=${version}"
tags=""

if [[ "${MANVI_RELEASE_ENGINE:-1}" == "0" ]]; then
  echo "MANVI_RELEASE_ENGINE=0: building without the engine (CGO_ENABLED=0)" >&2
  export CGO_ENABLED=0
else
  case "${goos}/${goarch}" in
    linux/amd64) triple=x86_64-unknown-linux-musl ;;
    linux/arm64) triple=aarch64-unknown-linux-musl ;;
    darwin/amd64) triple=x86_64-apple-darwin ;;
    darwin/arm64) triple=aarch64-apple-darwin ;;
  esac
  cgo_env="$root/../DevCouncil/rust/gusset-engine/cgo-env.sh"
  if [[ ! -x "$cgo_env" ]]; then
    echo "missing $cgo_env; run scripts/fetch-modules.sh first" >&2
    exit 1
  fi
  # Captured before eval: `eval "$(failing)"` evaluates the empty output and
  # succeeds, and the build then links whatever archive is lying around.
  exports="$("$cgo_env" --export --target="$triple")"
  eval "$exports"
  if [[ "$goos" == linux ]]; then
    : "${GUSSET_STATIC_EXTLDFLAGS:?cgo-env.sh printed no static link flags for $triple}"
    tags="netgo,osusergo"
    ldflags+=" -linkmode external -extldflags '${GUSSET_STATIC_EXTLDFLAGS}'"
  else
    # One clang builds both darwin architectures; -arch picks the one the
    # Rust archive was built for, including x86_64 on an arm64 runner.
    case "$goarch" in
      amd64) export CC="clang -arch x86_64" ;;
      arm64) export CC="clang -arch arm64" ;;
    esac
  fi
fi

go -C manvi build \
  -trimpath \
  ${tags:+-tags "$tags"} \
  -ldflags "$ldflags" \
  -o "../dist/${out}" \
  ./cmd/manvi
test -s "dist/${out}"

if [[ "${MANVI_RELEASE_ENGINE:-1}" != "0" ]]; then
  if [[ "$goos" == linux ]] && ! file "dist/${out}" | grep -q "statically linked"; then
    echo "dist/${out} is not statically linked:" >&2
    file "dist/${out}" >&2
    exit 1
  fi
  # Exit 2 is "engine not linked", 1 a failed check; either refuses the
  # artifact. darwin/amd64 runs under Rosetta on an arm64 runner.
  "dist/${out}" gusset-check
fi
if [[ -n "${GITHUB_ENV:-}" ]]; then
  printf 'artifact=%s\n' "$out" >> "$GITHUB_ENV"
fi
echo "built dist/${out}"
