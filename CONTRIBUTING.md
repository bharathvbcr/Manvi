# Contributing to MANVI

There is one gate, and it is `./verify.sh`. Everything below is either how to
make that command pass on your machine or what it will refuse.

---

## 1. What you need installed

Two toolchains, because two invariants have one owner each — the reasoning is
in [TRADE_OFFS.md](docs/TRADE_OFFS.md#2-two-toolchains-because-two-invariants-have-one-owner-each).

**Required.** Without these, the gate cannot run at all:

| Requirement | Why |
|---|---|
| **Go 1.26.6** (`manvi/go.mod`) | The execution plane. Built and tested with `CGO_ENABLED=0`, which the gate exports for you. |
| **Rust, stable** | The analysis plane. `crates/Cargo.toml` declares edition 2024 with resolver 3, so 1.85 or newer. |
| **A C compiler (`cc`)** | `dc-store` takes `rusqlite` with the `bundled` feature and compiles SQLite from source, rather than linking whatever `libsqlite3` the host ships. |
| **`python3`** | Runs the benchmark rig's eight suites and parses the test run's skip report inside `verify.sh`. |
| **`git`** | Several tests build a repository to run against. |

**Optional, and the gate says so when they are absent.** Each of these is a
check that will report *NOT COVERED* rather than pass:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
go install golang.org/x/vuln/cmd/govulncheck@latest
go install go.uber.org/nilaway/cmd/nilaway@latest
cargo install cargo-audit
```

`node` and `npm` (the mermaid grammar gate), `sqlite3` (schema readability), and
`script(1)` (TUI terminal restoration) are picked up if present.

**DevCouncil's components.** MANVI links none of them and builds only one —
`devmap` comes from DevCouncil and is not built here. On a fresh clone, without
`devmap` on `PATH`, the repo-navigation gate reports as *not covered*, which is
expected. `crates/` mirrors DevCouncil's other three component sources so the
gate can run without a DevCouncil checkout; at runtime the harness prefers an
installed component over that mirror. `manvi doctor` prints which binary each
component resolved to and whether it answered — read it before assuming which
one you are testing, because an installed component can be older than the
contract the harness expects, and `doctor` is where that shows up.

---

## 2. Running the gate

```bash
./verify.sh
```

Three opt-in modes, one flag at a time — run it again for the other:

| Command | What it adds |
|---|---|
| `./verify.sh --fix` | Rewrites formatting in place first, then runs everything. |
| `./verify.sh --race` | The Go suite again under the race detector, with cgo on. |
| `./verify.sh --fuzz` | Every declared fuzz target, actually executed, with a per-target budget. |

`--race` and `--fuzz` are separate from the default run for opposite reasons:
the race detector needs cgo, which the shipped configuration turns off, and the
fuzz sweep spends a budget rather than answering a fixed question.

### Reading the verdict

The last line is one of two, and the difference matters more than the colour:

- **`PASS all gates`** — every check ran and every check passed.
- **`PASS with gates that did not run:`** — followed by the list. These are
  *not* failures, and they are *not* passes either. A run that ends this way is
  telling you which questions nobody asked.

Some entries are permanent and expected on any machine: the hosted provider
adapters are verified against scripted servers only, `devmap` is not built here,
and no local model server is listening. An entry containing **"is not
installed"** is different — that is a tool from the list above that you can
provide, and CI treats it as a provisioning failure rather than a result.

Nothing in this repository may report a check that could not run the same way it
reports a check that ran and passed. If you find a place that does, that is a
bug worth its own commit.

---

## 3. What the gate will refuse

These are enforced mechanically, so it is cheaper to know them before the build
goes red than after:

- **A skipped test.** `MANVI_TEST_ALLOW_SKIP` is refused outright, and every
  skip that survives is named in the output. A package whose tests all skip
  still prints `ok`; the gate counts what actually ran.
- **Statement coverage below 78%** across the tree, measured with
  `-coverpkg=./...` so a package is credited for what any test reaches. The
  weakest package is named beside the total.
- **A new Go dependency.** The Go plane's three direct dependencies are an
  allowlist in `verify.sh`, and a fourth is a build failure rather than something
  noticed in review. The Rust members declare their own; nothing is hoisted to
  the workspace.
- **A tool without documentation.** Add a native tool and
  `TestToolsReferenceSpecifiesEveryTool` fails until
  [docs/TOOLS_REFERENCE.md](docs/TOOLS_REFERENCE.md) has a specification row for
  it, and `TestDocumentedToolCountMatchesRegistry` fails until every count in
  the prose agrees with the registry.
- **A mermaid diagram that does not parse** with the real grammar.
- **A fuzz target that reports zero executions** under `--fuzz`. A target the
  runner cannot find exits 0 and looks like a pass; the gate reads the counts.
- **`cgo`.** `CGO_ENABLED=0` is a claim this repository makes in its
  architecture and its README, so the default run is the shipped configuration.

---

## 4. Parity fixtures

Two behaviours are pinned to a reference implementation rather than to our own
opinion of them, because Go, Rust, and CPython each have to agree. Both
generators write to **stdout**; the committed TSV is the redirect target.

```bash
# 775 cases, from CPython's own fnmatch — needs nothing but python3.
python3 scripts/gen-fnmatch-parity.py > testdata/fnmatch-parity.tsv

# 256 cases, from the incumbent DevCouncil TaskPolicyEngine — needs that
# checkout importable, which is why this one is not reproducible from this
# repository alone.
DEVCOUNCIL_SRC=../DevCouncil/src python3 scripts/gen-command-parity.py \
    > testdata/command-parity.tsv
```

`testdata/command-parity.tsv` carries three rows that diverge from the
incumbent, applied by hand after generation and named in the file's own header.
Regenerating drops them; re-apply them, or the port starts matching a behaviour
this harness decided against.

Regenerate either fixture only when the reference behaviour itself is what
changed, and say so in the commit — a regenerated fixture that quietly absorbs
a divergence is the fixture no longer doing its job. The methodology is in
[VERIFICATION_AND_PARITY.md](docs/VERIFICATION_AND_PARITY.md).

---

## 5. Which repository does this change belong in?

Ask what the change is *about*, not which checkout is open. MANVI is the
unification layer; **DevCouncil owns the components**, and DevCouncil is
upstream for all of them.

| The change is about… | It belongs in |
|---|---|
| Diff parsing, what counts as a stub, coverage intersection | DevCouncil — `dcverify` |
| The code graph: extraction, resolution, dead code, impact | DevCouncil — `devmap` |
| The task schema, the lease, mutual exclusion | DevCouncil — `dcstore` |
| Tree walking, ignore rules, search semantics | DevCouncil — `dcgrep` |
| Driving a turn, cancelling a stream, dispatching a tool | MANVI |
| Whether a write is allowed; how a grant is recorded | MANVI — `gate`, `policy`, `grants` |
| Rendering, logging, replay | MANVI — `ui`, `session` |
| What the harness *asks* a component for | MANVI — `manvi/dc/…` |

The last row is the one that goes wrong. `manvi/dc/store`, `manvi/dc/devmap` and
`manvi/dc/dcgrep` are **clients**: they transport answers, they do not compute
them. A client that starts deciding whether a lease is valid — or caching one —
is reimplementing a component badly and on the wrong side of the boundary. A
cached lease is a lease that has already expired somewhere else.

**MANVI links no component.** Not by `cgo`, not by vendoring a crate, not by
importing a module. Every component is resolved as a binary
(`MANVI_<NAME>_BINARY` → `PATH` → a local build) and spoken to in JSON over
stdio. That rule is what keeps the harness a single static binary that another
application can embed, so it does not bend for convenience.

`crates/` holds a **mirror** of DevCouncil's component sources so MANVI can be
built and tested without a DevCouncil checkout. Edit the component in DevCouncil
and mirror it here; a change made only here forks silently, because nothing in
either build will notice. See
[`docs/COMPONENTS_AND_HARNESS.md`](docs/COMPONENTS_AND_HARNESS.md) §7.

**Changing a component's contract** — a field name, a new key, a changed default
— breaks every consumer, not only MANVI. Version the payload (`dcverify` carries
`schema_version`; `devmap` a store `user_version`), fail closed on an unknown
version rather than guessing, and run MANVI's live-contract tests against the
new binary. Those tests are the only thing that will notice.

---

## 6. Changes, tests, and commits

**Every fix ships with a test that fails against the pre-fix code.** Write the
failing test first and watch it fail; a test written after the fix proves only
that the code does what it does. Tests live in the package they guard. Some
files carry `zfix_`/`zgap_` prefixes — that convention is not written down
anywhere, so match what the package around you does rather than reading meaning
into it.

**Commit messages** are imperative, sentence case, no prefix, no trailing
period, and they say what changed and why:

```
Stop a change nothing scanned from reporting as verified
Give every analysis-plane subprocess its own process group
Add to the WaitGroup before waiting on it
```

The body is where the reasoning goes, and this repository would rather have too
much of it than too little — the comments in `verify.sh` and the
[hardening ledger](docs/HARDENING_LEDGER.md) are what a commit body grows into.

**Add a [changelog](CHANGELOG.md) entry** under **Unreleased**, in the same
commit as the change. Entries written afterwards are reconstructions.

**A defect worth remembering goes in the
[hardening ledger](docs/HARDENING_LEDGER.md)**, with its root cause and the
location of the regression test that now holds it. The ledger is organised by
defect pattern; the changelog is organised by release. Most changes need only
the second.

---

## 7. Continuous integration

[`.github/workflows/verify.yml`](.github/workflows/verify.yml) runs `./verify.sh`
on every pull request and every push to `main`, on Linux and macOS. It installs
all four optional analysis tools first and then fails the run if any of them
turned out to be missing, because a CI job that quietly skipped the linter and
the vulnerability scan prints the same green check as one that ran them.

A third job compiles the Go and Rust planes on `windows-latest`. That is all it
claims: `verify.sh` is bash and does not run there, and the Go suite is not run
there either — many of its tests fork shells and drive the Rust binaries across
a process boundary, and nothing has established that they pass on Windows. The
job exists because `procgroup_other.go` and `mkfifo_other.go` are compiled by
nothing else in the matrix, so a break in them would otherwise reach a user
before it reached CI. Windows *testing* is an open gap, named rather than
covered.

`--race` and `--fuzz` run nightly, and on demand through **Run workflow**.

Every run uploads its full `verify.log` as an artifact and puts the
gates-that-did-not-run list in the job summary, so the honest half of the
verdict is visible without opening the log.
