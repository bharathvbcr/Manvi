# Analysis-plane crates (symlinks)

Canonical sources live in DevCouncil:

`../DevCouncil/rust/dc-*`. Every name in `Cargo.toml`'s `members` is a
symlink to the DevCouncil crate of the same name.

This directory keeps **symlinks** so `cargo build --manifest-path crates/Cargo.toml`
and `./verify.sh` keep working in a sibling checkout. DevCouncil is the component
layer; MANVI wraps the binaries at runtime and does not own these crates.

## The one thing mirrored by hand

A symlinked member's `foo.workspace = true` resolves against **this**
`Cargo.toml`'s `[workspace.dependencies]`, not DevCouncil's. When DevCouncil
adds or changes such a pin, copy it here. A missing key fails the workspace
load. A different version or feature set fails
`manvi/devcouncil/crate_pins_test.go`, which compares what `cargo metadata`
resolves for every member here against DevCouncil's `rust/` workspace. It also
fails if a member stops being a symlink into DevCouncil.

## Release / CI without a DevCouncil tree

Pin or vendor from DevCouncil rather than copying sources here:

- **git dependency** (preferred once published): path to the DevCouncil revision
  that owns the crates, or a tagged release of that repo.
- **GitPulse**: `scripts/vendor-crates.mjs` origins `dc-*` from DevCouncil
  `rust/` (not from these symlinks). GitPulse selects the modules it needs and
  updates them independently of this wrap.

Do not edit through the symlink and commit only on the Manvi side — the change
belongs in DevCouncil.
