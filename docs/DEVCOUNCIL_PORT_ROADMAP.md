# DevCouncil Port Roadmap

> **Status (2026-09):** DevCouncil's Python package (`src/devcouncil/`) has been
> deleted. Inventory paths under that tree are historical. Do not install or
> `import devcouncil` as a Python package. Live map/query is `devmap`.

> **Re-baselined 2026-10-10 against DevCouncil's Phase 7 decisions** —
> `DevCouncil/docs/PHASE7_LONG_TAIL.md` (the retirement table), `docs/TODO.md`
> (the follow-ups) — with DevCouncil at the pin in `scripts/module-pins.txt`
> (`acb5e3b9`). Every row below says which of **done**, **retired by Phase 7**,
> **superseded** or **open** it is, with the evidence. Rows keep their
> pre-Phase-7 wording only where it is the record of a decision; where the code
> disagreed, the code won and the row was rewritten. Where a statement was
> checked by reading rather than running, it says so.

**Goal:** port every DevCouncil capability to Rust/Go **inside DevCouncil**, so
that DevCouncil becomes a set of building blocks — binaries with a JSON-on-stdio
contract, plus an MCP server — usable by any coding agent. MANVI is the harness
that unifies them into a working agent and can be embedded into other
applications.

Read [`COMPONENTS_AND_HARNESS.md`](COMPONENTS_AND_HARNESS.md) first: it defines
the boundary, the contract a component satisfies, and how the harness consumes
them. This document is the inventory and the order.

**If you have been assigned a piece of this**, go to
[`PORTING_TASKS.md`](PORTING_TASKS.md) — it turns this inventory into numbered
work packages with acceptance criteria, cautions and a testing protocol.

**Correction (2026-09-01).** An earlier revision of this file said the goal was
to "retire DevCouncil's Python by moving what only it has into MANVI." That was
wrong, and the error mattered: it pointed the work at absorbing DevCouncil's
subsystems into the harness, which would have destroyed the thing that makes them
reusable by any agent that is not MANVI. DevCouncil's capabilities are ported
**in place**. MANVI gains no subsystem it does not already own.

**No new Python, and no Python clients.** Still true, for a reason the correction
does not touch. DevCouncil already ran that experiment: `DevCouncil/docs/devmap/CONSUMERS.md`
records seven "Rust-primary with a Python fallback" consumers plus a 718-line
client whose Rust path **never executed once** — it pointed at the Python schema,
the Rust store failed closed on it, and every call silently took the fallback for
months until SC23/SC24. The fallback is what hid it. A hybrid client cannot tell
you whether the Rust side works, because it is built to keep working when it does
not. Port a component and cut its consumers over to it; do not wrap it in a shim
that can quietly do nothing.

Claims below are labelled **verified** (a command was run and its output read),
**inferred** (read from source, not executed), or **unmeasured**.

---

## 1. Decisions

D1 and D3 are settled by what the repositories now are. D2 is the one that still
needs the owner.

### D1 — MANVI's copy of the component sources: **settled, option 3**

The options were delete the copy, keep it with a digest, or consume by path. The
repository did the third, not the recommended second. `crates/dc-*` are symlinks
into `../../DevCouncil/rust/dc-*` (`ls -la crates`; `crates/README.md`), so there
is one copy of the sources. `manvi/go.mod` replaces DevCouncil by path, and the
release builds the commit pinned in `scripts/module-pins.txt`. Drift is no longer
possible between copies, and drift between the working checkout and the pin is
caught by `scripts/check-release.mjs` `crateMirrorProblems` (inherited manifest
values compared at the pin) and `manvi/devcouncil/crate_pins_test.go` (resolved
dependencies). The two-commit "edit in DevCouncil, mirror to MANVI" rule is gone.
**Verified** by `ls -la crates` and by reading both checks; the checks themselves
were not re-run for this edit. **P0.1 is done.**

What the original reasoning still gets right: only *sources* were ever shared.
MANVI resolves `devmap`, `dcstore`, `dcverify` and `dcgrep` as executables
(`cmd/manvi/toolbinary.go`) and links none of them, except the Gusset archive.

### D2 — Where the council lives: **open, and now narrower**

Phase 7 hard-cut DevCouncil's `planning/` and `council/` with the successor
"Manvi `llm/` + agents" (`PHASE7_LONG_TAIL.md:22`), and states that "LLM provider
routing, agent profiles, TUI/`watch`, campaign-like multi-agent runs: stay in
Manvi — do not reimplement in DevCouncil Go" (`:56-57`). That removes options 1
and 2 below as written: DevCouncil will not own council schemas or prompts in Go.

No council code exists in either repository. The only `planner`, `critic` and
`arbiter` hits are the `dc.Source` enum (`dc/requirement.go:86-88`) and the
agent definitions (`manvi/agents/definition.go:173,187`). **Verified** by
search.

The owner must record one of two outcomes, and this file does not choose:

1. **Retired, no successor.** The debate protocol is dropped. P3.1 to P3.3
   become retired rows.
2. **Council in MANVI.** MANVI owns schemas, prompts and the protocol. Then P3.1
   to P3.3 are filed as feature tasks, under the correction below.

**Correction to the earlier plan, which this file used to state as fact:** "forcing
a single tool call whose input schema *is* the output schema … works across all
four providers without touching the provider seam" is wrong. `llm.Request`
(`manvi/llm/provider.go`, the struct at line 17) has no tool-choice field, and
`rg -uu -i 'toolchoice|tool_choice' manvi` finds nothing. Forced tool choice
needs a new `Request` field and an implementation in each of the four adapters.
The `fallback=` caution survives either way: an empty critique and a critique
that could not run must not read the same to the arbiter.

### D3 — The Go component home in DevCouncil: **settled de facto**

Go components live in `DevCouncil/backend/go_orchestrator/` (`gate/`, `policy/`,
`grants/`, `devcouncil/{gating,verify,stopgate,correction,mcp,skills}`, `dc/`).
`DevCouncil/go/` was proposed and does not exist. **P0.3 is done.** The directory
also holds the Go clients MANVI imports (`dc/{store,devmap,dcgrep,dcverify}`).

## 2. What is already done

Two different things were previously listed together here, and separating them
matters: a capability MANVI owns does **not** discharge DevCouncil's port of the
same-named subsystem, because they serve different consumers.

### 2a. DevCouncil components genuinely ported

| Component | Replaces | Status |
|---|---|---|
| `devmap` | `indexing` 18,068 + `codeintel` 8,405 = **26,473** | **Done, as Rust** — `rust/`, 49,675 lines. The single largest subsystem, and it needs consuming rather than porting. **Verified** at the time: three `TestTheLive*` tests drive the real binary; they now live with the client in DevCouncil (`backend/go_orchestrator/dc/devmap`). |
| `dcstore` — leases | part of `storage` | **Done.** The Python `TaskLeaseRepository` it was proven against is deleted; the Python-driven interop check is retired (`dc/requirement_interop_test.go` now holds one empty marker test), and lease behaviour is covered by the Rust suite (`dc-store/tests/leases.rs`). |
| `dcstore` — task requirements | part of `domain` | **Done 2026-09-01.** `requirement_ids` / `acceptance_criterion_ids` cross the boundary. The pydantic parity check is dropped with the Python it compared against; wire shape is owned by `dc/requirement_test.go`. |
| `dcverify` | part of `verification` | **Done for the gates.** Diff parsing, scope classification, coverage intersection, rigor gates and AST stub detection (`rust/dc-verify/src/stub_ast.rs`, P4.1/P4.3). The Go side (`devcouncil/verify/`) spawns it. |
| `dcgrep` | — | **Done.** Ignore-aware search on ripgrep's linked engine; no Python predecessor. |

### 2b. Harness capabilities MANVI owns — *not* DevCouncil ports

Listing these as "done" was the error. DevCouncil's same-named subsystems serve
its consumers; MANVI's serve the harness. Porting one does not remove the other.

| MANVI capability | Size | Relationship to DevCouncil's subsystem |
|---|---|---|
| `gate` + `policy` + `grants` | 9,513 | A 6-rung ladder with 5 outcome states, for the harness's own tool calls. DevCouncil's `gating` is the **component-side** write gate and is ported (`devcouncil/gating/policy.go`, served as the `devcouncil_policy_check_write` MCP tool). Two gates, two consumers. |
| `ui` | 22,567 | The harness's TUI. DevCouncil's `ui` (753) is CLI presentation and is most likely **obsolete** rather than portable: components emit JSON, they do not render. Confirm before deleting. |
| `llm` | 20,379 | Four providers with local-model support. DevCouncil's `llm` router was hard-cut (`PHASE7_LONG_TAIL.md`, successor "Manvi `llm/`"), so this is the only one. |
| `mcp` (client) | 4,492 | **Correct as-is.** Consumes MCP servers, including the one DevCouncil is becoming. This is not a missing server — the server belongs on the component side. |

## 3. Status of every subsystem (2026-10-10)

The original table listed each DevCouncil Python subsystem as "awaiting its Rust/Go
port". The Python is deleted, and Phase 7 decided each row. Sizes are the
2026-09 Python line counts, kept as the record. "Row" cites
`DevCouncil/docs/PHASE7_LONG_TAIL.md` line numbers where it decides the row.

| DevCouncil subsystem | Python LOC | Status | Evidence |
|---|---|---|---|
| `integrations` (MCP server) | 11,284 | **Superseded.** The Go host serves MCP with **8 tools**; the thick surface is MANVI's native registry (40 `devcouncil_*` tools listed in `TOOLS_REFERENCE.md`). The "29 tools, headline deliverable" framing is gone. | `backend/go_orchestrator/cmd/devcouncil/main.go:70`, `devcouncil/registry.go`; `docs/TODO.md` `GAP-P7-MCP-HOST-THIN`; `PHASE7_LONG_TAIL.md` "Thick MCP" |
| `cli` | 16,547 | **Done (P5.12).** The Go host's `commands` map is the CLI: `mcp`, `integrate`, `skills`, `verify`, `grep`, `install`, `gate`, `hook`, `map`/`graph`/`ast`, and more. Commands that were application glue were dropped (exit 2). | `main.go:69-93`; `PHASE7_LONG_TAIL.md` "dev / devcouncil is the Go binary" |
| `verification` | 9,800 | **Done, in four parts.** AST stub detection and test-rigor in `dc-verify` (P4.1, P4.3); test execution, acceptance compiler, gap ids and next actions in Go (P4.2, P4.4, partly audited, see `PORTING_TASKS.md`). | `rust/dc-verify/src/stub_ast.rs`; `devcouncil/verify/{commands,criteria,gapid,next_actions}.go` |
| `execution` | 5,881 | **Split, as predicted.** Lease and scope are `dcstore` and `devcouncil/{lease,gating}`; stop-gate and correction manifests are `devcouncil/{stopgate,correction}`; turn-driving is MANVI's. Not audited line by line. | `backend/go_orchestrator/devcouncil/` listing |
| `executors` | 3,765 | **Retired.** "OpenHands / mini-SWE / Claude SDK executors: Retire; MANVI loop / hero-loop skill." | `PHASE7_LONG_TAIL.md:30` |
| `knowledge` | 2,338 | **Retired** (P5.1). Agent guides come from `devmap build --guides`. | `:11` |
| `reporting` | 1,947 | **Retired** (P5.2). Successor: `devcouncil verify --json`, `devcouncil_get_gaps`, MANVI session logs. | `:12` |
| `campaign` | 1,700 | **Retired** (P5.3). Successor: MANVI multi-agent profiles. | `:14` |
| `storage` | 1,574 | **Done as a transcription, with a residue.** `dc-store` creates 18 tables and the workbench adds 19 `work_*` tables. Six of the 18 are referenced by no code, and `correction_manifests` is never written; that is `ft-46ab2b`. Task creation has no product command: `TASK-P7-11`. | `rust/dc-store/src/schema.rs`; `PHASE7_LONG_TAIL.md:26`; `docs/TODO.md:26` |
| `planning` + `council` | 1,470 | **Hard-cut, successor "Manvi `llm/` + agents"; no council code exists.** See D2. | `PHASE7_LONG_TAIL.md:22,56-57` |
| `app` | 1,472 | **Unaudited** (P5.9). MANVI has `flags`, `bootstrap`, `core`. | — |
| `live` | 1,212 | **Retired** (P5.4). | `:15` |
| `optimization` | 998 | **Retired** (P5.7), no successor. | `:17` |
| `telemetry` | 918 | **Retired** (P5.6). Successor: MANVI session NDJSON and `devmap session-report`. | `:13` |
| `repo` | 870 | **Retired** (P5.5). SCA has no DevCouncil successor; GitPulse runs the audit tools as a related job. GitHub Checks were handed to GitPulse. | `:28`, "Successor corrections" |
| `skills` | 487 | **Done** (P5.10): embedded skill library with `devcouncil skills` (`list`, `scaffold`). | `devcouncil/skills/` |
| `utils` | 529 | Unaudited; internal. | — |
| `domain` | 305 | **Partial.** Only `Gap` has a Go type in the component module (`devcouncil/verify/types.go:17`). There are no `Evidence`, `Assumption`, `CritiqueFinding` or `CheckpointRefs` types. The pydantic-parity part is dropped with the Python. | `rg '^type (Gap\|Evidence\|Assumption\|CritiqueFinding\|CheckpointRefs)\b'` over both repositories |

### What decides the schedule now

The three items this section used to name no longer decide anything: the MCP
server exists at the size Phase 7 chose, `domain` + `storage` is transcribed, and
verification is ported. What remains open:

1. **D2** (above): retire the council or build it in MANVI, including forced
   tool choice in four adapters.
2. **`TASK-P7-11` / `GAP-P7-NO-TASK-CREATE`** (`DevCouncil/docs/TODO.md:26`): no
   production code writes `tasks` or `requirements` rows. Every hit for `INTO tasks`
   and `INTO requirements` across DevCouncil, MANVI and GitPulse is a test. P1.2
   (`backfill_acceptance_criteria`) has no input until this lands, so it is linked
   to that task here rather than filed alone.
3. **The `domain` types** that no consumer has yet asked for. Do not port them
   speculatively.

## 4. Standing risks

- **A skipped check that reports `ok`.** Found twice in this port and still the
  rule: when porting a gate, port its failure mode first. The specific gate it
  was found in, the `dc-store` Python interop test and its opt-in environment switch, is
  retired: nothing reads that switch any more (`DevCouncil/rust/README.md:245`,
  and no source in either repository, ignored files included), so setting it
  changes nothing.
- **Python defaults versus Go zero values.** Still a hazard for any wire type that
  decodes optional keys with a plain struct tag; `dc/requirement.go` is the worked
  example. The `exclude_defaults=True` check against pydantic is retired with
  pydantic; `dc/requirement_test.go` owns it.
- **Porting a weaker implementation and calling it a port.** Resolved for stub
  detection: the AST detector is in `dc-verify`, with a false-accept and
  false-positive corpus at zero (`tests/false_accept.rs`). The principle stands for
  whatever is ported next.
- ~~**Nothing is on Linux. No CI.**~~ **Closed.** `.github/workflows/verify.yml`
  runs `verify.sh` on `ubuntu-latest` and `macos-latest`. Windows is not covered
  and the workflow says why; that is `ft-7fb7af56` and `KNOWN_GAPS.md` GAP-18.
- **A component quietly becoming harness-only.** Unchanged. The check is §5 of
  [`COMPONENTS_AND_HARNESS.md`](COMPONENTS_AND_HARNESS.md): *can an agent that is
  not MANVI use this?* The Go host's MCP server, `devmap`, `dcverify` and `dcstore`
  can be used without MANVI; MANVI's native registry cannot, by design.
- ~~**Two sources of truth for one component.**~~ **Closed by D1.** One copy,
  consumed by path and pinned.
