# Changelog

What changed, per release, for someone deciding whether to upgrade.

This is not the [hardening ledger](docs/HARDENING_LEDGER.md). That document is
organised by *defect* — the pattern, its root cause, and the regression test
that now holds it — and it is the better read for understanding why a rule
exists. This one is organised by *release*, and answers a different question:
what is different in the build I am about to run.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions are `MAJOR.MINOR.PATCH`; while the leading zero stands, the surface —
CLI flags, exit statuses, the settings catalogue, the `manvi serve` protocol,
and the native tool schemas — may change in any release, and a change to any of
them is listed here.

Entries are written with the change, not reconstructed afterwards. Add yours
under **Unreleased** in the same commit as the code.

---

## [Unreleased]

Nothing yet.

---

## [0.0.5] — 2026-09-14

Re-cut from the 2026-09-11 build. The entries below landed after that stamp and
ship under the same version; everything from the original 0.0.5 follows them
unchanged.

### Removed

- **`verify.rigor.enabled` and `verify.diff_coverage.enforce` are gone.**
  DevCouncil retired both as switches nothing read, and `manvi/devcouncil` was
  the last thing still reading them — so the package stopped compiling against
  a current sibling checkout. The gates they named run in `dcverify`, reached
  by finding the binary; neither key ever decided whether a gate ran, so
  setting `enforce: true` bought a belief the run did not honour. Their
  defaults are now the whole behaviour: `stub_detection` findings are always
  kept, and an unmeasured or uncovered diff is always reported and never
  blocks. **A config file or `MANVI_*` variable still setting either key is
  refused by name at startup** — remove them. DevCouncil's own `verification.*`
  spellings in `.devcouncil/config.yaml` are unaffected.

### Changed

- **The macOS input guard is partitioned by what the input can commit.** It
  demanded exact equality across all fifteen `HIDSystemState` counters,
  `MouseMoved` included, so anyone touching the mouse aborted an otherwise
  valid run — a 40-run campaign suspended 17 times while all 40 independent
  saved-state checks passed. Buttons, keys, modifiers, scroll and drags can
  change application state and stay a hard refusal; `MouseMoved` and
  `TabletPointer` cannot press, type, scroll or drag, so element-addressed work
  (`observe`, `AXPress`, `AXValue`) reports them instead of aborting, and still
  revalidates the window, the scoped accessibility fingerprint and the target
  before dispatch. Coordinate-addressed input — `TypeText`, `Click`, `Scroll`,
  every visual click — still refuses motion, because there the pointer position
  is part of the action. *Security impact: this narrows the guard, it does not
  remove it.* Nothing that can change application state is newly admitted, no
  pre-dispatch revalidation is bypassed, and tolerated motion is recorded as
  `pointer_motion` on the observation and receipt so evidence still shows a
  person was present. The field is omitted when nothing was observed, so quiet
  runs serialize exactly as before.
- Document DevCouncil as independently selectable components and modules, Manvi
  as the wrap around them, and GitPulse as a host that takes both for their
  respective jobs.

### Added

- **Claude Code managed adapter.** Added managed session adapter for Claude Code (`codingagent.StartClaude`) driven over stdio `stream-json` transport with `--permission-prompt-tool stdio` approval proxying.
- **Runtime managed provider publishing.** `manvi serve` now dynamically advertises supported `ManagedProviders` in its `HelloResult` handshake payload, enabling clients to verify agent capabilities before claiming runs.
- **Gusset diagnostic check.** Added `manvi gusset-check` CLI subcommand and `gussetcheck` package verifying DevCouncil's linked Gusset diagnostic engine and pool sizing before execution.
- **An external-interaction refusal names the input category.** A refusal said
  only that hardware input changed, so an operator could not tell a person
  using the machine from a misfiring guard — a 40-run campaign suspended 22
  times with no way to attribute one of them. Refusals now name the coarse
  category that moved (`keyboard`, `scroll`, `drag`, `pointer_button`),
  deduplicated and ordered. Deliberately coarse: no counts, keycodes, buttons
  or pointer coordinates reach the message, and a test asserts it.
- **`replay.Decode` takes fixture bytes and a source label**, so a consumer can
  ship a fixture inside its binary. `replay.Load` reads the file and delegates;
  no behaviour change for existing callers. Jarvis's `--provider fixture`
  rehearsal derived its transcript path from `runtime.Caller`, which under
  `-trimpath` resolves to a module-relative name that is not on disk — it
  worked under `go test` and failed in the shipped binary.
- **`TransientObservation` names the re-observable capture failures once** —
  `observation_inconsistent`, `window_changed`, `state_changed`,
  `window_ambiguous`, `window_occluded` — so the execution loop and callers
  outside it agree on what a bounded re-observation may resolve. Jarvis's drift
  diagnostic did not retry at all and failed where replay succeeds.

### Fixed

- **Offline deterministic replay rejected its own valid journals.** `NewState`
  allocated `RecoveryCounts` as an empty non-nil map while the field is tagged
  `omitempty`, so encoding dropped it and decoding yielded nil, and a recorded
  trace never compared equal to a recomputed canonical admission.
  `ResultFromState` had the same shape on `Result.Outputs`. Allocating only
  when non-empty restores round-trip equality and leaves the wire format
  byte-identical, so existing evidence bundles and their hashes stay valid. A
  reflective guard now fails any map or slice field tagged `omitempty` but
  allocated empty, so the shape cannot return on a future field.
- **The `crates/` workspace failed to load, not just one member.** A symlinked
  member inherits `workspace = true` from `crates/Cargo.toml` rather than from
  DevCouncil's, so upstream's `sha2` (dc-evidence) and `libc` (dc-proc) had to
  be mirrored there, and `dc-verify`'s path dependency on the new `dc-proc`
  needed a sixth symlink.
- **`devcouncil_dev_inspect` routed to an Extended tool from the Core profile.**
  The tool description and unavailable error recommended `devcouncil_get_gaps`,
  which the core profile does not offer — stranding an agent running under
  `llm.local.core_tools_only` with an unknown-tool error. Replaced with
  `devcouncil_verify_task, manvi map`, which are both in Core, resolving
  `TestNoToolNamesAToolTheCoreProfileDoesNotOffer`.

### Added

- **Profile workbench storage and opt-in `work.*` host operations.**
  Persistent workspaces, board items, enhancement proposals, automation
  schedules, run records, and managed Codex activation over
  `manvi serve --workbench-db`. Storage lives in `dc-store` (`work_*`
  tables); the Go host advertises the operations only when that flag is
  set. Documented in [WORKBENCH.md](docs/WORKBENCH.md).
- **Computer-use foundations, typed multimodal tool results, and a native
  desktop broker.** Tool results carry text or image blocks; Gemini accepts
  image function results; session projection publishes scrubbed events
  without rewriting private image bytes. `computer` drives a Rust
  `manvi-desktop` broker over NDJSON stdio, with optional `llm/budget`
  admission before each HTTP attempt. Documented in
  [COMPUTER_USE_FOUNDATIONS.md](docs/COMPUTER_USE_FOUNDATIONS.md).
- **Workflow capability compiler.** Pure compile/reduce in `manvi/workflow`,
  interpreted by the computer-use runner rather than by the LLM loop.
- **Published module path `github.com/bharathvbcr/Manvi/manvi`.**
  `go install manvi/cmd/manvi@latest` was impossible while the module was the
  bare name `manvi` (`missing dot in first path element`). Remote install is
  now `go install github.com/bharathvbcr/Manvi/manvi/cmd/manvi@latest`;
  contributors keep `go -C manvi install ./cmd/manvi` from a checkout.
- **GitHub Actions release workflow for darwin/linux amd64+arm64.**
  Tag `v*` builds with `CGO_ENABLED=0` and stamps `-X main.stampedVersion`, then
  attaches SHA-256 checksums. No Windows target (see `verify.yml`).
- **Documented `go -C manvi install ./cmd/manvi` as the PATH install one-liner
  for a local checkout.** Puts the CLI in `~/go/bin` (or `GOBIN`) so GUI hosts
  that do not inherit a shell profile can still find it.
- **Retarget analysis-crate symlinks at DevCouncil `rust/`.** `dc-glob`,
  `dc-grep`, `dc-store`, `dc-verify`, and `dc-evidence` follow the flattened
  workspace instead of `rust-port/crates`.
- **Harden enhancement JSON extraction.** Constraint sentences stay verbatim,
  fenced or prefixed model output is still one object, and a stress suite
  covers oversized and adversarial replies.

### Fixed

- **CI would have failed on Linux on its first run.** `.golangci-debt.counts`
  was measured on one platform, and `unconvert` reads 1 finding on darwin
  against 3 on linux — `Stat_t`'s field widths are per-GOOS, so the conversions
  in `devcouncil/safefs.go` are load-bearing on darwin and redundant on linux.
  The ratchet fails on any increase, so a green tree would have gone red on
  `ubuntu-latest` with nothing changed. The conversions now carry the
  portability reason in place, the count is 0 on both platforms, and
  `unconvert` has moved to the enforced set — 21 linters at zero tolerance, 10
  left in the debt file.
- **The Windows CI job would also have failed on its first run**, and its
  premise was wrong. This tree does not build for Windows: `manvi/artifacts`
  fails on `syscall.O_NOFOLLOW` and `manvi/devcouncil` on `syscall.Stat_t`.
  Both are load-bearing — `O_NOFOLLOW` is how the artifact store refuses to
  follow a symlink out of itself, and `Stat_t` carries the inode pin and the
  `st_nlink` hard-link check — so a Windows build needs real equivalents rather
  than stubs. The job is gone and the reason is written down where a reader
  will find it, rather than a green check standing in for a port that does not
  exist.
- **Four documents called the policy ladder "5-tier"; it has six rungs.** The
  guard for this existed and matched only the literal phrase "N-tier policy
  ladder", so a "5-tier ladder", a "5-tier policy gate", a "5-tier evaluation
  ladder" and a "5-tier policy composition" all drifted past it while it
  reported clean. It now matches the number and the noun wherever they appear,
  and reads the HTML guides too — the drift that survived longest was in one of
  them.

### Added

- **`local.scan` on the `manvi serve` protocol**, which lists the model servers
  running on the machine and the models each one advertises. The discovery
  itself already existed — it probes the well-known loopback endpoints
  concurrently and identifies every runtime by asking it, never by assuming
  whichever runtime conventionally holds the port that answered — but no host
  could reach it, because `capability.probe` needs the model name up front and
  that is the answer rather than the question. Every model carries
  `capabilities_known` beside the capability flags, so "does not support tools"
  and "nobody asked" stay two different answers instead of the same `false`.
  Documented in [the host plane reference](docs/SERVE_HOST_PLANE.md), along
  with `chat.forget`, which was on the wire and in the served set but had no
  worked example.
- **Tests for `llm/replay`**, which had none. Its playback half was exercised
  second-hand by eight other packages; its recording half — `Load`,
  `NewRecord`, `Save`, and the recording stream — had been executed by nothing
  at all, in a package whose doc comment offers exactly that as the way a real
  session becomes an offline regression test. It works; it had simply never
  been run. The package goes from 0% self-coverage to 96.8%, with no function
  left unexecuted, and the tree total moves to 82.0%.
- **`SECURITY.md`**, pointing at this repository's private vulnerability
  reporting. It says what counts (a way past a gate, a credential leaving the
  machine, a check that reports success without running) and what does not (the
  `dev` posture doing what `dev` documents, an operator's own allowlist entry).
- **`.github/dependabot.yml`** for all three ecosystems — actions, gomod, and
  cargo. `govulncheck` and `cargo audit` catch a *vulnerable* dependency on
  every run; nothing was watching for a merely old one, and nothing looked at
  the actions at all.

### Changed

- **Every GitHub action is pinned by commit SHA**, with its version in a
  comment beside it. A tag is a mutable reference, and CI is the one place a
  silent substitution would run with a token in scope. Dependabot moves the
  pins; `verify.sh` decides whether the move is safe.
- **`UnusedExports` runs.** The repository's own dead-code detector — written,
  and documented with the defect it exists to catch — was called by nothing. It
  now reports alongside the other contract scans. It stands at 147 findings, of
  which twelve are its own documented blind spot (`internal/testsupport` and
  `llm/adaptertest` exist to be called from tests, and test files are
  deliberately not counted as callers). It reports rather than gates: the
  remaining 135 need reading one at a time, and an allowlist filled in without
  that reading would certify whatever was true the day somebody stopped looking.

### Added

- **Continuous integration.** `.github/workflows/verify.yml` runs `./verify.sh`
  on every pull request and every push to `main`, on Linux and macOS. The
  workflow installs every analysis tool the gate can use and then *fails* if any
  of them turned out to be missing — a gate that could not run must not report
  as one that passed. `--race` and `--fuzz` run nightly and on demand.
- **A statement-coverage gate.** `verify.sh` now measures how much of the tree
  the suite executes (`-coverpkg=./...`, so a package is credited for what any
  test reaches, not only its own) and fails below a floor of 78%. The profile is
  produced by the existing test run rather than a second one. A missing, empty,
  or unparseable profile fails loudly instead of reporting zero.
- **An end-to-end test of the shipped binary.** `manvi/cmd/manvi/zgap_endtoend_test.go`
  runs `manvi run` as a child process against a scripted OpenAI-compatible
  server over real HTTP, and asserts the file the model asked for appears with
  the right bytes, that the result is fed back for a second turn, that a write
  outside the repository root is refused as a hard rule, that the step ceiling
  exits 2, and that an unreachable model server fails non-zero rather than
  rounding to success. Deterministic and offline.
- **Specifications for the last fourteen tools.** `docs/TOOLS_REFERENCE.md` now
  specifies all 44 — tool discovery and activation, sub-agent management,
  artifacts, interactive questions, and MCP — where five categories were
  previously tabulated only. `TestToolsReferenceSpecifiesEveryTool` replaces the
  guard that policed the old disclaimer and now fails the build if a tool ships
  without a specification row.
- **A Windows compile job** in CI. It builds and vets the Go plane and builds
  the Rust plane on `windows-latest`, which is what covers the `_other` half of
  the `procgroup` and `mkfifo` build-tag splits — compiled by nothing else in
  the matrix. It does **not** run the Go suite: `verify.sh` is bash, and whether
  the tests that fork shells and drive the Rust binaries pass on Windows remains
  unestablished. Named as a gap rather than covered by a green check.
- **`CONTRIBUTING.md`** and this changelog.
- **Trade-off 3 in `docs/TRADE_OFFS.md`**: why the provider set is four adapters
  and why a hosted OpenAI-compatible endpoint is not a configuration change.

### Changed

- **Repository search runs on ripgrep's engine.** `dc-grep` joins the analysis
  plane, and `devcouncil_grep`, `devcouncil_find_files`, and `devcouncil_list_dir`
  now enumerate exactly the same file set through one walk and one set of ignore
  rules. A missing `dcgrep` refuses rather than falling back to a second walker
  that would answer the same question differently.
- **The Rust plane takes three dependencies**, declared by the members that need
  them: `rusqlite` (bundled) in `dc-store`, and `grep-regex`, `grep-searcher`,
  and `ignore` in `dc-grep`. `cargo audit` covers all of them on every run.
- **`verify.sh` gates the analysis tools themselves**, runs every fuzz target it
  declares with a per-target budget sized by the rate that target just measured,
  and reports a fuzz finding differently from a fuzz that could not run.

### Fixed

- Every analysis-plane subprocess now gets its own process group, so a killed
  parent cannot leave a child holding the terminal.
- An Anthropic tool call the harness cannot route is refused rather than
  silently dropped.
- The code graph is checked against the commit it was built from; a graph
  describing an older tree reports as unavailable instead of resolving no area
  for every file.
- The command gate's two shell scanners agree; splitting bypasses that one saw
  and the other did not are closed.
- An empty instrumented test run is no longer read as "nothing skipped".
- A `WaitGroup` is added to before it is waited on.

### Documentation

- `devmap` is no longer described as part of the Rust analysis plane. It is an
  external tool this repository does not build, resolved from `PATH` or
  `MANVI_MAP_BINARY`; `crates/` holds four members and `manvi/dc/devmap` is only
  the client. Corrected in `README.md`, `docs/ARCHITECTURE.md`,
  `docs/COMPARISON.md`, and the visual architecture guide.

---

## [0.0.2] — 2026-08-29

### Added

- **End-of-turn check.** The harness runs its own check when a turn ends,
  rather than trusting the turn's own account of itself.
- **Bounded documentation lookup.** `devcouncil_fetch_url` — off by default,
  registered only when an operator sets `MANVI_FETCH_HOSTS`, `https`-only, with
  private and link-local ranges refused on every request and every redirect hop.
- **A Cerebras arm in the benchmark rig**, with a wire suite covering it.

### Fixed

- Bare letters can no longer answer an approval card.
- A cross-arm protocol check could not see the arm it existed to flag.
- Contained Linux episodes were being stamped as uncontained.
- The runner's decode slots are counted, not the ceiling it was offered.

### Changed

- The benchmark paper's headline was replaced with the registered v2 grid, its
  figures regenerated from the v2 report, and the "three inert components" claim
  withdrawn as an upper bound rather than a measurement.

---

## [0.0.1] — 2026-08-29

First tagged release: the MANVI harness, dual-plane in Go and Rust with zero
cgo, the five-rung policy ladder and grants ledger, SQLite task leases across a
process boundary, the native tool suite, the full-screen TUI, multi-provider
support (Anthropic, Gemini, xAI, and local servers), the benchmark rig, and
`verify.sh` as the single gate over all of it.

[Unreleased]: https://github.com/bharathvbcr/Manvi/compare/v0.0.5...HEAD
[0.0.5]: https://github.com/bharathvbcr/Manvi/compare/v0.0.2...v0.0.5
[0.0.2]: https://github.com/bharathvbcr/Manvi/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/bharathvbcr/Manvi/releases/tag/v0.0.1
