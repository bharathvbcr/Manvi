# Analysis-plane crates (symlinks)

Canonical sources live in DevCouncil:

`../DevCouncil/rust/{dc-glob,dc-grep,dc-store,dc-verify,dc-evidence,dc-proc}`

This directory keeps **symlinks** so `cargo build --manifest-path crates/Cargo.toml`
and `./verify.sh` keep working in a sibling checkout. DevCouncil is the component
layer; MANVI wraps the binaries at runtime and does not own these crates.

## Release / CI without a DevCouncil tree

Pin or vendor from DevCouncil rather than copying sources here:

- **git dependency** (preferred once published): path to the DevCouncil revision
  that owns the crates, or a tagged release of that repo.
- **GitPulse**: `scripts/vendor-crates.mjs` origins `dc-*` from DevCouncil
  `rust/` (not from these symlinks). GitPulse selects the modules it needs and
  updates them independently of this wrap.

Do not edit through the symlink and commit only on the Manvi side — the change
belongs in DevCouncil.

## What is still copied, and how it is kept honest

The sources have one owner, but a symlinked member inherits its `version`,
`license`, lints and `workspace = true` dependency pins from `Cargo.toml`
here, not from DevCouncil's, and those values are copied by hand. They are
DevCouncil's values **at the commit `scripts/module-pins.txt` pins**, because
that is what CI and the release build. `scripts/check-release.mjs`
(`crateMirrorProblems`, run by `./verify.sh`) compares every inherited value
against DevCouncil's manifests at that pin, read through git, and refuses a
member that is a copied directory rather than a symlink. Bump the pin and the
check names each value to update.
