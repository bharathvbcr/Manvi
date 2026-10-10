# DevCouncil Port — Work Packages

> **Status (2026-09):** DevCouncil's Python package (`src/devcouncil/`) has been
> deleted. Paths and commands below that name `src/devcouncil/`, `dev map query`,
> `dev graph query`, or `uv run pytest tests/unit/test_codeintel` are historical.
> The live engines are the `devmap` CLI and DevCouncil/Manvi Go+Rust components.
> Do not install or `import devcouncil` as a Python package.
>
> **Re-baselined 2026-10-10** against DevCouncil's Phase 7 decisions
> (`DevCouncil/docs/PHASE7_LONG_TAIL.md`, `docs/TODO.md`; DevCouncil at the pin
> `acb5e3b9`). §6.0 gives the status of every task; §7 gives the decisions as
> settled. The task text below is the original brief and is kept as the record
> where §6.0 does not say otherwise.

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
2. **A component is a process, never a library MANVI links** — with one
   exception, below.
3. **One in-process archive, and only one.** Shipped binaries link the Gusset
   umbrella (DevCouncil `rust/gusset-engine`), whose dc-glob engine makes
   every policy pattern decision; Linux builds are fully static against musl.
   Nothing else is linked: every other component stays a process. The Go
   module still builds and is gated `CGO_ENABLED=0` (verify.sh's default
   leg), which is the build that decides with Go's fnmatch. Amended
   2026-09-28, when policy decisions moved onto the engine; before that the
   rule was `CGO_ENABLED=0` for every shipped build.
4. **No new dependency without asking.** The Go module has **zero** third-party
   dependencies. The Rust workspace has three, each justified in its manifest.
5. **A check that could not run never reports what a check that ran and passed
   reports.** This is the cardinal rule and most of §3 is instances of it.
6. **No placeholders in delivered code.** No `TODO`, no stub returning a fake
   value. Ship it complete or say what is blocked.
7. **Do not weaken a test to make a change pass.** Fix the code, or explain why
   the test encoded the wrong expectation.
8. **Edit components in DevCouncil only.** `crates/dc-*` are symlinks into
   DevCouncil (D1, settled): there is no mirror to update, and the pin in
   `scripts/module-pins.txt` is what a release builds.

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
cannot run must fail, or skip *visibly and by explicit opt-in*. The `dc-store`
interop tests used an opt-in switch for this, `DC_STORE_REQUIRE_INTEROP=1`, which
turned the skip into a failure. It is retired: the Python it compared against is
deleted, nothing reads the switch (`DevCouncil/rust/README.md:245`), and setting it
changes nothing. The pattern is what survives, not the variable.

### C2 — Python defaults versus Go zero values

DevCouncil declares `required: bool = True`. Pydantic omits defaults when it
serialises. Go zeroes an absent bool to `false`. Decoded with a plain struct tag,
**every acceptance criterion whose producer omitted the key silently became
optional** — still listed, still looking checked, no longer something the work
had to satisfy.

**What to do:** every pydantic field with a default is this hazard. Decode by hand
with a pointer to distinguish absent from explicit, and test against
`model_dump_json(exclude_defaults=True)` — the hostile case where every defaulted
key leaves the wire. See `dc/requirement.go` (in `DevCouncil/backend/go_orchestrator/`) for the worked
example. The pydantic cross-check is retired with the Python; `dc/requirement_test.go`
owns the wire shape now.

### C3 — Porting a weaker implementation and calling it a port

`dc-verify`'s `detect_stubs` was substring matching while DevCouncil's
`stub_detector.py` parsed the AST, with the same name and materially less
behaviour. **Resolved for stub detection:** the AST detector landed in DevCouncil
`c6b48a28` (Java and C# in `9b0b9e5c`), both in the pin
(`rust/dc-verify/src/stub_ast.rs`), and `tests/false_accept.rs` pins false accepts
and false positives at zero.

**What to do:** the principle stands. Measure before replacing. A name matching is
not evidence the behaviour does. If your port is weaker, say so in the ledger and
leave the incumbent wired.

### C4 — Absent versus empty

`dc-store` created `requirement_ids_json` and selected it never, so every task
crossed the boundary with **no** requirements — not an empty list, absent. A
coverage gate reading that reports a task accountable to no requirement exactly
as it reports one accountable to all of them.

**What to do:** emit the key even when empty. A consumer must be able to tell "no
requirements" from "this component does not report requirements."

### C5 — Two sources of truth *(closed by D1)*

The crate sources used to exist in both repositories with nothing detecting drift.
`crates/dc-*` are now symlinks into DevCouncil, so there is one copy. What can
still drift is the working checkout against the pin: `scripts/check-release.mjs`
`crateMirrorProblems` and `manvi/devcouncil/crate_pins_test.go` catch that.

**What to do:** change components in DevCouncil, move the pin in
`scripts/module-pins.txt`, and let the two checks say whether the workspace
values still agree.

### C7 — Porting a Python bug faithfully

`DevCouncil/docs/devmap/DIVERGENCES.md` records **28** places where the Rust port deliberately
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

- Hid `.venv` and demanded the interop evidence → three interop tests FAILED with
  "agreement is UNPROVEN" instead of reporting `ok`. (Historical: that suite is
  retired with the Python it drove.)
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
  `dc/requirement_interop_test.go` used to read the `verification_method`,
  `priority` and `source` members out of DevCouncil's Python `Literal`s and fail
  if Go refused one. It is retired with the Python (it now holds one explicit
  retirement marker); reintroduce it only when a second producer of the same JSON
  exists.

### 4.4 Interop must fail, not skip, when demanded

**Retired for `dc-store`** (2026-10): the Python it was compared against is
deleted, the opt-in switch is read by nothing, and the Rust suite covers lease
behaviour (`dc-store/tests/leases.rs`). The rule survives for any future component
with a second implementation: an interop test must be gated so a harness can demand
it, and a demanded test that cannot run must fail, not skip.

### 4.5 Run the real gates

`verify.sh` is the authority — format, vet, lint, coverage, dependency surface,
known vulnerabilities, cross-language store schema, parity fixtures, and fuzz-target
reachability (run it to see the current gate list; it was 33 gates before the
Python interop gate was retired, and has not been recounted).

```bash
./verify.sh
```

```bash
./verify.sh --race --fuzz
```

Component-level, while iterating:

```bash
cd crates && cargo test --workspace && cargo clippy --workspace --all-targets
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
- [ ] Cross-language interop tested while two implementations exist, failing rather than skipping when demanded (**§4.4**; none exist today).
- [ ] Shared schemas have a parity gate (**§4.3**).
- [ ] Every pydantic default checked against `exclude_defaults=True` (**C2**).
- [ ] The test failed against unmodified code, and the gate was red-demonstrated (**§4.1–4.2**).
- [ ] MANVI resolves it via `toolBinary` with an env override, degrading visibly when absent.
- [ ] It does not import, reference, or assume MANVI.
- [ ] `./verify.sh` passes.
- [ ] The pin in `scripts/module-pins.txt` moved, if it changes shared component sources (**C5**).
- [ ] [`KNOWN_GAPS.md`](KNOWN_GAPS.md) checked for gaps touching this subsystem, and §4 checked for Python defects not to reproduce (**C7**).
- [ ] Any new gap found is added to `KNOWN_GAPS.md` with a VERIFIED/RECORDED label.
- [ ] The ledger (DevCouncil `rust/STATUS.md` or equivalent) records what was verified and what was not.

---

## 6. The tasks

Sizes are **unmeasured** estimates from reading the Python. Python LOC is the
source subsystem's size, not a prediction of the port's.

### 6.0 Status at 2026-10-10

Status values: **done**, **retired** (decided away by Phase 7), **superseded**,
**partial**, **open**, **unaudited**. "Row" is a line in
`DevCouncil/docs/PHASE7_LONG_TAIL.md`. Rows I read in source are marked *read*;
rows taken from a DevCouncil ledger without checking are marked *ledger*.

| ID | Status | Evidence |
|---|---|---|
| **P0.1** drift between source copies | **Done**, by D1 option 3 (consume by path) rather than the digest proposed here | `crates/dc-*` symlinks; `manvi/go.mod` replace; `scripts/module-pins.txt`; `check-release.mjs` `crateMirrorProblems`; `manvi/devcouncil/crate_pins_test.go` (*read*) |
| **P0.2** CI for both workspaces, on Linux | **Done** for Linux and macOS; **open** for Windows (`ft-7fb7af56`). The interop switch is retired, not "demanded by CI". | `.github/workflows/verify.yml` matrix `[ubuntu-latest, macos-latest]` (*read*); `DevCouncil/rust/README.md:245` |
| **P0.3** Go component home | **Done**, by D3 | `DevCouncil/backend/go_orchestrator/`; `DevCouncil/go/` does not exist (*read*) |
| **P1.1** domain schemas | **Partial.** Only `Gap` has a Go type (`devcouncil/verify/types.go:17`); no `Evidence`, `Assumption`, `CritiqueFinding` or `CheckpointRefs`. The pydantic-parity part is **dropped** with the Python. | search over both repositories (*read*); `dc/requirement_interop_test.go` retirement marker |
| **P1.2** `backfill_acceptance_criteria` | **Open, and has no input.** No production code writes `tasks` or `requirements` rows. Linked to DevCouncil `TASK-P7-11` / `GAP-P7-NO-TASK-CREATE` (`docs/TODO.md:26`), board card `ft-aeb4a53f`, not filed alone. | `rg -uu 'INTO tasks'` and `'INTO requirements'`: every hit a test (*read*, per the audit that opened this task, not re-run here); `dcstore` has `task`/`ready`/`list`, no create |
| **P1.3** storage tables | **Done as transcription** (`PHASE7_LONG_TAIL.md:26`); schema-only tables are `ft-46ab2b` | `rust/dc-store/src/schema.rs` creates 18 tables (*read*) |
| **P2.1** MCP server skeleton | **Done** in the Go host | `cmd/devcouncil/main.go:70` (*read*) |
| **P2.2** task and lease handlers | **Done in part:** `checkout_task`, `renew_lease`, `release_task`, `next_task` are among the host's 8 tools; `handoff` and `status` are not tools | `devcouncil/registry.go` (*read*) |
| **P2.3** verification handlers | **Done:** `verify_task`, `get_gaps`, `policy_check_write` | `devcouncil/registry.go` (*read*) |
| **P2.4** code-intelligence handlers | **Superseded:** served by `devmap`'s own MCP server, not by Go handlers | `docs/TODO.md` `GAP-P7-DEVMAP-MCP-CLI` (*ledger*) |
| **P2.5** filesystem and exec handlers | **Superseded:** MANVI's native registry, the "thick successor" | `PHASE7_LONG_TAIL.md:79` |
| **P2.6** knowledge and adjuncts | **Retired** with their subsystems | rows 11-17 |
| **P2.7a-d** call-graph blackout | **Done** per the 2026-10-06 note below, with a Ruby residue (`DevCouncil` board `ft-6c5d9e9e`) | `KNOWN_GAPS.md` GAP-1 |
| **P3.1** schema-constrained completion | **Absent, and the plan was wrong:** `llm.Request` has no tool-choice field, so this needs a new field in all four adapters, not "no provider-seam change". Open only if D2 is "council in MANVI". | `manvi/llm/provider.go` (*read*); `rg -uu -i 'toolchoice\|tool_choice' manvi` finds nothing |
| **P3.2, P3.3** council schemas, prompts, protocol | **Absent; hard-cut.** Successor named as "Manvi `llm/` + agents" but no code exists in either repository. Decision D2. | `PHASE7_LONG_TAIL.md:22,56-57` |
| **P4.1** differential measurement | **Moot.** The Python it measured against is deleted; the AST detector it motivated landed. | `stub_ast.rs`, `tests/false_accept.rs` |
| **P4.2** test execution, sandbox, instrumentation | **Partial.** Command running is in Go (`verify/commands.go`). Sandboxing is deliberately `local` only: `verify.ParseSandbox` refuses the rest, and a sandbox is a separate DevCouncil decision (`PHASE7_LONG_TAIL.md`, "Isolated verification sandbox"). Coverage instrumentation was not audited. | `devcouncil/verify/{commands,cli}.go` (*read*, not line by line) |
| **P4.3** AST stub detection | **Done**, in `dc-verify` rather than inside `devmap`; Rust, Go, Python and TypeScript get the full rigor checks, Java and C# placeholders only | `rust/dc-verify/src/stub_ast.rs:1-27` (*read*); DevCouncil `c6b48a28`, `9b0b9e5c` both ancestors of the pin |
| **P4.4** acceptance compiler, gap ids, next actions | **Present, not audited against the Python:** `criteria.go`, `gapid.go` (`StableGapID`), `next_actions.go` (`NextActionFor`, `BuildNextActions`) | `devcouncil/verify/` (*read*: file and function names only) |
| **P5.1** knowledge | **Retired** | `PHASE7_LONG_TAIL.md:11` |
| **P5.2** reporting | **Retired** | `:12` |
| **P5.3** campaign | **Retired** | `:14` |
| **P5.4** live | **Retired** | `:15` |
| **P5.5** repo / SCA | **Retired**; no successor (GitPulse runs audit tools) | `:28` |
| **P5.6** telemetry | **Retired** | `:13` |
| **P5.7** optimization | **Retired** | `:17` |
| **P5.8** gating | **Done** | `devcouncil/gating/policy.go` (*read*) |
| **P5.9** app | **Unaudited** | — |
| **P5.10** skills | **Done** | `devcouncil/skills/` (*read*) |
| **P5.11** executors | **Retired** | `:30` |
| **P5.12** cli | **Done**: the Go host's `commands` map | `cmd/devcouncil/main.go:69-93` (*read*) |

### Wave 0 — Infrastructure (do first; everything else is safer after)

| ID | Task | Lang | Depends | Size |
|---|---|---|---|---|
| **P0.1** | **Done (D1, option 3).** Original brief (a checked-in digest of the sources) superseded: the sources are one copy behind symlinks, pinned by `scripts/module-pins.txt`. See §6.0. | Shell/CI | — | S |
| **P0.2** | **Linux and macOS done; Windows open.** Original brief superseded: `verify.yml` runs `verify.sh` on ubuntu and macOS, and the interop switch it was to set is retired. See §6.0. | CI | — | M |
| **P0.3** | **Done (D3).** Original brief superseded: the home is `DevCouncil/backend/go_orchestrator/`. See §6.0. | — | — | S (decision) |

### Wave 1 — Contract and state (blocks the council, verification, and most MCP handlers)

| ID | Task | Lang | Depends | Size |
|---|---|---|---|---|
| **P1.1** | **Domain schemas.** Port `Gap`, `Evidence`, `Assumption`, `CritiqueFinding`, `CheckpointRefs` (`src/devcouncil/domain/`, 305 lines) with the same treatment `Requirement` got: hand-decoding for defaults, enum validation, and a parity test reading the Python `Literal`s. These are the contract every other component reads, so they lead. | Go (+Rust where a component stores them) | P0.3 | M |
| **P1.2** | **Open, no input yet; linked to DevCouncil `TASK-P7-11`.** Original brief: ~50 lines of pure logic from `planning/plan_service.py`, now deleted with the Python. See §6.0. | Go | P1.1 | S |
| **P1.3** | **`storage`: the 14 remaining tables.** `dcstore` covers `tasks` and `task_leases`; DevCouncil has 16 SQLModel tables. Port requirements, critique findings, gaps, evidence, assumptions, artifact graph, planning state, shell sessions/commands, file-change events, semantic diffs, handoffs, correction manifests, verification runs, project state. **Rust, and in `dcstore`** — splitting one SQLite file across two languages means two writers with different assumptions. | Rust | P1.1 | L |

### Wave 2 — The MCP server

**Re-baselined 2026-10-10:** the Go host serves 8 tools and MANVI's native registry
is the thick surface (`PHASE7_LONG_TAIL.md:79`); "29 handlers" is superseded. The
rows below are the original brief; §6.0 has each one's status.

| ID | Task | Lang | Depends | Size |
|---|---|---|---|---|
| **P2.1** | **Server skeleton.** JSON-RPC over stdio: `initialize`, `tools/list`, `tools/call`, error envelopes, bounded payloads, cancellation. Ship **one** handler end to end (suggest `status`) to prove the shape. MANVI's `manvi/mcp/protocol.go` is the closest reference for the wire format — it is the *client* side of the same protocol. | Go | P0.3 | L |
| **P2.2** | **Handlers: task and lease** — `task`, `lease`, `checkout`, `scope`, `status`, `next_task`, `handoff` (7). Read through `dcstore`. | Go | P2.1, P1.3 | L |
| **P2.3** | **Handlers: verification** — `verify`, `evidence`, `policy`, `cli_gate` (4). Read through `dcverify`. Blocked on P4.1 for anything where Python currently leads. | Go | P2.1, P4.1 | M |
| **P2.4** | **Handlers: code intelligence** — `map`, `graph`, `trace`, `codeintel`, `ast_lsp` (5). Read through `devmap`, which is already Rust — these are thin. **Blocked on P2.7a**: without it these handlers expose a confident empty call graph for ~26 languages (**GAP-1**). | Go | P2.1, **P2.7a** | M |
| **P2.5** | **Handlers: filesystem and exec** — `read`, `write`, `run`, `runs`, `git` (5). Gated writes and bounded execution; the write gate is the component-side one from `gating`. | Go | P2.1 | L |
| **P2.6** | **Handlers: knowledge and adjuncts** — `knowledge`, `wiki`, `live`, `prompts`, `provenance`, `router_cache`, `debug`, `tool_specs` (8). Several depend on Wave 5 subsystems; port the handler when its subsystem lands. | Go | P2.1, Wave 5 | L |

#### P2.7 — Close the call-graph blackout *(GAP-1, the largest known defect)*

Split deliberately: **P2.7a is small, urgent and makes the defect honest; the
rest is large and makes it go away.** Do not let the second block the first.

> **Status 2026-10-06 (re-verified against DevCouncil source):** P2.7a is
> **done** — `dead` prices call-blind files, and `impact`/`trace` (and every
> traversal sharing `traverse_walked`, plus scoped `trace A B`) now refuse or
> qualify an answer whose start is in a call-blind file, from the same
> `is_call_blind` predicate. **P2.4 is unblocked on that count.** P2.7b and
> P2.7c have landed as `devmap-extract/src/langcalls/`, controlled by
> `language_capabilities.rs`; COBOL stays refused by `UNSAFE_GRAMMARS`. P2.7d
> resolved as "not applicable, reported unavailable" for HCL and CFML; Nix,
> SQL, shell, Vue, Svelte, Astro and Liquid extract calls. Open residue: Ruby's
> bare receiverless send — see [GAP-1](KNOWN_GAPS.md). The text below is the
> original plan, kept as the record.

**The defect, verified in source.** `extract_node`'s `match lang` in
`devmap-extract/src/treesitter.rs:1578` has exactly **four** language arms —
`python`, `javascript | typescript | tsx`, `rust`, `go`. Everything else falls to
the generic `_` arm, which emits declarations and **no calls**. The C family is
served separately by `extract_c_family_call` (the SC31 fix), which is why it
works and is also the template for this task. Seven of the nine `calls.push`
sites are inside those four arms; the other two are the C-family function and
Rust's macro-body probe.

Measured consequence: **Java, C#, Ruby, Swift and PHP each parse cleanly, emit
their symbols, and produce zero `Calls` edges.** `impact`, `trace`, dead-code and
the PDG then answer for those languages from an empty call graph, with nothing
distinguishing "no callers" from "callers were never extracted."

| ID | Task | Lang | Depends | Size |
|---|---|---|---|---|
| **P2.7a** | **Make the blackout visible.** Derive, from the language dispatch rather than a hand-maintained list, whether a language has call extraction. Where it does not, `impact`, `trace`, dead-code and the PDG must report **unavailable** — not an empty result. Dead code is the urgent surface: a symbol with no extracted calls is today indistinguishable from an unused one, and that is a wrong answer delivered confidently. | Rust | — | M |
| **P2.7b** | **Tier 1 — the five with measured evidence:** Java, C#, Ruby, PHP, Swift. These are confirmed by SC34's per-language control and are the most likely to appear in a real repository. | Rust | P2.7a | L |
| **P2.7c** | **Tier 2 — remaining general-purpose:** Kotlin, Scala, Dart, Lua, Luau, R, Erlang, Solidity, Pascal, COBOL. Solidity already has ownership handling from X30; extend rather than duplicate. | Rust | P2.7b | L |
| **P2.7d** | **Tier 3 — decide whether a call graph is meaningful at all:** HCL, Nix, SQL, shell, Vue, Svelte, Astro, Liquid, CFML. For several of these "a call" may not be a coherent concept. **A reasoned "not applicable, reported unavailable" is a correct outcome here** — what is not acceptable is an empty result that reads as "no calls found". | Rust | P2.7a | M |

**Method — follow SC19 and SC31, do not invent one.** Both closed a
language-coverage question the same way, and it is the reason those closures are
trustworthy:

1. **Write the control first.** A file where one function calls another, in the
   target language, plus an equivalent in a language that already works.
2. **Measure before changing anything** — record the `Calls` count for both. The
   target's is expected to be zero; if it is not, the defect is different from
   what you assumed and the plan changes.
3. **Discover the node kinds from the grammar**, not from memory. The existing
   arms key on `"call"` (Python), `"call_expression"` (JS/TS, Rust, C family),
   `"new_expression"`, `"macro_invocation"`, `"composite_literal"` (Go) and
   `"jsx_opening_element"`. Other grammars name these differently — read the
   grammar's node types rather than guessing, because a wrong kind string is a
   silent no-op that looks exactly like the bug you are fixing.
4. **Re-measure against the control.** The target must reach the comparison
   language's edge count for the same shape, and the comparison language must be
   unchanged.
5. **Then measure on a real corpus** and report the edge-count delta.

**Cautions specific to this task:**

- **C1 applies at corpus scale.** Every wrong answer this produces is a confident
  one. P2.7a exists so the rest can be done incrementally without the interim
  state lying.
- **C6 — do not add a second extraction path.** The C family got a function, not
  a fifth `match lang` arm. Decide early whether each family extends
  `extract_c_family_call`'s shape, joins an existing arm, or needs its own, and
  say why — nine `calls.push` sites is already more than the design wants.
- **A new grammar dependency needs explicit approval** under repository policy.
  Tier 1–3 should need none; every language listed already has a grammar. VB.NET
  does not, and is **out of scope** here (GAP-7).
- **Watch for SC14 collisions.** Adding calls in languages with nested types and
  anonymous callbacks will surface duplicate symbol identities (741 on the
  12,821-file corpus). Expect it; do not paper over it with a dedupe that hides
  which symbol an edge should have attached to.

**Done when:** the per-language control passes for every language in the tier, a
real-corpus edge-count delta is recorded, orphaned call edges remain **0** (the
SC9/SC10 joinability invariant), and no language reports an empty call graph
where it should report unavailable.

### Wave 3 — The council

Placement is [D2](#7-decisions) and is **not settled**, and Phase 7 has narrowed it:
DevCouncil will not own council schemas or prompts in Go (`PHASE7_LONG_TAIL.md:56-57`),
so the options are retired or MANVI-only. Do not start P3.1 to P3.3 before the
owner records the outcome.

| ID | Task | Lang | Depends | Size |
|---|---|---|---|---|
| **P3.1** | **Absent; open only if D2 is "council in MANVI".** Original brief claimed a forced single tool call needs no provider-seam change; that was wrong, `llm.Request` has no tool-choice field. See §6.0. | Go (harness) | — | M |
| **P3.2** | **Council schemas and prompts as components.** `PlanOutput`, `CritiqueOutput`, `RebuttalOutput`, plus the eight prompts in `src/devcouncil/council/prompts/` (planner_a/b, critic_a/b, rebuttal, arbiter, spec_writer, implementation_reviewer). This is the reusable part — the encoded knowledge of *how to run a council*. | Go (DevCouncil) | P3.1, D2 | M |
| **P3.3** | **The debate protocol.** Two rival planners → critics cross-assigned to the *other* plan → rebuttal → arbiter. **Caution:** DevCouncil degrades an unparseable critique to "no findings" so a weak model cannot crash a run. Port that *and* make it visible — an empty critique and a critique that could not run must not read the same to the arbiter (**C1**). | Go (harness) | P3.2 | L |

### Wave 4 — Verification

| ID | Task | Lang | Depends | Size |
|---|---|---|---|---|
| **P4.1** | **Moot.** Original brief (differential run against the Python gates) superseded: the Python is deleted and the AST detector it motivated landed. See §6.0. | — | — | M |
| **P4.2** | **Test execution, sandboxing, instrumentation.** `command_runner.py`, `sandbox.py`, `coverage_measurement.py` — process spawning, bounded timeouts, streamed output, process-group cleanup. `manvi/internal/proc` does this shape of work already. | Go | P0.3 | L |
| **P4.3** | **Done, in `dc-verify`.** Original brief (ask `devmap` instead) superseded: `rust/dc-verify/src/stub_ast.rs` parses with the same grammars `devmap-extract` builds. See §6.0. | Rust | P4.1 | L |
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

## 7. Decisions

- **D1 — MANVI's copy of the component sources: settled, option 3.** Consume by
  path. `crates/dc-*` are symlinks into DevCouncil, `manvi/go.mod` replaces by path,
  and `scripts/module-pins.txt` pins the commit; `check-release.mjs`
  `crateMirrorProblems` and `crate_pins_test.go` catch drift. P0.1 is done. Detail
  in [`DEVCOUNCIL_PORT_ROADMAP.md` §1](DEVCOUNCIL_PORT_ROADMAP.md).
- **D2 — Where the council lives: open, owner to record one of two outcomes.**
  Phase 7 hard-cut `planning/` with the successor "Manvi `llm/` + agents" and says
  model routing and multi-agent runs stay in MANVI (`PHASE7_LONG_TAIL.md:22,56-57`).
  No council code exists in either repository. Either **retired, no successor**
  (P3.1 to P3.3 become retired rows) or **council in MANVI** (file P3.1 to P3.3 as
  features, with P3.1 carrying a test that a forced single tool call returns
  schema-valid JSON on every adapter, because `llm.Request` has no tool-choice
  field yet). The old option of putting it in DevCouncil Go is closed by
  `PHASE7_LONG_TAIL.md:56-57`.
- **D3 — The Go component home: settled de facto.** `DevCouncil/backend/go_orchestrator/`;
  `DevCouncil/go/` was never created. P0.3 is done.

---

## 8. Where things are

| | Path |
|---|---|
| DevCouncil Python (deleted in Phase 7) | `DevCouncil/src/devcouncil/` no longer exists |
| DevCouncil Rust components | `DevCouncil/rust/` (`dc-glob`, `dc-grep`, `dc-store`, `dc-verify`) |
| DevCouncil code intelligence | `DevCouncil/rust/` (`devmap`, 49,675 lines) |
| DevCouncil Go (components, clients, host) | `DevCouncil/backend/go_orchestrator/` |
| MANVI harness | `Manvi/manvi/` |
| Symlinks to DevCouncil's component crates | `Manvi/crates/` |
| Component ledger | `DevCouncil/rust/STATUS.md` |
| Full gate suite | `Manvi/verify.sh` |
