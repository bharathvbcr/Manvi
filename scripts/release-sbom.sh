#!/usr/bin/env bash
# Write the CycloneDX SBOM for one release binary: dist/<artifact>.sbom.cdx.json.
#
# The Go module graph comes from the build info inside the binary, the Rust
# crates from `cargo metadata` for the engine's target triple, both through
# manvi/cmd/sbomgen (no third-party tool). A binary built without the engine
# (Windows, or MANVI_RELEASE_ENGINE=0) gets no RUST_TRIPLE and sbomgen checks
# that against the CGO_ENABLED the binary records, so an SBOM cannot omit the
# crates of a binary that links them.
#
# Inputs, set by release-build.sh through GITHUB_ENV or by hand:
#   artifact     file name under dist/ (after signing, so the digest is final)
#   RELEASE_TAG  the tag the binary was stamped with
#   rust_triple  empty when the binary has no engine
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

: "${artifact:?artifact is required (the file name under dist/)}"
: "${RELEASE_TAG:?RELEASE_TAG is required}"
binary="$root/dist/${artifact}"
test -s "$binary" || { echo "no binary at $binary" >&2; exit 1; }
sbom="${binary}.sbom.cdx.json"

args=(-binary "$binary" -tag "$RELEASE_TAG" -o "$sbom")
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
if [[ -n "${rust_triple:-}" ]]; then
  manifest="$root/../DevCouncil/rust/gusset-engine/Cargo.toml"
  test -f "$manifest" || { echo "missing $manifest; run scripts/fetch-modules.sh first" >&2; exit 1; }
  # --locked: the committed Cargo.lock decides the graph. --offline: the
  # build just fetched every crate; a metadata run must not reach the network.
  cargo metadata --format-version 1 --locked --offline \
    --manifest-path "$manifest" --filter-platform "$rust_triple" > "$tmp/cargo.json"
  args+=(-cargo-metadata "$tmp/cargo.json" -rust-target "$rust_triple")
fi

go -C manvi run ./cmd/sbomgen "${args[@]}"
test -s "$sbom"
echo "wrote ${sbom#"$root"/}"
