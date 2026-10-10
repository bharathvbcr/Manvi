# DevCouncil — Known Gaps and Defects

> **Status (2026-09):** DevCouncil's Python package (`src/devcouncil/`) has been
> deleted. Gaps that assume that tree is the live engine are historical. Do not
> install or `import devcouncil` as a Python package.

Everything known to be wrong, missing, or unproven, gathered so it is addressed
**during** the port rather than faithfully reproduced by it.

Companion to [`PORTING_TASKS.md`](PORTING_TASKS.md). Where a gap belongs to a
work package, its ID is named.

## How to read this

Gathered 2026-09-01 from `DevCouncil/docs/devmap/STATUS.md`, `DevCouncil/docs/devmap/INTEGRITY.md`,
`DevCouncil/docs/devmap/DIVERGENCES.md`, `DevCouncil/docs/devmap/CONSUMERS.md`, `IMPROVEMENTS.md`, the CI
workflows, and direct inspection of the source.

Every entry is labelled:

- **VERIFIED** — a command was run or the source read, in this session.
- **RECORDED** — a ledger says so and I did not re-check it. Treat as a lead.
- **STALE** — a ledger says it is open and the code says otherwise.

> **The ledgers disagree with the code, in both directions.** Five claims checked
> this session were wrong: two defects recorded as open are fixed (§3), one
> "29 of 35 languages fail to parse" is long superseded, one coverage floor moved
> from 18 to 90, and one file-difference count was one when it is two. **Read the
> code before acting on any RECORDED item**, and when they disagree, the code
> wins and the ledger gets fixed.

---

## 1. Verified open

Highest confidence. Each was checked in this session.

### GAP-1 — ~26 of 35 languages have no call graph *(the big one)*

> **Re-verified 2026-10-06 — mostly STALE; the residue is below.** Read this
> box before the 2026-09-01 text under it, which is kept as the record.
>
> - **Call extraction landed (P2.7b/c).** `rust/devmap-extract/src/langcalls/`
>   has a module per language, dispatched from `langcalls::extract_calls`:
>   C#, Dart, Erlang, Java, Kotlin, Lua/Luau, Nix, Pascal, PHP, R, Ruby,
>   Scala, shell, Solidity, SQL, Swift. Python, JS/TS, Go, Rust and the C
>   family keep their own arms. The per-language control is
>   `rust/testdata/capabilities/` plus
>   `devmap-extract/tests/language_capabilities.rs`, which pins every
>   language's `CALLS` bit to observed extraction in both directions (9/9
>   pass). Measured end to end with a two-method control per language
>   (`entry` calls `helper`, `orphan` called by nothing): Java, C#, PHP,
>   Swift, Ruby (`helper()`) and Python each produce exactly one `Calls`
>   edge, and `dead` flags `orphan` but not `helper`.
> - **Still call-blind, by name** (`the_call_blind_clean_parsing_languages_are_named`):
>   `vb` (no grammar, GAP-7), `cobol` (refused by `UNSAFE_GRAMMARS`),
>   `cfml` and `hcl` (parse clean, no call extractor — P2.7d's "not
>   applicable, reported unavailable"). Non-registry rows without `CALLS`:
>   protobuf and PowerShell (`NONE`), HTML and CSS (`REFERENCES` only), and
>   the prose/data formats.
> - **P2.7a was half done, now closed.** `dead` already priced call-blind
>   files with `CALL_BLIND_REASON`. `impact`, `trace`, `impact --layers`,
>   `neighbors`, `explore` and scoped `trace A B` did not: `impact` on an
>   HCL symbol answered `total: 0, Available, walk_incomplete: None`. Fixed
>   in DevCouncil `devmap-query` `traverse_walked` / `trace_between`, from
>   the same predicate `dead` uses (`devmap_extract::model::is_call_blind`);
>   pinned by `devmap-query/tests/a_call_blind_target_is_unavailable_not_empty.rs`.
> - **Ruby bare send — closed 2026-10-06, residue below.** `def entry;
>   helper; end` parses as a bare `identifier`, so `impact` on `helper`
>   answered `total: 0, Available, walk_incomplete: None`. DevCouncil
>   `langcalls/ruby.rs` (`extract_bare_sends`) now claims it under the
>   conservative half of Ruby's lexical rule: an identifier in a
>   positively-listed value slot is a send only when **no** binding of the
>   name exists anywhere in its `def`/`class`/`module`/file scope
>   (parameters of every kind, block params and locals, rescue/for
>   variables, single/multiple/operator assignment, every pattern binding
>   including `in {k:}`, regex named captures). Also refused: `defined?(x)`,
>   `_1`–`_9`, `it` in a block, superclass and `class << x` expressions, any
>   scope with a parse error, and any name an enclosing class defines without
>   `def` (`attr_*`, `define_method`, `alias(_method)`, delegators,
>   `Struct.new(:x)` superclass). Measured: control corpus 0 → 1 caller
>   (`paren_helper` unchanged at 1, same-named local read stays 0); Homebrew
>   (476 `.rb` files) `Calls` 3,244 → 4,116, 0 lost. Pinned by
>   `devmap-extract/tests/langcalls_ruby_php.rs` and
>   `devmap-query/tests/a_bare_ruby_send_has_a_caller.rs`.
> - **Residue, open — Ruby methods defined without `def` are not symbols.**
>   Bare sends are how attribute readers are called, so they fed the
>   resolver's `UniqueGlobal` rung names with no declaration: 26 of the first
>   126 new cross-class 0.9 edges had an `attr_*` of the callee's name in the
>   caller's own file (0 of the 807 pre-existing such edges did). The
>   enclosing-class refusal removes 20; still wrong are attributes inherited
>   from a superclass (6 on Homebrew, all `AbstractCaskErrorWithToken#reason`
>   → `Denylist.reason`), mixed in from a module, splatted delegators, and
>   `method_missing`. Of the 105 remaining new `UniqueGlobal` edges, 36 follow
>   a recorded `Extends` chain; the other 69 mix mixin/delegation-correct
>   targets with wrong namesakes and are not classified. The owner is the
>   declaration emitter (`langdecl`): `attr_*` declares a method. 872 new
>   edges split 523 `SameFile` same-class, 244 `AmbiguousGlobal` (0.2), 105
>   `UniqueGlobal` (0.9).
> - **Scala's `def entry = helper` is the same shape but not the same fix.**
>   tree-sitter-scala gives `helper` (a `def`) and `field` (a `val`) the same
>   `identifier` body, and both are template members, not method-scope
>   locals — so the lexical-local rule cannot tell a call from a field read.
>   Distinguishing them needs the enclosing template's member table
>   (resolution, not extraction); `langcalls/scala.rs` keeps refusing it.

**VERIFIED** (`DevCouncil/docs/devmap/STATUS.md` SC34, measured 2026-08-17 with a
per-language control). `treesitter.rs` has **9 `calls.push` sites, all inside
language-specific arms**. Every other language falls through to a generic arm
that emits declarations only. **Java, C#, Ruby, Swift and PHP each produce
`Contains` edges and zero `Calls`** while parsing cleanly and emitting symbols.
Only Python, JS/TS, Go, Rust and the C family have call extraction.

**Why it matters more than it looks:** `impact`, `trace`, dead-code and the PDG
return answers for those languages built on an **empty call graph**, with no
signal distinguishing "no callers" from "callers were never extracted." Dead-code
is the dangerous surface — a symbol with no extracted calls is indistinguishable
from an unused one. This is caution **C1** at corpus scale: a check that did not
run reporting what a check that ran and passed reports.

**Fix direction:** a call-extraction arm per language family, each measured
against a control the way SC19 and SC31 were. **Until then, dead-code output for
those languages must be marked unavailable rather than emitted.**

→ **[P2.7](PORTING_TASKS.md#p27--close-the-call-graph-blackout-gap-1-the-largest-known-defect)**,
split into P2.7a (make the blackout visible — small and urgent) and P2.7b–d
(close it, tiered by evidence). **P2.4 is blocked on P2.7a**, because
code-intelligence handlers must not expose a confident zero.

Verified in source this session: `extract_node`'s `match lang`
(`devmap-extract/src/treesitter.rs:1578`) has exactly four language arms —
`python`, `javascript | typescript | tsx`, `rust`, `go` — and everything else
falls to a generic arm emitting declarations only. The C family is served by a
separate `extract_c_family_call`, which is the template for the fix.

### GAP-2 to GAP-6 — re-checked 2026-10-10, now closed or narrowed

GAP-2 (`dcstore` table coverage), GAP-3 (no Go MCP server), GAP-4 (substring
stub detection), GAP-5 (no drift check) and GAP-6 (no Go component home) were
all contradicted by the code at the pinned DevCouncil commit and moved to
[§3](#3-recorded-open-but-actually-closed--do-not-spend-time-here), each with
its file:line evidence. The one residue, GAP-2's unreferenced tables, is the
single open row there and is tracked on this repository's board as `ft-46ab2b`.

---

## 2. Recorded open — moved to DevCouncil

GAP-7 to GAP-13 and GAP-21 were copied here from `DevCouncil/docs/devmap/STATUS.md`
and `INTEGRITY.md`. DevCouncil archived both as historical on 2026-09-11
(`DevCouncil/docs/archive/README.md`, files under `docs/archive/devmap/`), so
this file no longer keeps a copy. They were re-checked against DevCouncil source
on 2026-10-10; the outcome of each is in the table. The ones that survived are
filed on DevCouncil's board; a gap that DevCouncil's ledgers no longer carry is
DevCouncil's to track, not this file's.

| ID | Outcome (2026-10-10) | Evidence | Owner now |
|---|---|---|---|
| GAP-7 | **Verified open.** VB.NET has no grammar; declarations are recovered by pattern only. | `rust/devmap-extract/src/cache.rs` has a test asserting `grammar_version_for("vb")` starts with `unavailable:`; `fallback.rs` lists `vb` among pattern-recovered languages | DevCouncil board (grouped task `dc-devmap-recorded-gaps-reverify`) |
| GAP-8 | **Unverified.** Needs a measurement run on DevCouncil's corpus (741 duplicate identities was a 2026-08 figure), not a code read. | — | DevCouncil board (same task) |
| GAP-9 | **Unverified.** Needs a memory measurement run. | — | DevCouncil board (same task) |
| GAP-10 | **Partly closed, cost unmeasured.** `--affected` now narrows what a generation *writes* (the store carries unaffected edges forward) while resolution stays global by design (`devmap-cli/src/commands/build.rs`, the "B3/SC2" comment above the affected-closure step). Whether extraction still reads the whole tree needs a timing run: one-file `--affected` against a full build. | `build.rs`, "B3/SC2" comment | DevCouncil board (same task) |
| GAP-11, GAP-12, GAP-13 | **Unverified.** Property-suite coverage, mutation coverage and the freshness soak are measurement runs. | — | DevCouncil board (same task) |
| GAP-14 | **Superseded.** It listed Python consumers (`orphan_diff.py`, `semantic_diff.py`, ...) of a Python tree that has been deleted. | `PHASE7_LONG_TAIL.md` retirement table | — |
| GAP-15 | **Superseded.** The ~42 mypy errors were in the deleted Python package. | `PHASE7_LONG_TAIL.md` | — |
| GAP-21 | **Mostly closed; two leftovers.** `regex`/`rayon`/`tracing`/`ignore` declared under `[dependencies]` in `devmap-*` each have a `src` use (rg, 2026-10-10) and `thiserror` is no longer declared. Left: `devmap-cli` lists `rayon` under `[dev-dependencies]` (`Cargo.toml:51`) with no `rayon::` use in `src/` or `tests/` (only the `RAYON_NUM_THREADS` env string in `tests/state_discovery_contract.rs`; `cargo udeps` not run), and `devmap-extract/src/treesitter.rs` still matches `"async_function_definition"` in six places; tree-sitter-python's grammar has no such node type, so the arm is probably dead for Python (other grammars not checked). Whether CLI `manifest` uses its `path` argument was not checked. | `treesitter.rs`, `devmap-cli/src/cli.rs` | DevCouncil board (same task) |

---

## 3. Recorded open but actually closed — do not spend time here

**STALE.** `DevCouncil/docs/devmap/INTEGRITY.md`'s snapshot notice says these still need work.
The code says otherwise. Verified this session; the notice should be corrected.

| Ledger claim | Reality |
|---|---|
| **D10** — "still needs durable repository-root resolution" | **Closed.** `devmap-query/src/engine.rs` has `latest_repo_root()`, `resolve_source_path()` and `source_unavailable_reason`. |
| **D17** — "still needs a persisted unresolved-call ledger" | **Closed.** `Resolution::Unresolved` is constructed in `devmap-resolve/src/resolver.rs` and `devmap-analyze/src/liveness.rs`. SC30 further split the tier into four evidence-backed classes. |
| **T1–T9 vacuous tests** — "tests that cannot fail" | **Strengthened.** Spot-checked two: T1 now asserts `!paths2.contains("b.py")` with the message "deleted file must not remain as live nodes"; T3's `if let Some(edge)` pattern is gone from the test tree. |
| INTEGRITY: "29 of 35 languages return `ParseOutcome::Failed`" | **Superseded.** 34 of 35 reach a real grammar (only VB.NET does not). Note this is *parsing* — GAP-1 is about *call extraction*, which is a different and still-open question. |
| IMPROVEMENTS: `fail_under = 18` | **Superseded.** `pyproject.toml` now sets `fail_under = 90`. |
| `rust/STATUS.md`: "one file differs from MANVI's copy" | **Corrected in commit `a48c423`.** Two do. |
| **GAP-2** — "`dcstore` covers 2 of 16 tables" | **Stale.** `crates/dc-store/src/schema.rs` (DevCouncil `rust/dc-store/src/schema.rs`) creates 18 tables, `schema_version` included, and the workbench adds 19 `work_*` tables (`rust/dc-store/src/workbench/*.sql`). **Residue, open:** six of the 18 (`project_state`, `critique_findings`, `shell_sessions`, `shell_command_events`, `file_change_events`, `semantic_diffs`) are named by no source outside `schema.rs` and tests, and `correction_manifests` appears only in a comment (`devcouncil/correction/manifest.go:3`) and is never inserted. Tracked as `ft-46ab2b`. |
| **GAP-3** — "all 29 `devcouncil_*` handlers still Python" | **Stale.** The Go host serves MCP (`backend/go_orchestrator/cmd/devcouncil/main.go:70`, `"mcp"`) with 8 tools (`devcouncil/registry.go`). The thick surface is Manvi's native registry (40 `devcouncil_*` tools listed in `TOOLS_REFERENCE.md`); DevCouncil `docs/TODO.md` records this as `GAP-P7-MCP-HOST-THIN`. |
| **GAP-4** — "`dc-verify` stub detection is substring matching" | **Stale.** AST detection landed in DevCouncil `c6b48a28` and Java/C# in `9b0b9e5c`, both ancestors of the pin. `rust/dc-verify/src/stub_ast.rs:1-27`; the `allow-stub` marker is `rigor.rs` `ALLOW_STUB_MARKER`; `tests/false_accept.rs` and `tests/stub_ast.rs` are the gates. Assert-free and skipped-test checks cover Rust, Go, Python and TypeScript, not Java or C#; a file that does not parse falls back to the substring gate, labelled `Derived`. |
| **GAP-5** — "nothing detects drift between the two source copies" | **Superseded.** `crates/dc-*` are symlinks into `../../DevCouncil/rust/` (`ls -la crates`), so there is one copy. `scripts/check-release.mjs` `crateMirrorProblems` checks inherited manifest values at the pin in `scripts/module-pins.txt`, and `manvi/devcouncil/crate_pins_test.go` checks resolved dependencies. |
| **GAP-6** — "no Go component home in DevCouncil" | **Stale.** Go components live in `DevCouncil/backend/go_orchestrator/` (`gate/`, `policy/`, `grants/`, `devcouncil/{gating,verify,stopgate,correction,mcp,skills}`, `dc/`). `DevCouncil/go/` does not exist. |

**Also closed:** the entire `IMPROVEMENTS.md` P0–P3 backlog (#1–#20) — git
subprocess timeouts, atomic writes, LLM retry, swallowed exceptions, test
coverage, the god-module split, the import cycle, JSON persistence, AST caching.
It is marked **FULLY CLOSED** and the suite was green at 1385 passed / 0 failed.
Do not re-file these.

---

## 4. Python baseline defects the port must **not** reproduce

**RECORDED** from `DevCouncil/docs/devmap/DIVERGENCES.md`, which exists precisely because the
Rust port deliberately behaves *differently* from the Python. A porter reading
the Python as a specification will faithfully reproduce these bugs.

**Read `DIVERGENCES.md` before porting any subsystem it touches.**

| ID | Python behaviour — the defect | What correct looks like |
|---|---|---|
| G4 / G26 | Nondeterministic hash-map iteration | Deterministic output (`BTreeMap`/`BTreeSet`, sorted emission) — byte-identical artifacts across builds |
| G5 | A multi-candidate pick is emitted as `Extracted` confidence | A multi-candidate pick must **never** claim `Extracted`. Confidence honesty. |
| G6 | Import-scoped resolution silently widens | Strict import-scoped resolution; no false-positive edges |
| G7 | Ambiguous inheritance guesses a single path | Fan out, or abstain with `Unresolved` |
| X14 | An empty list returned for unknown dependents | Structured zero-count **unavailable** semantics — unknown ≠ verified-zero. This is **C1/C4** in the Python. |
| X16 | Arbitrary 500-symbol cap on snapshots | Budgeted truncation with shown/total flags |
| V12 | Dead list hides the total count | Budgeted, with `shown + hidden = total` |
| V14 | Artifact writes without atomic rename | tmp + rename with fingerprint skip |
| B3 | Full rewrite on small edits | Differential write with `affected_paths` |
| X1 | Misses TS `enum` / `namespace` / `declare` / `abstract` / overloads | Parsed cleanly |
| X2 | Drops `new_expression` and Go `composite_literal` calls | Extracted |
| X3 | Ignores `export *` re-exports | Re-export chain sentinel emitted |
| X9 | Drops grouped Rust `use {a, b::c}` | Expanded |
| X30 | Solidity functions emitted flat (`Contract.sol::get`), losing the owner | Owned by their contract |
| X31 | Exported module-level bindings misclassified as kind `function` | Emitted as `Variable` |
| X32 | HCL/Terraform blocks not addressable | Emitted under their Terraform address |
| G3 | Python stdlib names resolve globally, polluting other languages | Guard restricted to the Python family |
| G8 | Fixed traversal depth | Parametric depth |
| G19 | Generic `impl` blocks fail resolution | Handled |
| T1 | Unbounded manifest | ≤2k-token manifest with a subsystem cap |
| X6 / X7 | **Regex recovery fabricates symbols from syntactically invalid source** | Tree-sitter error trees stay `Partial`; regex recovery is never promoted to authoritative extraction |
| X18 | Regex-based public-symbol rules | Per-language public rules in the snapshot builder |
| G1 | CFG entry/exit threading missing | Explicit entry/exit control-flow nodes |
| G2 | Leader-line attribution inaccurate | Blocks own their leader lines explicitly |
| G10 | PDG batch reads unoptimised | Extraction cache enables shard re-read |
| N7 | RRF / vector ranking | FTS + lexical rank only (RRF deliberately deferred, not a defect) |

That is all 28 rows of `DIVERGENCES.md`. **X6/X7 deserves special attention**: the
Python invents symbols out of source it could not parse — the same
confident-answer-without-evidence failure as G5 and X14, except it manufactures
data rather than merely overstating it.

The pattern across G5, G7, X14, V12 and X6/X7 is one defect wearing four coats: **the
Python answers confidently where it does not know.** That is the single most
important thing not to carry across.

---

## 5. Cross-cutting and process gaps

| ID | Gap | Status | Task |
|---|---|---|---|
| GAP-16 | **Nothing in `src/devcouncil/` calls the ported components.** | SUPERSEDED (2026-09) — that Python tree is deleted; live engines are `devmap` and Go/Rust components | All of Wave 2 |
| GAP-17 | **No differential measurement** of Rust vs Python gates over real diffs. | SUPERSEDED (2026-10) — the Python tree is deleted, so there is nothing left to diff against. The premise that Python leads on stub detection is false (GAP-4, §3): the AST detector is in `dc-verify`. | P4.1 (done, see `PORTING_TASKS.md`) |
| GAP-18 | **Linux and Windows coverage is thin.** Everything before 2026-09-01 was verified on darwin/arm64 only. `.github/workflows/verify.yml` runs the `verify.sh` gate on `[ubuntu-latest, macos-latest]` and has no Windows job; its own comment says why (this tree does not build for `GOOS=windows`). Whether to add a Windows leg is `ft-7fb7af56`. | VERIFIED 2026-10-10 against `verify.yml` | P0.2 |
| GAP-19 | **`MANVI_*` naming leaked into DevCouncil components.** `testsupport.AllowSkipEnv` was `MANVI_TEST_ALLOW_SKIP`; the build lock was `.manvi-testbin.lock` beside copies in `target/manvi-testbin/`. Decided and renamed in both repositories together: `DEVCOUNCIL_TEST_ALLOW_SKIP`, `.devcouncil-testbin.lock`, `target/devcouncil-testbin/`. No deprecation window in which the old name still opts a run into skipping — that would let a stale config keep seams uncovered. Instead the old name is a tombstone: `testsupport.Unavailable` fails naming its replacement whenever it is set, and `verify.sh` refuses both names. No CI workflow in either repository referenced the old names (`rg -uu` over both trees). | CLOSED (2026-10) — pinned by `TestTheOnDiskAndEnvironmentNamesAreDevCouncils` and `TestTheRetiredOptInFailsNamingItsReplacement` in DevCouncil `testsupport` | P0.3 |
| GAP-20 | **`dc-glob` is duplicated by design** — Rust `dc-glob` and Go `manvi/internal/fnmatch`, held together by a 775-case parity fixture. This is correct, but it is a standing obligation: a change to one must change the other. | VERIFIED | — |
| GAP-21 | **Unused dependencies and a dead match arm** in `devmap-*` crates, recorded in `INTEGRITY.md`. | MOVED to DevCouncil — see §2 | — |

---

## 6. What to do when you find a new one

1. Add it here with a **VERIFIED / RECORDED** label and the evidence.
2. If a ledger disagrees with the code, **fix the ledger in the same change** —
   §3 exists because that did not happen five times.
3. If it is a gate that can silently not run, it outranks whatever you were
   doing. That class has already produced three live defects in this port.
