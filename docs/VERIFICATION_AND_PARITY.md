# MANVI Verification & Parity Specification

This document details the cross-language testing methodology, parity fixtures, diff-coverage intersection engine, and rigor verification gates in **MANVI**.

---

## 1. Parity Fixture Methodology

When porting critical policy systems from Python or external codebases, the failure mode is not a crash, but a subtle rule divergence that passes all conventional unit tests.

To eliminate human interpretation drift, glob matching is verified against a corpus generated from CPython's `fnmatch`. Command policy is pinned to a frozen snapshot of the Go gate (the Python `devcouncil` package that used to generate it is deleted):

```mermaid
flowchart LR
    CPython["CPython 3.12 Standard Library"] --> GenGlob["scripts/gen-fnmatch-parity.py"]
    GenGlob --> GlobTSV["testdata/fnmatch-parity.tsv<br/>(775 cases)"]
    
    GlobTSV --> GoFnmatch["manvi/internal/fnmatch (Go)"]
    GlobTSV --> RustGlob["crates/dc-glob (Rust)"]

    Frozen["Frozen Go command-policy snapshot"] --> CmdTSV["testdata/command-parity.tsv<br/>256 cases"]
    
    CmdTSV --> GoPolicy["DevCouncil Go policy (Manvi links)"]
```

### Parity Invariant

- Go's standard `path.Match` stops at `/` separators. Python's `fnmatch` crosses `/` separators.
- Using standard Go `path.Match` would have silently weakened glob-based secret path rules without raising an error.
- Both Go and Rust must evaluate all 775 glob test cases identically, and `verify.sh` verifies fixture integrity before running tests.

---

## 2. Unified Diff Parsing & Rigor Gates (`crates/dc-verify`)

The Rust analysis plane parses unified diffs and executes rigor gates before any patch is accepted:

```mermaid
flowchart TD
    DiffInput["Raw Git Unified Diff Output"] --> Parser["dc-verify: parse_unified_diff"]
    Parser --> ASTChecks{"Syntax / Format Valid?"}
    
    ASTChecks -- Malformed --> ParseErr["Return Hard Error (Fail Closed)"]
    ASTChecks -- Valid --> RigorGates["Run Rigor Gates on Added Lines"]
    
    subgraph RigorGatesSuite["Rigor Gates Suite"]
        R1["1. Credential Scanner: Detect API keys & tokens in added lines"]
        R2["2. Stub & TODO Gate: Detect unimplemented!(), panic!(), TODO comments"]
        R3["3. Scope Classifier: Compare changed files against task planned scope"]
        R4["4. Coverage Intersector: Cross-reference added lines with coverprofile/LCOV"]
    end
    
    RigorGates --> RigorGatesSuite
    RigorGatesSuite --> Verdict{"All Gates Passed?"}
    
    Verdict -- Failed --> ReportFailures["Emit Detailed JSON Findings with Line Numbers"]
    Verdict -- Passed --> ReportPass["Emit Verified Status (Passed)"]
```

### Git Diff Security & Kept-Index Diff Acceleration

- **External Diff Driver Prevention**: All Git invocations that generate diffs or patches (`diff`, `show`, `log -p`, `blame`, `format-patch`, etc.) pass `--no-textconv --no-ext-diff`. This prevents a repository under test from triggering arbitrary executable code execution via `.gitattributes` `diff.<driver>.textconv` or external diff programs.
- **Kept-Index Scoped Diff**: To avoid quadratic full-tree walks on every mutating turn, `scopedDiff` seeds from the pre-turn baseline's kept index instead of re-walking the tree. It preserves racy-git timestamps (`mtime`) and accurately detects edits even when files are flagged with `assume-unchanged` or `skip-worktree` during the turn (matching the cold oracle), reducing diff calculation latency from 63 ms to 47 ms.

---

## 3. Test Coverage Intersection Pipeline

A passing verification claim in MANVI requires proof that newly introduced code was executed by the test suite.

```mermaid
flowchart LR
    Diff["Unified Diff (Added Lines)"] --> Intersect["crates/dc-verify:<br/>intersect_coverage"]
    CoverData["Coverage Map (Go -coverprofile or LCOV)"] --> Intersect
    
    Intersect --> Categorize{"Line Coverage State"}
    Categorize -- Executed in Test --> Covered["Covered (Hits > 0)"]
    Categorize -- Not Executed --> Uncovered["Uncovered Line Violation"]
    Categorize -- No Profile Provided --> Unmeasured["Unmeasured Warning (MANVI_COVERAGE missing)"]
```

### Semantic Distinctions

- **Uncovered vs Unmeasured**:
  - `uncovered`: The file was measured by a test runner, and the added lines were **not** executed. (Blocks verification in strict mode).
  - `unmeasured`: No coverage profile was supplied. Stated explicitly as an unmeasured environment fact, never laundered into a passing claim.
- **Fail-Closed on Corrupt Data**: An unreadable or malformed coverage report produces an explicit error, rather than defaulting to zero or full coverage.

---

## 4. The `./verify.sh` Master Gate

The `./verify.sh` script is the single authoritative gate that validates both planes before code lands.

```mermaid
flowchart TD
    Start["Run ./verify.sh"] --> G1["Go: gofmt format check"]
    G1 --> G2["Go: go vet static analysis"]
    G2 --> G3["Go: go test with real subprocesses"]
    G3 --> G4["Go: Cross-boundary test assertion (>= 5 real calls)"]
    G4 --> G5["Go: go mod tidy -diff, then the dependency surface for the cgo-off and the cgo graphs"]
    G5 --> G6["Go: golangci-lint enforced set — 20 linters at zero, plus ruleguard"]
    G6 --> G7["Go: golangci-lint debt ratchet — 11 linters, counts may only fall"]
    G7 --> G8["Go: govulncheck — reachable standard-library advisories"]
    G8 --> G9["Go: nilaway — cross-package nil analysis, held under a ceiling"]

    G9 --> R1["Rust: cargo fmt --check"]
    R1 --> R2["Rust: cargo clippy -D warnings"]
    R2 --> R2n["Rust: native/ broker — fmt, clippy, test"]
    R2n --> R2a["Rust: cargo audit --deny unsound — crates/ and native/ Cargo.lock against the RustSec database"]
    R2a --> R3["Rust: cargo test (unit & integration)"]
    
    R3 --> P1["Parity: fnmatch-parity.tsv >= 500 cases"]
    P1 --> P2["Parity: command-parity.tsv >= 200 cases"]
    
    P2 --> X1["Cross-Language: Rust dcstore schema & SQLite index validation"]
    X1 --> X2["Cross-Language: Python interop validation"]
    
    X2 --> V1["Verifier: Rigor credential & stub rejection"]
    V1 --> V2["Verifier: Coverage parsing & profile errors"]
    
    V2 --> M1["Brand: SVG asset parity check (assets/manvi-mark.svg)"]
    M1 --> T1["TUI: Raw terminal restore on exit check"]
    
    T1 --> Finish["PASS ALL GATES"]
```

### How to Run

```bash
# Complete verification run
./verify.sh

# Automatically fix Go and Rust formatting in place before running gates
./verify.sh --fix

# The Go suite again under the race detector, with cgo on
./verify.sh --race

# Every declared fuzz target actually executed, and proven to have executed
./verify.sh --fuzz
```

`--race` and `--fuzz` take one flag at a time; run the script twice for both.

Both are opt-in, for opposite reasons. `--race` is the whole Go suite again,
serialised, under the race runtime; `--fuzz` spends a budget
rather than answering a fixed question, so a green unattended run would mean
less each time the machine got busier.

`--fuzz`'s budget is per target and adaptive, because the targets are not
alike. Measured on one flat-30s run: the pure-function targets reached 13.6M
inputs while `FuzzShellDifferentialOracle` reached 29,142 and the three that
drive a real child process reached 40–55k — three to four orders of magnitude
behind, and they are the differential oracles, which is where most of the
defects this gate has found actually came from. A budget calibrated on the fast
targets starves exactly the ones worth running.

So each target runs for `MANVI_FUZZTIME` (default `20s`); one that has not
reached `MANVI_FUZZMIN` inputs (default `1000000`) by then gets the remainder of
`MANVI_FUZZMAX` (default `120s`). Two invocations at most, because each one
re-gathers baseline coverage over the whole corpus and looping in small rounds
would spend the extra budget on setup rather than on fuzzing; the corpus
persists in `GOCACHE`, so the second round resumes rather than restarts. No list
of which targets are slow is kept anywhere — that would be a second source of
truth about the targets, wrong the first time someone made a slow target fast —
the measurement decides.

A target that still cannot reach the floor with the whole budget is named under
`NOT COVERED` with its actual count. Equalising the budget does not change the
physics: a fork-per-case target will never reach millions of inputs. What the
gate can do is stop reporting a sampled target as an explored one.

All three knobs are a whole number of seconds (a trailing `s` is allowed, and
anything else is refused rather than passed through to `go test`, which would
accept `2m` and make the budget arithmetic quietly wrong).

`--fuzz` also
cannot trust `go test`: `go test -fuzz=X ./pkg` prints `no fuzz tests to fuzz`
and **exits 0** when `X` is not in `pkg`, which is how a sweep once recorded
three passes for targets it never ran. The proof required is the fuzzer's own
execution count, asserted positive for each target individually, with the total
carried alongside the per-target numbers.

A target that fails leaves its input under
`manvi/<pkg>/testdata/fuzz/<Target>/`; the gate names that path, and the input
is committed as a seed once the defect is fixed.

Workers are bounded (`MANVI_FUZZ_WORKERS`, default 4). `go test -fuzz` otherwise
starts one per CPU, and this step runs after the suite, the linters and nilaway
have already saturated the machine; unbounded, the engine reported `context
deadline exceeded` — its own coordination timing out, with no failing input
written. The gate tells that apart from a real finding by whether the corpus
directory grew, and reports the first as a gate that did not run rather than as
a defect.

### Tooling the gate needs

Four of the steps above call binaries this repository cannot vendor. A missing
one is recorded and reprinted next to the final verdict rather than scrolling
past mid-run, because a gate that could not run must not be indistinguishable
from a gate that ran and passed — the run then ends in `PASS with gates that
did not run`, naming each.

The versions are pinned in `scripts/tool-versions.env`, which `verify.sh`, the
workflows and `scripts/verify-lib.test.mjs` all read (see
[Pinned toolchain](#pinned-toolchain) below):

```bash
. scripts/tool-versions.env
go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$GOLANGCI_LINT_VERSION"
go install "golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION"
go install "go.uber.org/nilaway/cmd/nilaway@$NILAWAY_VERSION"
cargo install cargo-audit --locked --version "$CARGO_AUDIT_VERSION"
```

### Two build configurations, both certified

The default run is the `CGO_ENABLED=0` build, because "no cgo" is a claim the
architecture makes. Releases are the other configuration: they link
DevCouncil's Rust gusset engine in-process, with cgo on. The dependency
allowlist, both lint sets, `govulncheck` and `nilaway` therefore run twice, the
second time under DevCouncil's `rust/gusset-engine/cgo-env.sh` exports, the same
environment the `--race` leg and the engine step use. The cgo allowlist differs
from the cgo-off one by exactly `github.com/bharathvbcr/gusset`: its appearance
in the cgo-off graph still means a cgo file was compiled into that
configuration. Lint counts are compared as the larger of the two runs against
one recorded number per linter.

### A check that could not run is a failure

The helpers in `scripts/verify-lib.sh` give the steps above one shape: a tool
that exited abnormally, produced nothing, or reported its own failure to load a
package is an error and never a count. A `golangci-lint` report holding
`typecheck` rows, a `nilaway` run that printed `analysis skipped due to errors
in package`, a `go list` that failed and a `go test` whose subtest failed after
sixty others passed were each once read as a pass. `scripts/verify-lib.test.mjs`
runs every helper against stub binaries that behave that way, and scans
`verify.sh` for a `| grep -c … || true` over a test or analysis command.

The suite runs once, as `go test -json`, and the skip report and the
cross-boundary counts read that run's events rather than running it again.

In CI, `scripts/check-verify-log.sh` runs after `verify.sh` in every job that
runs it, including the nightly `--race` and `--fuzz` jobs, and fails the job on
a missing analysis tool or on any NOT COVERED line or skipped test that
`scripts/verify-expected-gaps.json` does not list for that runner and mode.

### Windows

`verify.sh` is bash and drives Unix tools, so it does not run on Windows. Two
things cover the Go plane there. `GOOS=windows go vet ./...` is a verify step and
compiles every test file, so a Unix-only call cannot enter one. The
`windows-latest` job in `verify.yml` builds the cgo-off plane and runs
`go test ./...`; that job's first result is the first evidence about the
Windows port, and no run of it has been recorded here.

### Pinned toolchain

CI installs exactly these, from `scripts/tool-versions.env`. `release.yml` pins
the same values.

| Tool | Pinned version |
| --- | --- |
| golangci-lint | `v2.14.0` |
| govulncheck | `v1.8.0` |
| nilaway | `v0.0.0-20260918162853-acb8859b9031` |
| cargo-audit | `0.22.2` |
| Rust toolchain | `1.99.0` (components clippy, rustfmt) |
| node | `22.22.2` |
| Go | `go.mod`'s `go 1.26.6`, through `go-version-file` |

Each is the version that was resolving when it was pinned (2026-10-10). A
floating reference made the debt ratchet and the `nilaway` ceiling a measurement
of whichever release a run fetched. Move a pin in a change that also re-measures
`manvi/.golangci-debt.counts` and `nilaway_max` in `verify.sh`.

### Why the lint gate is two files

`manvi/.golangci.yml` holds the linters that are clean on the whole tree, so its
verdict is a number and the number is zero. `manvi/.golangci-debt.yml` holds the
checks worth having that this tree does not pass yet: 1059 findings, too many to
gate on and too many to leave unnamed. Their counts are recorded per linter in
`manvi/.golangci-debt.counts` and may only go down. Per linter rather than as a
sum, because one total lets a fix pay for a new defect.

Those counts are larger than any default `golangci-lint` run reports. Its own
summary said **197** for this same tree. `max-issues-per-linter` stops at 50,
`max-same-issues` at 3, and `uniq-by-line` keeps one finding per line — three
caps, all silent, withholding 850 findings behind a line that reads like a
total. Both config files turn all three off.

`manvi/gorules/rules.go` carries this repository's own invariants as ruleguard
patterns, loaded by the enforced set: the credential that must not reach `fmt`,
the verdict that must not be set from a literal, the peer-controlled stream that
must be bounded, the skip that must say what it is gating on.

---

## 5. Related Documentation

- [Documentation Index](README.md)
- [Why MANVI is Different (Comparison)](COMPARISON.md)
- [Technical Architecture Specification](ARCHITECTURE.md)
- [Hardening Ledger & Defects](HARDENING_LEDGER.md)
- [Architectural Trade-offs](TRADE_OFFS.md)

