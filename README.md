<img src="assets/manvi-mark.svg" alt="MANVI Mark" width="72" align="left" hspace="12" vspace="4">

# MANVI

A lightweight, high-performance coding-agent harness in pure Go and Rust — designed for local models, customizable for specialized tools and use cases, and featuring additional capabilities from its dual-plane architecture.

**Status: runnable & fully certified.** Kernel, ported policy gates, git safety, override seam, flag registry, immutable session log, turn driver, multi-provider seam, Elm-loop TUI, embedded stdio host server (`manvi serve`), 44 native tools (including a native git integration, an MCP 2.0 client, and a bridge to the external `devcouncil` CLI), and the Go↔Rust process boundary — all verified against **1,031 parity cases** generated from the Python incumbent. Everything is certified by `./verify.sh`.

<br clear="left">

---

## Why MANVI?

MANVI was created as a new lightweight coding harness designed for local models, customizable for other tools and use cases, and featuring additional capabilities from the architecture:

- **Designed for Local Models**: Sub-30ms auto-discovery across local runners (Ollama, LM Studio, vLLM, llama.cpp, Jan), append-only context compaction preserving KV prefix caching (**1.5s warm vs 120s cold prefill on 27B models**), wire-level parser recovery (Hermes, Qwen3 XML), and prefilled `<think>` sanitization.
- **Customizable for Other Tools & Use Cases**: Zero-runtime overhead extension model, native tool execution (44 native tools, MCP 2.0 client, and tool bridges), and an embedded stdio host server (`manvi serve`) for embedding into custom IDEs, editors, and external toolchains.
- **Additional Architectural Capabilities**: Dual-plane process partition (pure Go concurrency + Rust CPU determinism with zero CGO), a 6-rung non-cheating policy ladder with audited override grants, and SQLite-backed multi-agent task concurrency that survives process termination.
- **Tested & Optimized Against Sibling Harnesses**: Empirical testing and experiments with lightweight and local harnesses (such as Pi, Oh My Pi, and Kon) informed key harness optimizations — notably replacing naive 24-step cutoffs with a 500-step ceiling backed by progress stall-cost tracking, eliminating volatile in-memory state, and preventing prefix-cache invalidation.

---

## The Approach

MANVI is built on a handful of deliberate design decisions that shape everything else:

1. **Two planes, split by workload.** IO-bound concurrency (event loops, streaming, cancellation) runs in pure Go (`CGO_ENABLED=0`); CPU-bound determinism (unified diff parsing, coverage intersection, AST indexing, SQLite persistence) runs in Rust. They never link.
2. **Process boundary, not CGO.** Go and Rust communicate exclusively via `fork`/`exec` child processes exchanging line-delimited JSON over stdio. This preserves static binaries, instant cross-compilation, and fault isolation — a crash in the analysis plane cannot take down the execution plane.
3. **Non-cheating invariants.** A check that did not run never reports as passed. A grant that cleared a rule never reports as a clean pass. Blocks are overridable when appropriate, but never invisible.
4. **Mutual exclusion in storage, not application code.** Multi-agent task concurrency is enforced by SQLite's partial unique index (`ON task_leases (task_id) WHERE status = 'active'`) — not by an in-memory lock that dies with the process.
5. **The log is the source of truth.** Model-visible history is *always* projected on demand from the append-only session log; nothing lives only in volatile memory. What the model saw is provably what was logged.

---

## Architecture

### System Overview

```mermaid
flowchart TB
    subgraph UI["User Interface & Host Surfaces"]
        TUI["Fullscreen Elm-Loop TUI"]
        CLI["Headless CLI<br/>manvi run / check / allow"]
        Serve["Host Server<br/>manvi serve (NDJSON stdio)"]
    end

    subgraph GoPlane["Go Execution Plane — CGO_ENABLED=0"]
        Bus["Event Bus & Waterfalls"]
        AgentLoop["Agent Turn Driver"]
        Gate["Policy Gate<br/>Write + Command Ladders"]
        Grants["Grants Ledger"]
        Registry["Flags Registry & Posture Engine"]
        Providers["Provider Seam<br/>Anthropic · Gemini · xAI · Local"]
        Tools["44 Native Tools"]
    end

    subgraph Boundary["Process Boundary — line-delimited JSON over stdio"]
        P1["fork/exec dcstore"]
        P2["fork/exec dcverify"]
        P3["fork/exec devmap"]
    end

    subgraph RustPlane["Rust Analysis Plane"]
        DCStore["dc-store<br/>Tasks & Lease Mutex"]
        DCVerify["dc-verify<br/>Diff Parsing · Rigor Gates · Coverage"]
        DCGlob["dc-glob<br/>Zero-dep fnmatch engine"]
        DevMap["devmap<br/>AST Code Graph"]
    end

    subgraph Storage["Persistent Storage"]
        SQLite[("state.sqlite<br/>partial unique index")]
        CodeGraph[(".devcouncil/code_graph.json")]
        SessionLog[("session log<br/>append-only")]
    end

    TUI --> Bus
    CLI --> AgentLoop
    Serve --> Gate
    Bus --> AgentLoop
    AgentLoop --> Gate
    Gate --> Grants
    Gate --> Registry
    AgentLoop <--> Providers
    AgentLoop --> Tools
    AgentLoop --> SessionLog
    Tools --> P1 & P2 & P3
    P1 --> DCStore
    P2 --> DCVerify
    P3 --> DevMap
    DCStore --> SQLite
    DevMap --> CodeGraph
```

### Plane Responsibilities

| Responsibility | Plane | Package | Rationale |
|---|---|---|---|
| Agent loop, turn driving | Go | `manvi/agent` | Goroutine concurrency, stream cancellation |
| LLM provider seam | Go | `manvi/llm` | HTTP SSE client, multi-provider normalization |
| Policy gates & overrides | Go | `manvi/gate`, `manvi/policy`, `manvi/grants` | Fast in-memory evaluation, origin tracking |
| Terminal UI & telemetry | Go | `manvi/ui` | Event multiplexing, raw termios, damage-diffed rendering |
| Unified diff parsing | Rust | `crates/dc-verify` | CPU-bound text processing |
| Coverage intersection | Rust | `crates/dc-verify` | Line-level coverage bitsets (`-coverprofile`, LCOV) |
| Task & lease persistence | Rust | `crates/dc-store` | `rusqlite`, ACID transactions, exclusion index |
| Glob pattern matching | Both | `crates/dc-glob` ↔ `manvi/internal/fnmatch` | Shared 775-case CPython parity fixture |

---

## How a Turn Executes

Every turn is an evidence-driven state machine: project history → call the model → execute tool calls through the policy gate → log everything → repeat until completion evidence or step ceiling.

```mermaid
sequenceDiagram
    autonumber
    participant AL as Agent Loop (Go)
    participant Log as Session Log
    participant Provider as LLM Provider
    participant Gate as Policy Gate
    participant Tool as Native Tool
    participant Rust as Rust Analysis Plane

    AL->>Log: Append user message record
    loop Each step (until completion evidence or MaxSteps)
        AL->>Log: Project history + assert logged == model-visible
        AL->>Provider: Stream request (SSE)
        Provider-->>AL: Deltas: text / reasoning / tool calls
        alt Model emitted tool calls
            loop For each tool call
                Tool->>Gate: Evaluate write/command policy
                Gate-->>Tool: Passed | Blocked | Granted | Demoted | Degraded
                alt Allowed
                    Tool-->>Rust: JSON over stdio (e.g. verify diff, lease task)
                    Rust-->>Tool: Structured verdict
                    Tool-->>AL: Tool result (+ qualification metadata)
                else Blocked
                    Tool-->>AL: Error: policy violation w/ rule & reason
                end
                AL->>Log: Append tool result record
            end
        else No tool calls (apparent completion)
            AL->>AL: Terminal checkpoint — harness verifies what the turn wrote
            alt Check failed and bounces remain
                AL->>Log: Append harness message carrying the findings
                Note over AL: Turn stays open for one more step
            else Passed, skipped, or bounce budget spent
                AL->>AL: Close, carrying the verdict on the outcome
            end
        end
    end
    AL-->>AL: Turn summary & report
```

### The turn ends on a check, not on a claim

A turn that stops emitting tool calls looks finished. Whether it *is* finished is
decided by the harness, not by the model saying so:

- **The check runs itself.** A mutating turn is verified against the paths its own
  handlers reported writing — not against `git diff`, which also carries whatever
  was uncommitted before the turn began. A turn that changed nothing is skipped,
  and the skip is recorded, so "not owed" stays distinguishable from "did not run".
- **A failure is handed back once.** The findings are appended as a message from
  the harness — marked as such, so no face or resumed session shows them as the
  operator's words — and the turn takes another step. Bounced at most twice; past
  that it closes with `BouncesExhausted` set rather than riding the step ceiling.
- **A check that could not run is never a pass.** Missing verifier, unreadable
  diff, no file list: each reports `degraded`, which the run summary renders as a
  warning rather than a completion.
- **A second opinion is bought on evidence.** Only a turn that has already been
  told and failed again — or one the loop measures as going in circles — dispatches
  an adversarial reviewer, which answers against a fixed contract where a missing
  or self-contradicting verdict counts as *not* passed.

### Tool Execution Pipeline

Every dispatched tool call passes through pre-validation, gate evaluation, body execution, and post-qualification:

```mermaid
flowchart LR
    A["Model emits tool call"] --> B["Pre-execute waterfall<br/>schema + context checks"]
    B --> C{"Gate decision?"}
    C -- "Blocked" --> X["Error back to model:<br/>rule fired + reason"]
    C -- "Allowed" --> D["Execute native tool body"]
    D --> E["Post-execute waterfall:<br/>attach GrantID / posture / degraded marks"]
    E --> F["Log result → model context"]
```

Stateful edits keep before/after hashes and in-memory snapshots, so a failed multi-file edit rolls back instead of leaving the tree half-mutated.

---

## The Policy Engine

### Evaluation Ladder

Operations resolve through six ordered checks. Hard rules are immutable and ungrantable; soft rules can be bypassed by audited grants or harness posture — but every bypass is *marked*, never silent.

```mermaid
flowchart TD
    Op["Operation: file write / command exec"] --> B1{"Within repo root?"}
    B1 -- "No" --> Refuse["Hard refusal — cannot escape workspace"]
    B1 -- "Yes" --> B2{"Hard rules:<br/>credential paths, .git internals,<br/>agent configs, force-push"}
    B2 -- "Matched" --> HardBlock["Block — no grant or posture clears this"]
    B2 -- "Clear" --> B3{"Soft rules:<br/>planned scope, AST neighborhood"}
    B3 -- "Pass" --> Clean["PASSED ✓ clean"]
    B3 -- "Violated" --> B4{"Valid unexpired grant?"}
    B4 -- "Yes" --> Granted["GRANTED ✓ [granted]<br/>carries Grant ID"]
    B4 -- "No" --> B5{"Posture: dev / yolo?"}
    B5 -- "Yes" --> Demoted["DEMOTED ✓ [demoted]"]
    B5 -- "strict" --> B6{"Attended TUI approver?"}
    B6 -- "Yes" --> Prompt["Operator modal → issue human grant"]
    B6 -- "No (CI)" --> Block["BLOCKED ✗"]

    style HardBlock fill:#5b1d1d,color:#fff
    style Block fill:#5b1d1d,color:#fff
    style Clean fill:#1d3a1d,color:#fff
```

### Five Outcome States

```mermaid
stateDiagram-v2
    [*] --> Evaluated
    Evaluated --> Passed : rules satisfied cleanly
    Evaluated --> Blocked : hard rule fired OR ungranted soft rule
    Evaluated --> Granted : soft rule bypassed via audited grant
    Evaluated --> Demoted : soft rule relaxed by posture (dev/yolo)
    Evaluated --> Degraded : check ran without its data/tooling
```

| Outcome | Marker | Semantics |
|---|---|---|
| **Passed** | `✓` green | All rules evaluated and satisfied |
| **Blocked** | `✗` red | Prevented; carries fired rule + reason |
| **Granted** | `✓ [granted]` yellow | Cleared by an explicit recorded override |
| **Demoted** | `✓ [demoted]` amber | Relaxed by `dev`/`yolo` posture |
| **Degraded** | `✓ [degraded]` magenta | Check ran without optional dependencies |

---

## Multi-Agent Concurrency

Leases live in SQLite, so mutual exclusion survives process death. Interrupting a run releases held leases using an uncancelled context — orphaned locks are impossible by construction.

Two bounds decide how wide a fan-out may go, and both answer to the hardware rather
than to intent:

- **A local session delegates nothing.** Not narrowly — at all. Children would
  contend with the parent for the same weights and the same memory, and the
  tightest a fan-out cap can express is "one child at a time", which is still a
  team. The escape is to point the session at a provider with the headroom.
- **The width follows where the children actually run.** A frontier parent placing
  builders on a local model resolves to the frontier width, and then dispatches all
  of them onto one device. The bound is now narrowed by the children's own
  placement, and the narrowing is reported — an operator who set 8 and got 2 is
  told which rule decided it.

```mermaid
flowchart TB
    Planner["Planner — read-only, fast model<br/>explores codebase, drafts tasks"] --> Orchestrator["Orchestrator — high-reasoning<br/>decomposes work, acquires leases"]
    Orchestrator --> Builder1["Builder 1<br/>lease: TASK-001"]
    Orchestrator --> Builder2["Builder 2<br/>lease: TASK-002"]
    Builder1 & Builder2 --> Reviewer["Reviewer<br/>dcverify diff audit + coverage"]
    Reviewer --> SQLite[("state.sqlite<br/>UNIQUE(task_id) WHERE status='active'")]

    style SQLite fill:#26203a
```

---

## Verification & Parity

Porting policy logic across languages fails subtly, not loudly — a glob rule that silently stops crossing `/` separators passes every conventional test. So MANVI generates shared corpora by running the *incumbent* engines, then requires byte-identical behavior from both implementations.

```mermaid
flowchart LR
    CPython["CPython 3.12 fnmatch"] --> G1["gen-fnmatch-parity.py"]
    G1 --> TSV1["fnmatch-parity.tsv<br/>775 cases"]
    TSV1 --> GoF["Go fnmatch"]
    TSV1 --> RustG["Rust dc-glob"]

    PyEngine["DevCouncil TaskPolicyEngine"] --> G2["gen-command-parity.py"]
    G2 --> TSV2["command-parity.tsv<br/>256 cases"]
    TSV2 --> GoP["Go policy engine"]
```

Before any patch is accepted, `dc-verify` runs rigor gates over added lines:

```mermaid
flowchart TD
    Diff["Unified diff"] --> Parse["Parse & validate"]
    Parse -- "Malformed" --> Err["Hard error — fail closed"]
    Parse -- "Valid" --> Gates{"Rigor gates"}
    Gates --> R1["Credential scanner"]
    Gates --> R2["Stub / TODO detector"]
    Gates --> R3["Scope classifier vs planned globs"]
    Gates --> R4["Coverage intersection<br/>added lines × coverprofile/LCOV"]
    R1 & R2 & R3 & R4 --> V{"All pass?"}
    V -- "No" --> Fail["JSON findings with line numbers"]
    V -- "Yes" --> Pass["Verified"]
```

Coverage has three semantic states — **covered**, **uncovered** (ran tests, line never executed), and **unmeasured** (no profile provided). Unmeasured is a warning; it can never masquerade as covered.

---

## Documentation Lookup

The prompt used to tell every run to "verify current documentation" while nothing
here could fetch anything. `manvi/fetch` is the capability behind that instruction,
built on the standard library alone — this module still has **zero dependencies**.

It is in-process on purpose. An out-of-process fetcher (an MCP server, a sidecar, a
hosted crawler) can be gated on the *call* and not on where it then goes; an egress
policy is only enforceable where the socket is opened.

**Off by default.** With no `MANVI_FETCH_HOSTS` the tool is not on the surface at
all — not a tool that refuses, which would cost schema tokens and teach a model to
retry. The allowlist is read from the environment and never from settings: those
load from a file inside the repository, and the rung protecting it is disabled
under a relaxed posture.

```bash
MANVI_FETCH_HOSTS="go.dev,pkg.go.dev,docs.python.org" manvi
```

| Refused, on every request and every redirect hop | Reason |
|---|---|
| Anything but `https` | A position that can rewrite plaintext can write the model's instructions |
| Hosts off the allowlist | Whole-label matching: `go.dev` admits `pkg.go.dev`, not `notgo.dev` |
| Loopback, private, link-local, ULA, multicast, CGNAT, reserved | `169.254.169.254` is cloud metadata; `127.0.0.1` is whatever else runs on the box |
| Non-default ports, URLs carrying credentials, non-text bodies | The allowlist names hosts, not endpoints |

Addresses are re-resolved and re-checked inside the dialer, which then connects to
the vetted literal rather than the name — resolving once and handing the name to
the transport is the hole DNS rebinding aims at. Responses are bounded (20s, 2 MiB,
5 hops) and wrapped in `BEGIN/END UNTRUSTED WEB CONTENT` markers stating that the
enclosed text is evidence, not instruction. That framing is steering; the boundary
is still the gate every subsequent tool call passes.

---

## First-Class Local LLM Engineering

```mermaid
flowchart LR
    Discover["30ms zero-config discovery<br/>Ollama · LM Studio · vLLM · llama.cpp · Jan"] --> Prep["Context preparation"]
    Prep --> Compact["Append-only compaction<br/>preserves KV prefix reuse"]
    Compact --> Stream["Streaming"]
    Stream --> Recover{"Wire-level recovery"}
    Recover --> Qwen["Qwen3 XML parser recovery"]
    Recover --> Hermes["Hermes function-calling recovery"]
    Recover --> Think["Prefilled think-tag sanitization"]
    Recover --> Trunc["Recoverable output truncation"]
```

Append-only compaction means warm requests reuse the cached KV prefix — **1.5s vs 120s cold prefill on 27B models**. Certify the wire contract against a real endpoint with `manvi probe local`.

---

## Quick Start

```bash
# Run the complete test & verification suite (gofmt+vet+test, fmt+clippy+test, parity+interop)
./verify.sh

# Build both planes
go -C manvi build -o /tmp/manvi ./cmd/manvi
cargo build --manifest-path crates/Cargo.toml --bin dcstore --bin dcverify

# Interactive full-screen TUI
/tmp/manvi

# Headless single turn (exit codes: 0 ok · 1 failure · 2 step ceiling · 3 output cap · 4 no answer · 5 unfinished)
/tmp/manvi run -p "fix the failing test in src/calc.go"
/tmp/manvi run -p "..." --json --max-steps 40 --timeout 10m

# Local models: discover endpoints, then run against them
/tmp/manvi local
export MANVI_LLM_PROVIDER_DEFAULT=local
/tmp/manvi run -p "remove unused imports from helper.go"

# Expose gates + LLM prep over NDJSON stdio for IDE embedding
/tmp/manvi serve
```

---

## Cheat Sheet

### CLI Subcommands

| Command | Description |
|---|---|
| `manvi` | Full-screen interactive TUI (composer, transcript, approvals, dashboard) |
| `manvi doctor` | Check configuration, store reachability, weakened gates |
| `manvi run -p "..."` | One headless turn; exit 2 = step ceiling, 3 = output cap, 4 = no answer, 5 = unfinished |
| `manvi local` | Discover local model servers (~30ms) |
| `manvi pull` | Fast-forward the current branch; refuses a dirty tree |
| `manvi push` | Push the current branch to its configured upstream (never force) |
| `manvi issues` | Report up to 100 open GitHub issues, newest activity first |
| `manvi serve` | NDJSON stdio host plane for IDEs and desktop apps |
| `manvi check PATH` | Dry-run a file write against policy (no mutation) |
| `manvi allow PATH` | Record an audited human grant after a soft denial |
| `manvi lease list` | Show active task leases and lock holders |
| `manvi flags` | Show settings, active values, safety markings, origin layers |
| `manvi probe local` | Certify local LLM wire contract with a real tool-call response |

### TUI Keybindings

| Keybinding | Action |
|---|---|
| `Ctrl+P` | Command palette & slash commands |
| `Ctrl+N` / `Ctrl+T` | New session tab · `Ctrl+W` close tab |
| `Ctrl+S` | Session switcher modal |
| `Ctrl+Y` | Live theme switcher (`/theme`) |
| `Ctrl+G` | Global telemetry dashboard & lease inspector |
| `Ctrl+1`–`Ctrl+9` | Jump to session tab |
| `Ctrl+C` | Cancel running turn (releases leases cleanly) |
| `Tab` / `Shift+Tab` | Pane focus / autocomplete |

---

## Documentation

Full specifications live in [`docs/`](docs/README.md):

| Guide | Contents |
|---|---|
| [Technical Architecture](docs/ARCHITECTURE.md) | Dual-plane partition, stdio IPC protocol, session-log invariants, package map |
| [Policy & Safety Engine](docs/POLICY_AND_SAFETY.md) | 5-tier ladder, outcome states, Write/Command gates, grants ledger |
| [Agent & Turn Lifecycle](docs/AGENT_AND_TURN_LIFECYCLE.md) | Turn loop, tool waterfalls, compaction, lease concurrency |
| [Verification & Parity](docs/VERIFICATION_AND_PARITY.md) | Parity methodology, rigor gates, coverage semantics, `./verify.sh` |
| [Local LLMs](docs/LOCAL_LLMS.md) | Discovery, KV prefix preservation, wire recovery, server configs |
| [Stdio Host Plane](docs/SERVE_HOST_PLANE.md) | Zero-cgo NDJSON protocol, operations reference, IDE embedding |
| [Terminal UI & Events](docs/TUI_AND_EVENT_SUBSYSTEM.md) | Elm loop, tabs, themes, damage diffing, ANSI reduction |
| [CLI & Configuration](docs/CLI_AND_CONFIGURATION.md) | Commands, flag catalogue, mutability scopes, posture matrix |
| [Native Tool Suite](docs/TOOLS_REFERENCE.md) | All 44 native tools in Go and Rust |
| [Comparison](docs/COMPARISON.md) | MANVI vs SWE-agent, OpenHands, Aider, Claude Code |
| [Trade-offs](docs/TRADE_OFFS.md) | Write discipline vs shell breadth, two toolchains, zero-cgo costs |
| [Hardening Ledger](docs/HARDENING_LEDGER.md) | 30+ hardened invariants, bug patterns, test locations |
