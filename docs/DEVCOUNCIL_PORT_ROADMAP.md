# DevCouncil Port Roadmap

**Goal:** port every DevCouncil capability to Rust/Go **inside DevCouncil**, so
that DevCouncil becomes a set of building blocks — binaries with a JSON-on-stdio
contract, plus an MCP server — usable by any coding agent. MANVI is the harness
that unifies them into a working agent and can be embedded into other
applications.

Read [`COMPONENTS_AND_HARNESS.md`](COMPONENTS_AND_HARNESS.md) first: it defines
the boundary, the contract a component satisfies, and how the harness consumes
them. This document is the inventory and the order.

**Correction (2026-09-01).** An earlier revision of this file said the goal was
to "retire DevCouncil's Python by moving what only it has into MANVI." That was
wrong, and the error mattered: it pointed the work at absorbing DevCouncil's
subsystems into the harness, which would have destroyed the thing that makes them
reusable by any agent that is not MANVI. DevCouncil's capabilities are ported
**in place**. MANVI gains no subsystem it does not already own.

**No new Python, and no Python clients.** Still true, for a reason the correction
does not touch. DevCouncil already ran that experiment: `rust-port/CONSUMERS.md`
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

## 1. Open decisions

Two. The architecture correction above changes the answer to both.

### D1 — MANVI's copy of the component sources

The `dc-*` crate sources exist in DevCouncil (`rust/`) and in MANVI (`crates/`),
with no mechanism keeping them equal. Not hypothetical: the first change after
the port — carrying `requirement_ids` across the boundary — had to be applied
**twice by hand**, and stayed consistent only because one person did both halves
in one sitting.

**What the correction settles:** these are DevCouncil components, so **DevCouncil
is upstream**. Authorship in MANVI is history, not ownership. That reverses the
earlier recommendation, which had MANVI as the destination and treated
DevCouncil's copy as the temporary one.

**What it also settles:** only *sources* are duplicated. **Verified**
(`cmd/manvi/toolbinary.go`): MANVI resolves `devmap`, `dcstore`, `dcverify` and
`dcgrep` as executables and links none of them. The deployed arrangement is
already right — one set of component binaries, one harness consuming them. So
this is a development-convenience question, not an architectural one.

That leaves how MANVI's copy should end:

1. **Delete it; require installed components.** The honest expression of
   "DevCouncil owns the components". `toolBinary`'s local-build fallback stops
   being reachable and MANVI's suite needs the binaries on `PATH` — which its
   `devmap` live-contract tests already assume.
2. **Keep it, with a checked-in digest.** MANVI stays buildable without a
   DevCouncil checkout, and a stale copy becomes a test failure rather than a
   surprise.
3. **Consume by path or git dependency.** Removes the source duplication at the
   cost of coupling release cycles — and buys nothing at runtime, since nothing
   links.

**Recommendation: 2 now, 1 once installing DevCouncil's components is routine.**
Option 2 is the cheapest thing that makes drift *detectable*, which is the actual
failure mode. **Not yet chosen.** Until then: **edit in DevCouncil, mirror to
MANVI.**

### D2 — Where the council lives

The council is DevCouncil's identity and has no counterpart in MANVI. It is
unblocked: tasks carry `RequirementIDs` and `AcceptanceCriterionIDs`, and
`dc.Requirement` / `dc.AcceptanceCriterion` exist and are checked against
DevCouncil's own pydantic models.

**What it is** (verified, from `src/devcouncil/planning/` and
`src/devcouncil/council/prompts/`): a structured debate, not one planning call.

```
planner_a ─┐                    ┌─ critic_b reviews A ─┐
           ├─ two rival plans ──┤                      ├─ rebuttal ─ arbiter ─ final plan
planner_b ─┘                    └─ critic_a reviews B ─┘
```

- **planner_a** is the pragmatic tech lead (simplicity, minimal dependencies);
  **planner_b** the production-readiness architect (security, performance, edge
  cases). Two different objective functions, not two samples.
- **critics** are prompted as hostile staff engineers reviewing *the other team's*
  plan. Every finding carries a `falsifiable_check`.
- **rebuttal** returns each finding to its planner, who may reject it only with
  evidence.
- **arbiter** merges into the final plan.
- `backfill_acceptance_criteria` then guarantees every acceptance criterion is
  owned by exactly one task — the gap `dc.UnownedCriteria` now detects.

**The question the correction opens.** The council needs LLM calls, and the two
repositories split on exactly that line: DevCouncil is deterministic building
blocks, MANVI is the dynamic layer that talks to models. So the council does not
sit cleanly on one side.

Three ways to place it:

1. **Split on the determinism line.** DevCouncil owns the schemas
   (`PlanOutput` / `CritiqueOutput` / `RebuttalOutput`), the eight prompts,
   `backfill_acceptance_criteria`, and validation — all deterministic, all
   testable without a model. MANVI owns running the debate, because it already
   has the provider seam, the subagent machinery, and roles named `planner` and
   `critic`.
2. **Wholly in DevCouncil**, which means porting DevCouncil's `llm/` router
   (2,586 lines) too, so the component can call models itself.
3. **Wholly in MANVI**, which makes the council unavailable to any other agent —
   contradicting the point of the component layer.

**Recommendation: 1.** It matches the stated split exactly, keeps the valuable
and hard-to-reproduce part (the prompts and schemas that encode *how to run a
council*) reusable by other agents, and avoids giving a component its own model
credentials. Option 3 is the one to avoid. **Not yet chosen.**

**What the work is, under option 1:**

1. **Schema-constrained completion in MANVI.** DevCouncil's
   `router.complete_structured(schema=…, fallback=…)` is what makes the debate
   machine-readable. MANVI has no equivalent, but it is **closer than it looks**:
   `llm.ToolSchema` already carries `InputSchema json.RawMessage` (verified,
   `llm/provider.go:11-14`) — JSON Schema on the tool-call path. Forcing a single
   tool call whose input schema *is* the output schema is the standard way to get
   structured output, and works across all four providers without touching the
   provider seam. Likely a wrapper over existing machinery. **Unmeasured.**
2. **The `fallback=` semantics, carefully.** DevCouncil degrades an unparseable
   critique to "no findings" so a weak model cannot crash a planning run.
   Defensible for a critique and **dangerous as a default**: an empty critique and
   a critique that could not run must not read the same to the arbiter. Port the
   degradation *and* make the difference visible, or this reproduces the
   silent-pass shape the rest of the system refuses.
3. **The debate protocol** — role pairing, cross-assignment, rebuttal routing,
   arbitration.
4. **Eight prompt files and the output schemas**, as DevCouncil components.
5. **`backfill_acceptance_criteria`** — ~50 lines of pure logic, no LLM, trivially
   testable. The natural first commit, and it belongs in DevCouncil.

**Estimate: ~1,500–2,000 lines plus tests, split across both repositories.
Unmeasured** — from reading the Python, not from an attempt.

## 2. What is already done

Two different things were previously listed together here, and separating them
matters: a capability MANVI owns does **not** discharge DevCouncil's port of the
same-named subsystem, because they serve different consumers.

### 2a. DevCouncil components genuinely ported

| Component | Replaces | Status |
|---|---|---|
| `devmap` | `indexing` 18,068 + `codeintel` 8,405 = **26,473** | **Done, as Rust** — `rust-port/`, 49,675 lines. The single largest subsystem, and it needs consuming rather than porting. **Verified**: MANVI's three `TestTheLive*` tests build a fixture repository and drive the real binary, against DevCouncil's current `rust-port` build. |
| `dcstore` — leases | part of `storage` | **Done and proven interoperable.** `dc-store` and DevCouncil's `TaskLeaseRepository` agree on schema, token and expiry against one `state.sqlite`, with the interop test failing rather than skipping when it cannot run. |
| `dcstore` — task requirements | part of `domain` | **Done 2026-09-01.** `requirement_ids` / `acceptance_criterion_ids` cross the boundary; `dc.Requirement` and `dc.AcceptanceCriterion` are checked against DevCouncil's own pydantic models, including under `exclude_defaults=True`. |
| `dcverify` | part of `verification` | **Partial.** Diff parsing, scope classification, coverage intersection and rigor gates exist. See §3 — Python still leads on stub detection. |
| `dcgrep` | — | **Done.** Ignore-aware search on ripgrep's linked engine; no Python predecessor. |

### 2b. Harness capabilities MANVI owns — *not* DevCouncil ports

Listing these as "done" was the error. DevCouncil's same-named subsystems serve
its consumers; MANVI's serve the harness. Porting one does not remove the other.

| MANVI capability | Size | Relationship to DevCouncil's subsystem |
|---|---|---|
| `gate` + `policy` + `grants` | 9,513 | A 6-rung ladder with 5 outcome states, for the harness's own tool calls. DevCouncil's `gating` (692) is the **component-side** write gate — already an MCP tool (`policy`) — and still needs its Rust/Go port. Two gates, two consumers. |
| `ui` | 22,567 | The harness's TUI. DevCouncil's `ui` (753) is CLI presentation and is most likely **obsolete** rather than portable: components emit JSON, they do not render. Confirm before deleting. |
| `llm` | 20,379 | Four providers with local-model support. DevCouncil's `llm` (2,586) is a router the council needs; whether it ports depends on D2. |
| `mcp` (client) | 4,492 | **Correct as-is.** Consumes MCP servers, including the one DevCouncil is becoming. This is not a missing server — the server belongs on the component side. |

## 3. What is left

Every row is a DevCouncil subsystem awaiting its Rust/Go port **in DevCouncil**.
The question for each is *what shape does it take as a component* — a binary, an
MCP tool, or a schema — not "what is MANVI's counterpart". Where MANVI already
has something, that is noted as a consumer or an overlap, not as the destination.

Subsystem sizes are **verified** (`wc -l`); shape and status judgements are
**inferred** from reading both sides. An inventory, not a burn-down: a Go port is
not line-for-line with the Python it replaces.

The **Lang** column is the recommendation from
[`COMPONENTS_AND_HARNESS.md` §4](COMPONENTS_AND_HARNESS.md#4-which-language-each-component-belongs-in),
which carries the decision rule and the reasoning per component. The short
version: **1,574 lines of the 64,828 remaining are Rust work** (`storage`,
extending `dcstore`) plus targeted extensions to `dcverify` and `devmap`.
Everything else is Go, because the parsing-heavy, memory-sensitive half of
DevCouncil is already done as `devmap`.

| DevCouncil subsystem | Python LOC | Shape as a component | Lang | Status |
|---|---|---|---|---|
| `integrations` | 11,284 | **MCP server, 29 `devcouncil_*` tools** | Go | **The headline deliverable.** This *is* "MCP layer for coding agents" — the surface Claude Code, Cursor and MANVI all drive it through. MANVI's `mcp/` is already a client, so it consumes this the day it exists. Highest priority. |
| `cli` | 16,547 | Per-component CLIs | per component | **Partial, and not a like-for-like port.** DevCouncil's 54 commands are one application's surface; as components they split across `devmap`, `dcstore`, `dcverify` and new binaries. MANVI's 17 commands are the *harness's* own and are not meant to match. Port the commands that are component operations; drop the ones that were only application glue. |
| `verification` | 9,800 | Extend `dcverify` | **split** | **Partial, and Python leads.** `stub_detector.py` does AST analysis where `dc-verify` does substring matching; Python also has the acceptance compiler, diff-coverage instrumentation, gap ids and next-actions. Do not cut over without a differential run. |
| `execution` | 5,881 | Split | Go | **Needs an audit.** Lease/scope operations are component work (`dcstore`). The turn-driving parts are harness work MANVI already owns. Checkpoints, handoff, stop-gate history and correction manifests have no counterpart on either side. |
| `executors` | 3,765 | Probably nothing | Go | **Likely obsolete.** Adapters that shell out to Claude Code / OpenHands / Aider. Under this architecture those agents consume DevCouncil's MCP server directly instead of being driven by it. **Confirm before deleting** — inverting a dependency is not the same as removing a feature. |
| `knowledge` | 2,338 | Binary or MCP tools | Go | Absent. OKF knowledge base / wiki. |
| `reporting` | 1,947 | Binary | Go | Absent. Evidence bundles, HTML reports. |
| `campaign` | 1,700 | Binary or MCP tools | Go | Absent. Multi-task campaign orchestration. |
| `storage` | 1,574 | Extend `dcstore` | **Rust** | **Partial: 2 of 16 tables.** `tasks` and `task_leases` only. Absent: requirements, critique findings, gaps, evidence, assumptions, artifact graph, planning state, shell sessions/commands, file-change events, semantic diffs, handoffs, correction manifests, verification runs, project state. |
| `planning` + `council` | 1,470 | Schemas + prompts here, execution in MANVI | Go | **Absent — see D2**, which is about where the split falls. |
| `app` | 1,472 | Mostly harness | Go | Config and bootstrap; MANVI has `flags`, `bootstrap`, `core`. Unaudited. |
| `live` | 1,212 | MCP tools | Go | Absent. Live review cards, repair prompts. |
| `optimization` | 998 | Binary | Go | Absent. |
| `telemetry` | 918 | Split | Go | Partial. Component-side counters vs the harness's event bus. |
| `repo` | 870 | Binary | Go | Absent. CI scaffold, gitignore, SCA. |
| `skills` | 487 | Assets | Go | Absent. |
| `utils` | 529 | Internal | Go | Partial. |
| `domain` | 305 | **Shared schemas** | both | **Partial.** Task and requirement done and cross-checked against pydantic. Gap, evidence, assumption, critique and checkpoint refs remain. These are the contract every other component reads, so they lead. |

### The three that decide the schedule

1. **`integrations` (the MCP server).** Nothing else changes what DevCouncil *is*.
   Until the `devcouncil_*` tools exist in Rust/Go, the component layer has no
   front door, no other agent can use it, and the port cannot be dogfooded.
2. **`domain` + `storage` (14 of 16 tables).** Requirements, gaps, evidence and
   critique findings are what the council and the verifier read and write. Most
   remaining subsystems are blocked behind these schemas and tables.
3. **`verification`.** The largest subsystem where **Python is genuinely ahead**.
   A real port, not a move. Measure a differential run first, then port what wins.

### Suggested order

Each step chosen so the next one is testable.

1. **`backfill_acceptance_criteria`** into DevCouncil — pure logic, no model, and
   it completes the requirements work already landed.
2. **`domain` schemas** — gap, evidence, critique finding — with the same
   pydantic cross-checks used for `Requirement`.
3. **`storage`**: the tables those schemas need.
4. **Schema-constrained completion in MANVI** (D2's prerequisite; useful well
   beyond the council).
5. **The council**, split per D2.
6. **The MCP server surface**, so the whole thing is drivable by any agent and
   can finally be dogfooded. Arguably belongs earlier — bring it forward if
   dogfooding matters more than depth.
7. **Verification**, differential-measured before anything is replaced.
8. Everything else, by whatever dogfooding says hurts most.

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
- **A component quietly becoming harness-only.** The failure this document's own
  correction was heading for: absorbing a DevCouncil subsystem into MANVI reads
  as progress — tests pass, the capability works — while removing the thing that
  made it a building block. The check is §5 of
  [`COMPONENTS_AND_HARNESS.md`](COMPONENTS_AND_HARNESS.md), and the sharpest
  question in it is the last: *can an agent that is not MANVI use this?* If the
  answer is no, it was not ported, it was consumed.
- **Two sources of truth for one component.** D1. Sources are duplicated between
  the repositories with nothing detecting drift, and one change has already been
  mirrored by hand.
