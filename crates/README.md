# Analysis-plane crates (symlinks)

Canonical sources live in DevCouncil:

`../DevCouncil/rust/{dc-glob,dc-grep,dc-store,dc-verify,dc-evidence}`

This directory keeps **symlinks** so `cargo build --manifest-path crates/Cargo.toml`
and `./verify.sh` keep working in a sibling checkout.

## Release / CI without a DevCouncil tree

Pin or vendor from DevCouncil rather than copying sources here:

- **git dependency** (preferred once published): path to the DevCouncil revision
  that owns the crates, or a tagged release of that repo.
- **GitPulse**: `scripts/vendor-crates.mjs` origins `dc-*` from DevCouncil
  `rust/` (not from these symlinks).

Do not edit through the symlink and commit only on the Manvi side — the change
belongs in DevCouncil.
