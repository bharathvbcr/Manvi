# Components and Harness — how MANVI and DevCouncil divide the work

**DevCouncil is the components. MANVI is the unification.**

That sentence decides most of the questions people ask about these two
repositories: where a change belongs, why the boundary is a process rather than
a library, why MANVI can be embedded in an application that has never heard of
DevCouncil, and what has to be true before a newly ported component is allowed
to be depended on.

---

## 1. The division

| | DevCouncil | MANVI |
|---|---|---|
| **Is** | A set of components, each answering one question well | The harness that unifies them into a working agent |
| **Ships** | Standalone binaries, one contract each | One static Go binary, plus `manvi serve` |
| **Owns** | Code intelligence, task/lease state, verification, search — and the planning council as it lands | Turn loop, provider seam, policy ladder, grants, session log, TUI, tool suite |
| **Consumed by** | MANVI, and anything else that speaks JSON on stdio | IDEs, editors, host applications, CI |
| **Depends on** | Nothing in MANVI | Every component, at the process boundary only |

The dependency runs one way. DevCouncil does not know MANVI exists, which is
what keeps its components reusable; a component that had to be told about the
harness would be a harness feature wearing a component's name.

---

## 2. Why MANVI is embeddable, and what that costs the design

MANVI's reason to exist is that a set of good components is not a thing you can
run. Somebody has to drive a turn, decide whether a write is allowed, keep the
model's view of history honest, and hand results back. That is the harness, and
because it is a single static Go binary (`CGO_ENABLED=0`) whose only external
contract is `fork`/`exec` plus line-delimited JSON, it embeds into other
applications without dragging an interpreter, a shared library, or a package
manager behind it.

`manvi serve` is the surface for that: policy enforcement, capability discovery,
token budget preparation and completion settling, over NDJSON on stdio. See
[`SERVE_HOST_PLANE.md`](SERVE_HOST_PLANE.md).

The cost is a rule that has to hold everywhere: **MANVI links no component.**
Not by `cgo`, not by vendoring a crate, not by importing a Python module. The
moment one component is linked, the harness stops being a static binary, stops
cross-compiling instantly, and starts carrying that component's dependency tree
into every application that embeds it. The boundary is what makes the embedding
claim true, so it is not negotiable for convenience.

---

## 3. Component inventory

**Ported — consumed as binaries today.**

| Component | Contract | What MANVI uses it for |
|---|---|---|
| `devmap` | `devmap <cmd>`, JSON on stdout | Code graph, adjacency, dead code, impact; the neighbour rule in the write gate |
| `dcstore` | one JSON object on stdout, always | Tasks, and the lease that makes concurrent building safe |
| `dcverify` | diff on stdin, one JSON object out | Diff parsing, scope classification, rigor gates, diff↔coverage |
| `dcgrep` | JSON on stdout | Ignore-aware repository search on ripgrep's engine |

`dc-glob` is deliberately absent: it is a *library* linked into `dcverify`, has
no binary, and never appears on the boundary. Its Go counterpart is
`manvi/internal/fnmatch`, and the two are pinned to each other by a shared
775-case CPython `fnmatch` parity fixture — the one rule both sides must agree
on, so it is implemented twice on purpose and checked against a third party.

**Not yet ported — still Python in DevCouncil.** Roughly 50k lines, of which the
load-bearing pieces for the harness are the planning council (`planning/`,
`council/`), the deeper verification gates (`verification/`), and the
integrations layer (`integrations/`). Until a subsystem is ported and has a
contract, MANVI does not reach it at all — there is deliberately no Python
fallback path in the harness, because a fallback is what lets a broken primary
path go unnoticed.

**In flight.** The task model now carries `requirement_ids` and
`acceptance_criterion_ids` across the boundary, and `dc.Requirement` /
`dc.AcceptanceCriterion` mirror DevCouncil's model field for field. That is the
substrate the council port needs; the council itself has not moved.

---

## 4. How MANVI resolves a component

One rule, identical for every component, implemented in
`manvi/cmd/manvi/toolbinary.go`:

1. `MANVI_<NAME>_BINARY`, if set. An explicit path always wins.
2. `PATH`. This is the normal case for an installed DevCouncil.
3. A sibling cargo build directory, release before debug. A development
   convenience, not the intended deployment.
4. The bare name, so the failure is a clear `exec` error rather than a silent
   skip.

```bash
# Point the harness at a specific build of a component.
MANVI_MAP_BINARY=/path/to/DevCouncil/rust-port/target/release/devmap manvi doctor
```

The environment variables are `MANVI_MAP_BINARY`, `MANVI_STORE_BINARY`,
`MANVI_VERIFY_BINARY` and `MANVI_GREP_BINARY`.

`manvi doctor` prints which binary each one resolved to and whether it answered,
so "which component am I actually running" is never a guess. A real run, showing
three components resolved from three different rungs of the ladder:

```
  verifier        /Users/…/.local/bin/dcverify
                  reachable — secret_scan, stub_detection, diff_coverage will run
  searcher        /Users/…/crates/target/debug/dcgrep
                  reachable — devcouncil_grep will search, honouring ignore rules
  store           /Users/…/.local/bin/dcstore -> …/.devcouncil/state.sqlite
                  UNAVAILABLE: … did not report its lease exclusion index as
                  "verified" — without the partial unique index on task_id, two
                  builders racing for one task both win
                  lease checks cannot run; writes that need one will be refused
  dev map         index holds no symbols — run `manvi map build`
                  no …/code_graph.json — the scope rung will record repo_map.unavailable
```

Two things worth reading off that output. The verifier came from an installed
component and the searcher from a local build, which is the ladder working as
intended. And the store is an **installed component older than its contract** —
it predates the exclusion-index assertion, so it cannot make the claim the
harness requires. The harness does not shrug and continue: it refuses the writes
that would need a lease. That is the version-skew hazard of §7, caught by the
component's own health check rather than by a corrupted run.

### A missing component degrades loudly, and never silently passes

This is the rule the whole arrangement rests on. When a component cannot be
reached, the gate that needed it reports **`degraded`** — a distinct outcome from
`passed`, rendered differently, and never collapsed into it. `verify.sh` records
such a gate as one that *did not run*, not one that ran and was clean.

Concretely: without `devmap`, the neighbour rule reports `repo_map.unavailable`
and repository navigation is recorded as an unrun gate. Without `dcverify`,
`secret_scan`, `stub_detection` and `diff_coverage` report as degraded rather
than as clean. A check that could not run must never report what a check that
ran and passed reports.

---

## 5. Where a change belongs

Ask what the change is *about*, not which repository is convenient.

| The change is… | It belongs in |
|---|---|
| How a diff is parsed, what counts as a stub, how coverage intersects | DevCouncil (`dcverify`) |
| What the code graph contains, how symbols resolve | DevCouncil (`devmap`) |
| The task schema, the lease, mutual exclusion | DevCouncil (`dcstore`) |
| How search walks a tree or honours ignore rules | DevCouncil (`dcgrep`) |
| How a turn is driven, a stream cancelled, a tool dispatched | MANVI |
| Whether a write is allowed, how a grant is recorded | MANVI (`gate`, `policy`, `grants`) |
| How a result is rendered, logged, or replayed | MANVI (`ui`, `session`) |
| What the harness *asks* a component for | MANVI's client (`manvi/dc/...`) |

The last row is the one that is easy to get wrong. `manvi/dc/store`,
`manvi/dc/devmap` and `manvi/dc/dcgrep` are **clients** — they transport
answers, they do not compute them. A client that started deciding whether a
lease is valid, or caching one, would be re-implementing the component badly and
on the wrong side of the boundary. A cached lease is a lease that has already
expired somewhere else.

---

## 6. Porting a component: the checklist

DevCouncil is mid-port. Each subsystem that crosses from Python to Rust/Go
becomes another binary on the same boundary, and MANVI's shape does not change
when it does. A component is ready to be depended on when all of these hold:

1. **One JSON object on stdout, including on failure.** A caller must never have
   to parse prose or infer from an exit code alone. The exit code is a coarse
   duplicate, for shell use.
2. **Expected outcomes are not errors.** Contention, a missing task, an unknown
   id — these exit 0 with a code in the payload. A caller that has to
   distinguish "busy" from "broken" by reading stderr will eventually get it
   wrong.
3. **A check that could not run is distinguishable from one that ran clean.**
   Empty results and unavailability must not share a representation.
4. **A `health` subcommand that asserts identity**, so a caller can confirm it is
   talking to this component and not to some other program that prints JSON.
5. **Bounded everything** — payload size, nesting depth, timeouts — since input
   crosses a trust boundary.
6. **A live-contract test on the MANVI side** that drives the real binary rather
   than a fake. A fake asserts only that the test and the code were written by
   the same hand; the field names are a contract with a producer built from
   another repository. `manvi/dc/devmap`'s `TestTheLive*` tests are the pattern:
   they skip visibly when the binary is absent, and a skip must never read as a
   pass.
7. **Resolution wired into `toolBinary`** with a `MANVI_<NAME>_BINARY` override,
   and the degraded path proven — the gate must report `degraded`, not `passed`,
   when the binary is removed.

Point 6 has teeth: verify it by removing the binary from `PATH` and confirming
the test reports `SKIP` rather than `PASS`. A live test that passes without the
producer is not testing the producer.

### Before retiring the Python you ported from

A separate phase, and the one with a trap in it. Some of MANVI's Go is a *port*
of DevCouncil Python rather than a client of a component, and where it is, a
**parity fixture** holds the two in step. That fixture is generated by importing
the Python — so porting the Python to Rust/Go breaks the generator, and breaks
it silently.

One such coupling exists today:

| Fixture | Cases | Generated from | At risk |
|---|---|---|---|
| `testdata/command-parity.tsv` | 256 | `scripts/gen-command-parity.py`, which imports `devcouncil.execution.policy_engine.TaskPolicyEngine`, `normalize_allowlist_command`, and `devcouncil.domain.task.Task` | **Yes** — when `execution/policy_engine.py` is ported |
| `testdata/fnmatch-parity.tsv` | 775 | `scripts/gen-fnmatch-parity.py`, which imports **CPython's** `fnmatch` | No — CPython is not being ported |

The asymmetry is the point: only the fixture whose source of truth is *being
replaced* is exposed. Do not apply this to the other one.

**Why it fails quietly.** Nothing imports the generator at build or test time.
`TestCommandParityWithPythonEngine` reads the committed `.tsv`, so it keeps
passing against a file that is now a snapshot of an implementation nobody runs
any more. The gate stays green while the thing it was comparing against no
longer exists, which is the exact shape this repository refuses everywhere else
— a check that cannot run must not report what a check that ran and passed
reports.

**A complication that rules out the easy answer.** The fixture is not purely
generated: its header records **three rows applied by hand after generation**,
where this harness deliberately decided against the incumbent's behaviour (the
Python engine normalises any absolute `…/.venv/bin/dev` to a bare `dev`). Any
regeneration drops them, and nothing re-applies them automatically — so
"regenerate against the new implementation" silently reverts three considered
decisions unless someone remembers.

**So, before the Python `policy_engine.py` is deleted**, one of:

1. **Repoint the generator** at the new Rust/Go implementation and regenerate —
   *then re-apply the three divergence rows*, which the file's own header names.
   Cheapest, and it keeps the fixture meaning "these two agree", but it leaves
   the deliberate differences encoded as comments in a data file, which is how
   they get lost the second time.
2. **Freeze the baseline and open a divergence ledger.** The fixture becomes a
   record of what the *incumbent* decided, and every deliberate difference is
   written down as a ledger entry rather than a comment. `rust-port/` already
   did exactly this — see its `DIVERGENCES.md` and `tools/parity/parity_harness.py`
   — so the pattern is in the repository rather than something to invent. The
   three hand-applied rows are already a divergence ledger in everything but
   name, which is the argument for making it one.

What is **not** acceptable is deleting the Python and leaving the fixture. It
would then assert agreement with nothing, and the first divergence in the
command gate — a command the incumbent denied that the port allows — would
arrive with no test able to notice.

Whoever takes the policy subsystem owns this decision; it is not the harness's
to make, because the harness only consumes the outcome.

---

## 7. Working across the two repositories

The components and the harness are separate repositories with no build-time
relationship, which is the intended coupling — but it means nothing fails when
they drift.

- **Edit a component in DevCouncil.** Build it, put it on `PATH` (or point
  `MANVI_<NAME>_BINARY` at it), then run MANVI's live-contract tests for that
  component. Those tests are the only thing that will notice a contract change.
- **A contract change is a breaking change** for every consumer, not only MANVI.
  Version the payload — `dcverify` carries `schema_version`, `devmap` a store
  `user_version` — and fail closed on an unknown one rather than guessing.
- **The `dc-*` crate sources currently exist in both repositories.** DevCouncil
  is the component home and therefore upstream; MANVI's `crates/` copy is a
  development convenience that lets the harness be built and tested without a
  DevCouncil checkout. It is a duplication with a known end: when DevCouncil's
  components are installed as a matter of course, MANVI's copy should go, and
  `toolBinary`'s step 3 becomes the only thing that needs it. Until then, edit in
  DevCouncil and mirror.
