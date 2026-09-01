# DevCouncil — Known Gaps and Defects

Everything known to be wrong, missing, or unproven, gathered so it is addressed
**during** the port rather than faithfully reproduced by it.

Companion to [`PORTING_TASKS.md`](PORTING_TASKS.md). Where a gap belongs to a
work package, its ID is named.

## How to read this

Gathered 2026-09-01 from `rust-port/STATUS.md`, `rust-port/INTEGRITY.md`,
`rust-port/DIVERGENCES.md`, `rust-port/CONSUMERS.md`, `IMPROVEMENTS.md`, the CI
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

**VERIFIED** (`rust-port/STATUS.md` SC34, measured 2026-08-17 with a
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

### GAP-2 — `dcstore` covers 2 of DevCouncil's 16 tables

**VERIFIED** this session. `tasks` and `task_leases` only. Absent: requirements,
critique findings, gaps, evidence, assumptions, artifact graph, planning state,
shell sessions and commands, file-change events, semantic diffs, handoffs,
correction manifests, verification runs, project state.
→ **P1.3**.

### GAP-3 — No MCP server in Rust/Go

**VERIFIED** this session — all 29 `devcouncil_*` handlers are still Python. This
is the front door of the component layer; without it no other coding agent can
use any of the ported work.
→ **P2.1–P2.6**.

### GAP-4 — `dc-verify` stub detection is weaker than the Python it replaces

**VERIFIED** by reading both. `dc-verify`'s `detect_stubs` is substring matching
over added diff lines. DevCouncil's `stub_detector.py` parses the AST, handles
per-language idioms, the `devcouncil: allow-stub` escape hatch gated on the task
mentioning scaffolding, assert-free test detection, and a separate
stub-declaration audit. **Cutting over today would weaken the gate.** This is
caution **C3**.
→ **P4.1** (measure), then **P4.3** (reach parity via `devmap`'s grammars).

### GAP-5 — Nothing detects drift between the two source copies

**VERIFIED** — `.github/workflows/analysis-plane.yml` has `rust`, `interop` and
`go` jobs and **zero** drift-check lines. The component sources exist in
DevCouncil and MANVI with no build-time relationship. Two files differ
deliberately (`dc-store/tests/interop.rs`, `dc-glob/src/lib.rs`'s `include_str!`
path) and four paths exist only in DevCouncil, so a naive `diff` is not the gate
— it has to know the exceptions.
→ **P0.1**, decision **D1**.

### GAP-6 — No Go component home in DevCouncil

**VERIFIED.** `backend/go_orchestrator/` holds harness-side *client* code
mirrored from MANVI. Most remaining work is Go and has nowhere to land.
→ **P0.3**, decision **D3**.

---

## 2. Recorded open — leads to verify before acting

From `rust-port/STATUS.md`'s "Not complete" line (2026-08-17) and
`CONSUMERS.md`. **RECORDED**, not re-checked here.

| ID | Gap | Notes | Task |
|---|---|---|---|
| GAP-7 | **VB.NET has no grammar.** Of 35 frozen `LANGUAGE_SPECS`, 34 reach a real grammar; `vb` returns `ParseOutcome::Failed`. | Adding a grammar dependency needs explicit approval under repo policy. | P2.4 |
| GAP-8 | **SC14 — nested-symbol identity collisions** inside anonymous callbacks and nested types. **741 duplicate identities** on the 12,821-file corpus. | Duplicate identities mean edges can attach to the wrong symbol. | P2.4 |
| GAP-9 | **SC29 — an unexplained per-file memory increase.** ~210 → ~278 KiB/file after SC26, cause withdrawn and reopened. The Σ N² model this was once explained by was **refuted by measurement**; peak memory is linear in edges at ~410 B/edge. | Reopened deliberately rather than left with a wrong explanation. | — |
| GAP-10 | **SC2 — B3 write-amplification redesign and genuinely incremental resolve.** `build --affected` still extracts the whole tree. | Every small edit pays a full rebuild. | — |
| GAP-11 | **Audit-property suite incomplete** — the 117-property spec is not fully covered. | | P0.2 |
| GAP-12 | **Mutation coverage** does not extend beyond the retention surface. | Mutation testing is what keeps the strengthened tests honest. | P0.2 |
| GAP-13 | **Freshness soak never run** — no two-week shadow soak, no production deployment, no platform publication. | `STATUS.md` is explicit that passing tests are "local/mechanical evidence only". | — |
| GAP-14 | **Consumer cutover incomplete.** `CONSUMERS.md` lists as pending: `orphan_diff.py` (path heuristic only), `subsystem_boundary.py`, `semantic_diff.py`, `acceptance_corpus.py`, `verify_orchestration.py` / `verifier.py`, MCP adjuncts (`ast_lsp`, `debug`), and remaining CLI/reporting/execution consumers. `cli/commands/map.py` is frozen. | These are the Python call sites still on the Python path. | P2.3, P2.4 |
| GAP-15 | **~42 mypy errors** in `llm/provider`, `executors`, `semantic_layer` and others (down from ~82). | `IMPROVEMENTS.md` 2026-07-08. Ported code should not inherit the untyped shapes. | — |

---

## 3. Recorded open but actually closed — do not spend time here

**STALE.** `rust-port/INTEGRITY.md`'s snapshot notice says these still need work.
The code says otherwise. Verified this session; the notice should be corrected.

| Ledger claim | Reality |
|---|---|
| **D10** — "still needs durable repository-root resolution" | **Closed.** `devmap-query/src/engine.rs` has `latest_repo_root()`, `resolve_source_path()` and `source_unavailable_reason`. |
| **D17** — "still needs a persisted unresolved-call ledger" | **Closed.** `Resolution::Unresolved` is constructed in `devmap-resolve/src/resolver.rs` and `devmap-analyze/src/liveness.rs`. SC30 further split the tier into four evidence-backed classes. |
| **T1–T9 vacuous tests** — "tests that cannot fail" | **Strengthened.** Spot-checked two: T1 now asserts `!paths2.contains("b.py")` with the message "deleted file must not remain as live nodes"; T3's `if let Some(edge)` pattern is gone from the test tree. |
| INTEGRITY: "29 of 35 languages return `ParseOutcome::Failed`" | **Superseded.** 34 of 35 reach a real grammar (only VB.NET does not). Note this is *parsing* — GAP-1 is about *call extraction*, which is a different and still-open question. |
| IMPROVEMENTS: `fail_under = 18` | **Superseded.** `pyproject.toml` now sets `fail_under = 90`. |
| `rust/STATUS.md`: "one file differs from MANVI's copy" | **Corrected in commit `a48c423`.** Two do. |

**Also closed:** the entire `IMPROVEMENTS.md` P0–P3 backlog (#1–#20) — git
subprocess timeouts, atomic writes, LLM retry, swallowed exceptions, test
coverage, the god-module split, the import cycle, JSON persistence, AST caching.
It is marked **FULLY CLOSED** and the suite was green at 1385 passed / 0 failed.
Do not re-file these.

---

## 4. Python baseline defects the port must **not** reproduce

**RECORDED** from `rust-port/DIVERGENCES.md`, which exists precisely because the
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
| GAP-16 | **Nothing in `src/devcouncil/` calls the ported components.** The Rust plane is additive; every Python gate, lease repository and search path runs as before. | VERIFIED | All of Wave 2 |
| GAP-17 | **No differential measurement** of Rust vs Python gates over real diffs. §4 of `rust/STATUS.md` records that Python leads on stub detection; **nothing else is measured** — the secrets scanner and orphan-diff comparisons are explicitly unverified. | VERIFIED | **P4.1** |
| GAP-18 | **Linux and Windows coverage is new and thin.** Everything before 2026-09-01 was verified on darwin/arm64 only. CI now runs ubuntu/macos/windows for the components, but `dc-store` on Windows compiles SQLite from source and was not confirmed locally. | VERIFIED | P0.2 |
| GAP-19 | **`MANVI_*` naming leaks into DevCouncil components.** `testsupport.AllowSkipEnv` is `MANVI_TEST_ALLOW_SKIP`; the build lock is `.manvi-testbin.lock`. Harmless but wrong once these are DevCouncil's. Renaming breaks any CI config referencing them, so it is a deliberate decision, not a drive-by. | VERIFIED | P0.3 |
| GAP-20 | **`dc-glob` is duplicated by design** — Rust `dc-glob` and Go `manvi/internal/fnmatch`, held together by a 775-case parity fixture. This is correct, but it is a standing obligation: a change to one must change the other. | VERIFIED | — |
| GAP-21 | **Unused dependencies** recorded in `INTEGRITY.md`: `regex`/`rayon` in resolve and analyze, `tracing` in 5 crates, `ignore` in cli; `thiserror` declared 7× and used 0×. Also a dead `"async_function_definition"` arm and CLI `manifest` ignoring its `path` arg. | RECORDED — some may have been pruned since | — |

---

## 6. What to do when you find a new one

1. Add it here with a **VERIFIED / RECORDED** label and the evidence.
2. If a ledger disagrees with the code, **fix the ledger in the same change** —
   §3 exists because that did not happen five times.
3. If it is a gate that can silently not run, it outranks whatever you were
   doing. That class has already produced three live defects in this port.
