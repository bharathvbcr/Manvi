# Analysis-plane crates (symlinks)

Canonical sources live in DevCouncil:

`../DevCouncil/rust/dc-*`. Every name in `Cargo.toml`'s `members` is a
symlink to the DevCouncil crate of the same name.

This directory keeps **symlinks** so `cargo build --manifest-path crates/Cargo.toml`
and `./verify.sh` keep working in a sibling checkout. DevCouncil is the component
layer; MANVI wraps the binaries at runtime and does not own these crates.

## What is still copied, and how it is kept honest

The sources have one owner, but a symlinked member inherits its `version`,
`license`, lints and `foo.workspace = true` dependency pins from `Cargo.toml`
here, not from DevCouncil's, and those values are copied by hand. When
DevCouncil adds or changes one, copy it here. A missing dependency key fails
the workspace load. One check catches the rest:

- `manvi/devcouncil/crate_pins_test.go` (`TestCratePinsMatchDevCouncil`, run by
  `go test ./...`) is the single owner. It compares what `cargo metadata`
  resolves for every member (version, licence, edition and each dependency
  spec) and the `[workspace.lints]` tables against DevCouncil's `rust/` **at
  the commit `scripts/module-pins.txt` pins**, exported with `git archive`
  from the sibling checkout, because that is what CI and the release build. A
  newer or edited sibling working tree changes nothing. It also fails if a
  member stops being a symlink into DevCouncil. Bump the pin and the failure
  names each value to update.

## Release / CI without a DevCouncil tree

Pin or vendor from DevCouncil rather than copying sources here:

- **git dependency** (preferred once published): path to the DevCouncil revision
  that owns the crates, or a tagged release of that repo.
- **GitPulse**: `scripts/vendor-crates.mjs` origins `dc-*` from DevCouncil
  `rust/` (not from these symlinks). GitPulse selects the modules it needs and
  updates them independently of this wrap.

Do not edit through the symlink and commit only on the Manvi side — the change
belongs in DevCouncil.
