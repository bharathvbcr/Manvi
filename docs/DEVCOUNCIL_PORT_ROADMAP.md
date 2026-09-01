# DevCouncil → MANVI Port Roadmap

**Goal:** retire DevCouncil's Python by moving what only it has into MANVI, which
is already a working Go/Rust harness. Chosen 2026-09-01 over the alternative of
Rust-ifying DevCouncil from the inside behind Python clients.

**Why this direction.** DevCouncil already ran the other experiment. `rust-port/
CONSUMERS.md` records seven "Rust-primary with a Python fallback" consumers plus
a 718-line client whose Rust path **never executed once** — the client pointed at
the Python schema, the Rust store failed closed on it, and every call silently
took the fallback for months until SC23/SC24. The fallback is what hid it. A
hybrid client cannot tell you whether the Rust side works, because it is built to
keep working when it does not.

So: **no new Python.** Every line of a Python client is a line to delete later,
and the one this repository already wrote reported success while doing nothing.

Claims below are labelled **verified** (a command was run and its output read),
**inferred** (read from source, not executed), or **unmeasured**.

---

## 1. Open decisions

Two, both needing an answer before the next large increment.

### D1 — The two copies of the analysis plane

Since 2026-09-01 the `dc-*` crates and the Go IPC clients exist in **both**
repositories, with no build-time relationship between them:

| | MANVI | DevCouncil |
|---|---|---|
| Rust crates | `crates/dc-{glob,grep,store,verify}` | `rust/dc-{glob,grep,store,verify}` |
| Go clients | `manvi/dc/{store,dcgrep,devmap}` | `backend/go_orchestrator/dc/…` |

Nothing fails when they drift. This is not hypothetical: the very first change
after the port — carrying `requirement_ids` across the boundary — had to be
applied **twice by hand**, and it stayed consistent only because one person did
both halves in one sitting. One file is deliberately different and must stay so
(`dc-store/tests/interop.rs`: DevCouncil's copy resolves the repository as its
own ancestor, MANVI's searches upward for a sibling checkout).

The options, recorded in DevCouncil's `rust/STATUS.md` §6:

1. **Vendor with a digest.** Keep editing in MANVI, re-vendor on a cadence, and
   check in a hash so a stale copy is a test failure rather than a surprise.
2. **One workspace, consumed by path or git dependency.** Removes the
   duplication; couples the two repositories' release cycles.
3. **Accept it as temporary.** Under this roadmap DevCouncil's Python is retired
   and its copy goes with it, so the duplication has a known end date.

**Recommendation: 3, with 1 as insurance.** Option 3 is the honest reading of
this roadmap, but "temporary" has no enforcement, and the ledger already shows
one hand-mirrored change. A digest check is cheap and makes drift loud for
however long temporary turns out to be. **Not yet chosen.**

### D2 — Is the council the next increment?

The council is DevCouncil's identity and the largest capability MANVI has no form
of. It is now unblocked: tasks carry `RequirementIDs` and
`AcceptanceCriterionIDs`, and `dc.Requirement` / `dc.AcceptanceCriterion` exist
and are checked against DevCouncil's own pydantic models.

**What it is** (verified, read from `src/devcouncil/planning/` and
`src/devcouncil/council/prompts/`): a structured debate, not a single planning
call.

```
planner_a ─┐                    ┌─ critic_b reviews A ─┐
           ├─ two rival plans ──┤                      ├─ rebuttal ─ arbiter ─ final plan
planner_b ─┘                    └─ critic_a reviews B ─┘
```

- **planner_a** is the pragmatic tech lead (simplicity, minimal dependencies);
  **planner_b** is the production-readiness architect (security, performance,
  edge cases). Two genuinely different objective functions, not two samples.
- **critics** are prompted as hostile staff engineers reviewing *the other
  team's* plan. Every finding must carry a `falsifiable_check`.
- **rebuttal** returns each finding to its planner, who may reject it only with
  evidence.
- **arbiter** merges into the final plan.
- `backfill_acceptance_criteria` then guarantees every acceptance criterion is
  owned by exactly one task — the gap `dc.UnownedCriteria` now detects.

**What MANVI already has:** the LLM plane (`llm/`, 20,379 lines, four providers),
the subagent machinery (`agents/`, `devcouncil/subagent_tools.go`), and shipped
roles including `planner` and `critic`.

**What is missing, and is the actual work:**

1. **Schema-constrained completion.** DevCouncil's `router.complete_structured(
   schema=…, fallback=…)` is what makes the debate machine-readable — each role
   returns a validated `PlanOutput` / `CritiqueOutput` / `RebuttalOutput` rather
   than prose to be scraped.

   MANVI has no `complete_structured` equivalent, but it is **closer than it
   looks**: `llm.ToolSchema` already carries `InputSchema json.RawMessage`
   (verified, `llm/provider.go:11-14`), which is JSON Schema on the tool-call
   path. Forcing a single tool call whose input schema *is* the output schema is
   the standard way to get structured output, and it works across all four
   providers without touching the provider seam. So this is likely a wrapper over
   machinery that exists, not new transport work. **Unmeasured** — no attempt has
   been made, and the per-provider forced-tool-choice details are unchecked.
2. **The `fallback=` semantics, carefully.** DevCouncil degrades an unparseable
   critique to "no findings" so a weak model cannot crash a planning run. That is
   defensible for a critique and **dangerous everywhere else**: an empty critique
   and a critique that could not run must not read the same to the arbiter. Port
   the graceful degradation *and* make the difference visible, or this reproduces
   the silent-pass shape the rest of the harness refuses.
3. **The debate protocol itself** — role pairing, cross-assignment (each critic
   reviews the *other* plan), rebuttal routing, arbitration.
4. **Eight prompt files**, moved as-is from `src/devcouncil/council/prompts/`.
5. **`backfill_acceptance_criteria`** (~50 lines of pure logic, trivially
   testable, no LLM needed — the natural first commit).

**Estimate: ~1,500–2,000 lines of Go plus tests. Unmeasured** — from reading the
Python, not from an attempt.

---

## 2. What is already done

**Verified.** Code intelligence is the largest subsystem and it is *already Rust*
— it does not need porting, only consuming.

| Capability | DevCouncil Python | Status |
|---|---|---|
| Code intelligence / AST graph | `indexing` 18,068 + `codeintel` 8,405 = **26,473** | **Done, as Rust.** `rust-port/` (49,675 lines). MANVI drives it over `dc/devmap`; the three `TestTheLive*` tests build a fixture repo and run the real `devmap` binary, verified against DevCouncil's current `rust-port` build. |
| Policy / gating | `gating` 692 | **Done.** MANVI's `gate` + `policy` + `grants` = 9,513 lines, a 6-rung ladder with 5 outcome states. |
| Terminal UI | `ui` 753 | **Done.** MANVI's `ui` = 22,567 lines. |
| Lease mutual exclusion | part of `storage` | **Done and proven interoperable.** `dc-store` and DevCouncil's `TaskLeaseRepository` agree on schema, token and expiry against one `state.sqlite`. |
| Task requirements / acceptance criteria | part of `domain` | **Done 2026-09-01.** Carried across the boundary; `dc.Requirement` checked against DevCouncil's pydantic models, including `exclude_defaults=True`. |

---

## 3. What is left

Subsystem sizes are **verified** (`wc -l`); the coverage judgements are
**inferred** from reading both sides. This is an inventory, not a burn-down — a
Go port is not line-for-line with the Python it replaces.

| DevCouncil subsystem | Python LOC | MANVI counterpart | Status |
|---|---|---|---|
| `cli` | 16,547 | `cmd/manvi` — **17** top-level commands against DevCouncil's **54** | **Partial.** Missing: `plan`, `requirements`, `gaps`, `repair`, `report`, `evidence`, `provenance`, `campaign`, `wiki`, `okf`, `skills`, `handoff`, `rollback`, `runs`, `trace`, `semantic`, `dashboard`, `cost`, and more. |
| `integrations` | 11,284 | **none** | **Absent, and structural.** DevCouncil's `integrations/mcp/` is an MCP **server** with **29** `devcouncil_*` handlers — the surface every coding agent drives it through. MANVI's `mcp/` is a **client** that consumes other servers; `serve/` is a different protocol (NDJSON stdio). Without this, agents lose their way in. |
| `verification` | 9,800 | `devcouncil/verify.go` + `rigor.go` + `verify_paths.go` (1,372) + `dc-verify` (1,793) | **Partial, and Python leads.** Its `stub_detector.py` does AST analysis where `dc-verify` does substring matching; it also has the acceptance compiler, diff-coverage instrumentation, gap ids, and next-actions. Do not cut over without a differential run. |
| `execution` | 5,881 | `gate`, `agent`, `session`, `grants` | **Mostly covered, needs an audit.** Checkpoints, handoff, stop-gate history and correction manifests have no obvious counterpart. |
| `executors` | 3,765 | — | **Likely obsolete, not missing.** These are adapters that shell out to Claude Code / OpenHands / Aider. MANVI *is* the agent loop, so most of this disappears rather than ports. **Confirm before deleting.** |
| `knowledge` | 2,338 | — | Absent. OKF knowledge base / wiki. |
| `reporting` | 1,947 | — | Absent. Evidence bundles, HTML reports. |
| `campaign` | 1,700 | — | Absent. Multi-task campaign orchestration. |
| `storage` | 1,574 | `dc-store` covers **2 of 16 tables** | **Partial.** `tasks` and `task_leases` only. Absent: requirements, critique findings, gaps, evidence, assumptions, artifact graph, planning state, shell sessions/commands, file-change events, semantic diffs, handoffs, correction manifests, verification runs, project state. |
| `planning` + `council` | 1,470 | roles exist, protocol does not | **Absent — see D2.** |
| `app` | 1,472 | `flags`, `bootstrap`, `core` | Probably covered; unaudited. |
| `live` | 1,212 | — | Absent. Live review cards, repair prompts. |
| `optimization` | 998 | — | Absent. |
| `telemetry` | 918 | `ui` event bus | Partial. |
| `repo` | 870 | — | Absent. CI scaffold, gitignore, SCA. |
| `skills` | 487 | — | Absent. |
| `utils` | 529 | `internal/` | Partial. |
| `domain` | 305 | `dc/` | **Partial.** Task and requirement done; gap, evidence, assumption, critique, checkpoint refs remain. |

### The three that decide the schedule

1. **`integrations` (MCP server).** Nothing else changes how DevCouncil is *used*.
   Until MANVI exposes the `devcouncil_*` tools, no agent can drive it the way
   agents drive DevCouncil today, and the port cannot be dogfooded.
2. **`storage` (14 of 16 tables).** Requirements, gaps, evidence and critique
   findings are what the council and the verifier read and write. Most remaining
   subsystems are blocked behind these tables.
3. **`verification`.** The largest subsystem where **Python is genuinely ahead**.
   This is a real port, not a move, and the honest sequencing is: measure a
   differential run first, then port what wins.

### Suggested order

Each step is chosen so the next one is testable.

1. `backfill_acceptance_criteria` + schema-constrained completion in `llm/` (D2's
   prerequisites — the second is useful on its own).
2. The council debate protocol (D2).
3. Storage: requirements, critique findings, gaps, evidence — the tables the
   council writes.
4. The MCP server surface, so the whole thing can be driven by an agent and
   dogfooded.
5. Verification, differential-measured before anything is replaced.
6. Everything else, by whatever the dogfooding says hurts most.

---

## 4. Standing risks

- **A skipped check that reports `ok`.** Already found twice in this port: the
  `dc-store` interop tests reported `ok` in 0.00s for three checks that never ran,
  in both repositories, for different path reasons. Assume more exist. When
  porting a gate, port its failure mode first.
- **Python defaults versus Go zero values.** `required: bool = True` decoded with
  a plain struct tag turns every criterion whose producer omitted the key into an
  optional one — still listed, still looking checked. Every pydantic field with a
  default is this hazard. Check each one against
  `model_dump_json(exclude_defaults=True)`.
- **Porting a weaker implementation and calling it a port.** `dc-verify`'s
  `detect_stubs` is substring matching; DevCouncil's is AST-based. The Rust name
  matching the Python name is not evidence the behaviour does.
- **Nothing is on Linux.** Everything verified here ran on darwin/arm64 only.
- **No CI.** Neither the Rust workspace nor the Go module is wired into
  `.github/`. `DC_STORE_REQUIRE_INTEROP=1` exists so CI can demand the interop
  evidence, and nothing sets it.
