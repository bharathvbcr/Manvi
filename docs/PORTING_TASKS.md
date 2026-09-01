# DevCouncil Port — Work Packages

For engineers picking up a piece of the port. Every task here is meant to be
assignable to someone who has not seen the codebase before.

**Read first, in this order:**

| Document | Answers |
|---|---|
| [`COMPONENTS_AND_HARNESS.md`](COMPONENTS_AND_HARNESS.md) | What a component is, the contract it satisfies, which language it belongs in |
| [`DEVCOUNCIL_PORT_ROADMAP.md`](DEVCOUNCIL_PORT_ROADMAP.md) | What is done, what is left, open decisions |
| [`KNOWN_GAPS.md`](KNOWN_GAPS.md) | Every known defect and gap — **including bugs in the Python you must not faithfully reproduce** |
| This file | What to actually do, how to prove it works |

---

## 1. The goal

DevCouncil becomes a set of **building blocks** — binaries with a JSON-on-stdio
contract, plus an MCP server — usable by any coding agent. MANVI is one consumer;
Claude Code and Cursor are others. MANVI is the harness that unifies them.

**The one-sentence test for any piece of work:**

> Can an agent that is not MANVI use this?

If the answer is no, the thing was **consumed, not ported**. That failure looks
exactly like success from the inside — tests pass, the capability works — which
is why it is the first question, not the last.

**Programme done means:** no Python remains in `src/devcouncil/`, every capability
is reachable as a component, and MANVI drives all of them through the process
boundary with nothing linked.

---

## 2. Ground rules

Non-negotiable. A change that breaks one of these is rejected regardless of what
else it does.

1. **No new Python, and no Python clients.** Port a component and cut its
   consumers over. Do not wrap a component in a Python shim with a fallback —
   [DevCouncil already did that](DEVCOUNCIL_PORT_ROADMAP.md) and the Rust path
   never executed once, for months, because the fallback hid it.
2. **A component is a process, never a library MANVI links.**
3. **`CGO_ENABLED=0` for every shipped Go build.** cgo is enabled only for the
   race detector.
4. **No new dependency without asking.** The Go module has **zero** third-party
   dependencies. The Rust workspace has three, each justified in its manifest.
5. **A check that could not run never reports what a check that ran and passed
   reports.** This is the cardinal rule and most of §3 is instances of it.
6. **No placeholders in delivered code.** No `TODO`, no stub returning a fake
   value. Ship it complete or say what is blocked.
7. **Do not weaken a test to make a change pass.** Fix the code, or explain why
   the test encoded the wrong expectation.
8. **Edit components in DevCouncil, mirror to MANVI** until [D1](#7-open-decisions)
   is resolved.

---

## 3. Cautions

Each of these is a real incident from this port, not a hypothetical. Read them
before writing code; every one cost time and two of them were live defects that
had been reporting success.

### C1 — A skipped check that reports `ok`

**Found twice.** `dc-store`'s interop tests located DevCouncil by counting three
parent directories. From a git worktree that path lands somewhere else, so the
tests took the "not found" branch, printed to stderr, and returned — and cargo
printed `ok`. Stderr from a passing test is hidden without `--nocapture`, so
**three checks that never ran were invisible in every summary**, in both
repositories, for different path reasons.

**What to do:** when porting a gate, port its failure mode first. A check that
cannot run must fail, or skip *visibly and by explicit opt-in*. The pattern is
`DC_STORE_REQUIRE_INTEROP=1`: unset it skips loudly, set it turns the skip into a
failure so CI can demand the evidence.

### C2 — Python defaults versus Go zero values

DevCouncil declares `required: bool = True`. Pydantic omits defaults when it
serialises. Go zeroes an absent bool to `false`. Decoded with a plain struct tag,
**every acceptance criterion whose producer omitted the key silently became
optional** — still listed, still looking checked, no longer something the work
had to satisfy.

**What to do:** every pydantic field with a default is this hazard. Decode by hand
with a pointer to distinguish absent from explicit, and test against
`model_dump_json(exclude_defaults=True)` — the hostile case where every defaulted
key leaves the wire. See `dc/requirement.go` and `dc/requirement_interop_test.go`
for the worked example.

### C3 — Porting a weaker implementation and calling it a port

`dc-verify`'s `detect_stubs` is substring matching. DevCouncil's `stub_detector.py`
parses the AST. The Rust function has the same name and does materially less.

**What to do:** measure before replacing. A name matching is not evidence the
behaviour does. If your port is weaker, say so in the ledger and leave the
incumbent wired.

### C4 — Absent versus empty

`dc-store` created `requirement_ids_json` and selected it never, so every task
crossed the boundary with **no** requirements — not an empty list, absent. A
coverage gate reading that reports a task accountable to no requirement exactly
as it reports one accountable to all of them.

**What to do:** emit the key even when empty. A consumer must be able to tell "no
requirements" from "this component does not report requirements."

### C5 — Two sources of truth

The crate sources exist in both repositories with nothing detecting drift. The
first change after the port had to be applied twice by hand and stayed consistent
only because one person did both halves in one sitting.

**What to do:** until [D1](#7-open-decisions) lands, every component change is two
commits — DevCouncil first, then the MANVI mirror — and the mirror commit must
say what it mirrors.

### C7 — Porting a Python bug faithfully

`rust-port/DIVERGENCES.md` records **28** places where the Rust port deliberately
behaves differently from the Python, because the Python is wrong. A porter reading
the Python as a specification will reproduce all of them.

The sharpest cluster — G5, G7, X14, V12 and X6/X7 — is one defect wearing five
coats: **the Python answers confidently where it does not know**, and X6/X7 goes
further and fabricates symbols from source it could not parse.

**What to do:** before porting any subsystem, check
[`KNOWN_GAPS.md` §4](KNOWN_GAPS.md#4-python-baseline-defects-the-port-must-not-reproduce).

### C6 — Building a second engine

`dc-grep` links ripgrep's own crates rather than shelling out to `rg` or
reimplementing ignore resolution. `devmap` links 32 tree-sitter grammars.

**What to do:** before writing a parser, a matcher, or a walker, check whether one
of the existing components already has the machinery. Adding a second one means
keeping it in step forever.

---

## 4. Testing protocol

**Mandatory for every task.** A task is not reviewable without these.

### 4.1 The test comes first, and must fail

Write the test against the **unmodified** code and record how it fails. A test
that has never failed is not evidence. For a new field or type this is usually a
compile error — that counts, and paste it.

### 4.2 Red-demonstrate the gate

After it passes, **break something and show the test catches it**, then restore.
Paste both outputs. Worked examples from this port:

- Hid `.venv`, set `DC_STORE_REQUIRE_INTEROP=1` → three interop tests FAILED with
  "agreement is UNPROVEN" instead of reporting `ok`.
- Removed `devmap` from `PATH` → the live-contract tests reported `--- SKIP`, not
  `--- PASS`, proving the earlier PASS meant the binary ran.
- Dropped `llm_review` from Go's accepted set → the enum parity test failed,
  naming the member.

### 4.3 Cross-language parity for anything shared

Two patterns, both already in use. Use one; do not invent a third.

- **A generated fixture both sides read.** `rust/testdata/fnmatch-parity.tsv` is
  775 CPython-generated cases that `dc-glob` and `manvi/internal/fnmatch` both
  check against. If they drift, one fails.
- **A test that interrogates the other implementation directly.**
  `dc/requirement_interop_test.go` reads the `verification_method`, `priority` and
  `source` members out of DevCouncil's Python `Literal`s and fails if Go refuses
  one. This is how a member added on one side surfaces immediately rather than in
  production.

### 4.4 Interop must fail, not skip, when demanded

While Python and the port coexist, every component gets an interop test that
drives both sides against the same state. It must be gated so CI can require it:

```bash
DC_STORE_REQUIRE_INTEROP=1 cargo test --workspace
```

### 4.5 Run the real gates

`verify.sh` is the authority — **33 gates** including format, vet, lint, coverage,
dependency surface, known vulnerabilities, cross-language store schema, Python
interop, parity fixtures, and fuzz-target reachability.

```bash
./verify.sh
```

```bash
./verify.sh --race --fuzz
```

Component-level, while iterating:

```bash
cd rust && DC_STORE_REQUIRE_INTEROP=1 cargo test --workspace && cargo clippy --workspace --all-targets
```

```bash
cd manvi && go build ./... && go vet ./... && gofmt -l . && go test ./...
```

### 4.6 Report honestly

State counts before and after, name what you did not verify, and never report a
skipped gate as passed. "I could not verify this" is a complete answer.

---

## 5. Definition of done

Every task. Copy this into the PR description and tick it.

- [ ] The component is a binary or MCP tool meeting the [§1 contract](COMPONENTS_AND_HARNESS.md#1-the-contract-a-component-satisfies) — not a library MANVI links.
- [ ] `health` names it and its schema version.
- [ ] Every failure path emits JSON on **stdout**; nothing panics to stderr with an empty stdout.
- [ ] A negative or contended result is a normal answer with a `code`, not an error.
- [ ] Empty is emitted as empty, never omitted (**C4**).
- [ ] A check that could not run is distinguishable by the caller from one that ran and passed, **with a test proving it** (**C1**).
- [ ] Every call is bounded: timeout, payload ceiling, own process group.
- [ ] Cross-language interop tested while both implementations exist, failing rather than skipping under its env gate (**§4.4**).
- [ ] Shared schemas have a parity gate (**§4.3**).
- [ ] Every pydantic default checked against `exclude_defaults=True` (**C2**).
- [ ] The test failed against unmodified code, and the gate was red-demonstrated (**§4.1–4.2**).
- [ ] MANVI resolves it via `toolBinary` with an env override, degrading visibly when absent.
- [ ] It does not import, reference, or assume MANVI.
- [ ] `./verify.sh` passes.
- [ ] Mirrored to the other repository, if it touches shared component sources (**C5**).
- [ ] [`KNOWN_GAPS.md`](KNOWN_GAPS.md) checked for gaps touching this subsystem, and §4 checked for Python defects not to reproduce (**C7**).
- [ ] Any new gap found is added to `KNOWN_GAPS.md` with a VERIFIED/RECORDED label.
- [ ] The ledger (`rust/STATUS.md` §7 or equivalent) records what was verified and what was not.

---

## 6. The tasks

Sizes are **unmeasured** estimates from reading the Python. Python LOC is the
source subsystem's size, not a prediction of the port's.

### Wave 0 — Infrastructure (do first; everything else is safer after)

| ID | Task | Lang | Depends | Size |
|---|---|---|---|---|
| **P0.1** | **Resolve D1: stop the two source copies drifting.** Add a checked-in digest of the component sources so a stale MANVI mirror is a test failure. See [D1](#7-open-decisions). | Shell/CI | — | S |
| **P0.2** | **CI for both workspaces.** Neither `rust/` nor `backend/go_orchestrator` is in `.github/`. Must set `DC_STORE_REQUIRE_INTEROP=1` so interop evidence is demanded, not hoped for. Must run on **Linux** — everything to date is verified on darwin/arm64 only. | CI | — | M |
| **P0.3** | **Decide and create the Go component home in DevCouncil.** See [D3](#7-open-decisions). Today `backend/go_orchestrator/` holds *client* code mirrored from MANVI, which is harness code living in the component repo. Most remaining work is Go and needs a home that is not that. | — | — | S (decision) |

### Wave 1 — Contract and state (blocks the council, verification, and most MCP handlers)

| ID | Task | Lang | Depends | Size |
|---|---|---|---|---|
| **P1.1** | **Domain schemas.** Port `Gap`, `Evidence`, `Assumption`, `CritiqueFinding`, `CheckpointRefs` (`src/devcouncil/domain/`, 305 lines) with the same treatment `Requirement` got: hand-decoding for defaults, enum validation, and a parity test reading the Python `Literal`s. These are the contract every other component reads, so they lead. | Go (+Rust where a component stores them) | P0.3 | M |
| **P1.2** | **`backfill_acceptance_criteria`.** ~50 lines of pure logic from `planning/plan_service.py`: guarantee every acceptance criterion is owned by exactly one task, preferring a writable task already implementing that requirement. No model needed, trivially testable. `dc.UnownedCriteria` already detects the gap this fills. | Go | P1.1 | S |
| **P1.3** | **`storage`: the 14 remaining tables.** `dcstore` covers `tasks` and `task_leases`; DevCouncil has 16 SQLModel tables. Port requirements, critique findings, gaps, evidence, assumptions, artifact graph, planning state, shell sessions/commands, file-change events, semantic diffs, handoffs, correction manifests, verification runs, project state. **Rust, and in `dcstore`** — splitting one SQLite file across two languages means two writers with different assumptions. | Rust | P1.1 | L |

### Wave 2 — The MCP server (the headline deliverable)

This is what makes DevCouncil a component layer rather than a set of binaries.
Until it exists, no other agent can use any of this.

| ID | Task | Lang | Depends | Size |
|---|---|---|---|---|
| **P2.1** | **Server skeleton.** JSON-RPC over stdio: `initialize`, `tools/list`, `tools/call`, error envelopes, bounded payloads, cancellation. Ship **one** handler end to end (suggest `status`) to prove the shape. MANVI's `manvi/mcp/protocol.go` is the closest reference for the wire format — it is the *client* side of the same protocol. | Go | P0.3 | L |
| **P2.2** | **Handlers: task and lease** — `task`, `lease`, `checkout`, `scope`, `status`, `next_task`, `handoff` (7). Read through `dcstore`. | Go | P2.1, P1.3 | L |
| **P2.3** | **Handlers: verification** — `verify`, `evidence`, `policy`, `cli_gate` (4). Read through `dcverify`. Blocked on P4.1 for anything where Python currently leads. | Go | P2.1, P4.1 | M |
| **P2.4** | **Handlers: code intelligence** — `map`, `graph`, `trace`, `codeintel`, `ast_lsp` (5). Read through `devmap`, which is already Rust — these are thin. | Go | P2.1 | M |
| **P2.5** | **Handlers: filesystem and exec** — `read`, `write`, `run`, `runs`, `git` (5). Gated writes and bounded execution; the write gate is the component-side one from `gating`. | Go | P2.1 | L |
| **P2.6** | **Handlers: knowledge and adjuncts** — `knowledge`, `wiki`, `live`, `prompts`, `provenance`, `router_cache`, `debug`, `tool_specs` (8). Several depend on Wave 5 subsystems; port the handler when its subsystem lands. | Go | P2.1, Wave 5 | L |

### Wave 3 — The council

Placement is [D2](#7-open-decisions) and is **not settled**. The recommendation is
to split on the determinism line: schemas and prompts are DevCouncil components,
execution is MANVI. Confirm before starting P3.2/P3.3.

| ID | Task | Lang | Depends | Size |
|---|---|---|---|---|
| **P3.1** | **Schema-constrained completion in MANVI's `llm/`.** DevCouncil's `router.complete_structured(schema=…, fallback=…)` is what makes the debate machine-readable. `llm.ToolSchema` already carries `InputSchema json.RawMessage`; forcing a single tool call whose input schema *is* the output schema is the standard route and needs no provider-seam change. **Useful far beyond the council.** Per-provider forced-tool-choice details are **unverified**. | Go (harness) | — | M |
| **P3.2** | **Council schemas and prompts as components.** `PlanOutput`, `CritiqueOutput`, `RebuttalOutput`, plus the eight prompts in `src/devcouncil/council/prompts/` (planner_a/b, critic_a/b, rebuttal, arbiter, spec_writer, implementation_reviewer). This is the reusable part — the encoded knowledge of *how to run a council*. | Go (DevCouncil) | P3.1, D2 | M |
| **P3.3** | **The debate protocol.** Two rival planners → critics cross-assigned to the *other* plan → rebuttal → arbiter. **Caution:** DevCouncil degrades an unparseable critique to "no findings" so a weak model cannot crash a run. Port that *and* make it visible — an empty critique and a critique that could not run must not read the same to the arbiter (**C1**). | Go (harness) | P3.2 | L |

### Wave 4 — Verification

| ID | Task | Lang | Depends | Size |
|---|---|---|---|---|
| **P4.1** | **Differential measurement, before porting anything.** Run `dc-verify` and DevCouncil's Python gates over the same corpus of real diffs and record where each wins. §4 of DevCouncil's `rust/STATUS.md` records that Python leads on stub detection; nothing else is measured. **This task produces evidence, not code**, and it gates P2.3 and P4.3. | — | — | M |
| **P4.2** | **Test execution, sandboxing, instrumentation.** `command_runner.py`, `sandbox.py`, `coverage_measurement.py` — process spawning, bounded timeouts, streamed output, process-group cleanup. `manvi/internal/proc` does this shape of work already. | Go | P0.3 | L |
| **P4.3** | **AST-based stub detection, inside `devmap`.** Reach parity with `stub_detector.py` by asking `devmap` — which already links 32 tree-sitter grammars and records byte spans per symbol — rather than writing a second AST parser (**C6**). **Inferred:** no such `devmap` surface exists yet, and whether extraction retains enough of the body is unchecked. Scope that first. | Rust | P4.1 | L |
| **P4.4** | **Acceptance compiler, gap ids, next actions.** Text and logic, low volume. | Go | P1.1 | M |

### Wave 5 — The remainder

All Go, all rule 4 (IO-bound, orchestration, templating). Order by what dogfooding
says hurts most once Wave 2 lands.

| ID | Subsystem | Python LOC | Notes |
|---|---|---|---|
| **P5.1** | `knowledge` | 2,338 | OKF knowledge base, wiki generation. |
| **P5.2** | `reporting` | 1,947 | Evidence bundles, HTML. Go stdlib templating. |
| **P5.3** | `campaign` | 1,700 | Multi-task orchestration; concurrency and supervision. |
| **P5.4** | `live` | 1,212 | Review cards, repair prompts. |
| **P5.5** | `repo` | 870 | CI scaffold, gitignore, SCA. |
| **P5.6** | `telemetry` | 918 | Counters and events. |
| **P5.7** | `optimization` | 998 | GEPA / SkillOpt — rollout→reflect→propose loops around model calls. |
| **P5.8** | `gating` | 692 | The **component-side** write gate. Distinct from MANVI's harness gate; both exist, serving different consumers. |
| **P5.9** | `app` | 1,472 | Config and bootstrap. **Audit first** — MANVI's `flags`/`bootstrap` may cover most of it. |
| **P5.10** | `skills` | 487 | Mostly markdown needing a loader, not a port. |
| **P5.11** | `executors` | 3,765 | **Audit before porting.** Likely obsolete: other agents consume DevCouncil's MCP server rather than being driven by it. Inverting a dependency is not removing a feature — confirm, then delete rather than port. |
| **P5.12** | `cli` | 16,547 | Not one task. Each component's CLI is in that component's language; commands that were only application glue are dropped. Split per component as each lands. |

---

## 7. Open decisions

Blocking the tasks that name them. **None are yours to make alone** — raise them.

- **D1 — MANVI's copy of the component sources.** DevCouncil is upstream.
  Recommendation: keep the mirror with a checked-in digest now (**P0.1**), delete
  it once installing DevCouncil's components is routine. Detail in
  [`DEVCOUNCIL_PORT_ROADMAP.md` §1](DEVCOUNCIL_PORT_ROADMAP.md).
- **D2 — Where the council lives.** It needs model calls (harness side) but its
  prompts and schemas are the reusable part (component side). Recommendation:
  split on the determinism line. **Avoid** putting it wholly in MANVI — that makes
  the council unavailable to every other agent.
- **D3 — The Go component home in DevCouncil.** Most remaining work is Go and
  there is no obvious place for it. `backend/go_orchestrator/` currently holds
  harness-side *client* code mirrored from MANVI. Proposal: `DevCouncil/go/` for
  components, mirroring `rust/`, leaving the mirrored clients where they are or
  retiring them under D1.

---

## 8. Where things are

| | Path |
|---|---|
| DevCouncil Python (the source being ported) | `DevCouncil/src/devcouncil/` |
| DevCouncil Rust components | `DevCouncil/rust/` (`dc-glob`, `dc-grep`, `dc-store`, `dc-verify`) |
| DevCouncil code intelligence | `DevCouncil/rust-port/` (`devmap`, 49,675 lines) |
| DevCouncil Go (today: mirrored clients) | `DevCouncil/backend/go_orchestrator/` |
| MANVI harness | `Manvi/manvi/` |
| MANVI's mirror of the components | `Manvi/crates/` |
| Component ledger | `DevCouncil/rust/STATUS.md` |
| Full gate suite | `Manvi/verify.sh` |
