# MANVI Agent & Turn Lifecycle Specification

This document details the turn execution loop, tool execution waterfalls, multi-agent fan-out management, and SQLite task lease concurrency in **MANVI**.

---

## 1. The Turn Execution Lifecycle

The agent execution loop in `manvi/agent` is modeled as an evidence-driven, waterfall-augmented state machine.

```mermaid
sequenceDiagram
    autonumber
    actor User as User / Harness CLI
    participant AL as Agent Loop (manvi/agent)
    participant Log as Session Log
    participant Waterfall as Event Bus Waterfalls
    participant Provider as LLM Provider (Anthropic/Gemini/xAI)
    participant ToolReg as Tool Registry (manvi/tools)
    participant Gate as Policy Gate (manvi/gate)

    User->>AL: Run(prompt)
    activate AL
    AL->>Log: Append User Message Record
    
    loop Each Step (until evidence of completion or MaxSteps)
        AL->>Log: Project History (Messages)
        AL->>Log: Assert Model-Visible == Logged
        
        AL->>Waterfall: Trigger PreStep Waterfall
        alt Context Compaction Needed
            Waterfall->>Log: Append ToolResultCompacted event(s)
            Waterfall->>Log: Re-derive History
            Waterfall-->>AL: Compacted Messages (== projection)
        else Step Rejected
            Waterfall-->>AL: Reject Error -> Abort Turn
        end

        AL->>Waterfall: Trigger LLMRequest Waterfall
        AL->>Provider: Stream(Request)
        activate Provider
        
        loop SSE Streaming Chunks
            Provider-->>AL: StreamDelta (Text / Reasoning / ToolCall)
            AL->>Log: Record Streaming Delta
        end
        Provider-->>AL: Response Complete (Usage, ReplayState)
        deactivate Provider

        alt Model Emitted Tool Calls
            loop For Each Tool Call
                AL->>ToolReg: Execute(ToolCall)
                activate ToolReg
                ToolReg->>Gate: Evaluate Write / Command Policy
                Gate-->>ToolReg: Decision (Passed / Blocked / Granted / Demoted)
                
                alt Policy Passed or Allowed
                    ToolReg->>ToolReg: Run Native Tool Body
                    ToolReg-->>AL: ToolResult (Output, Status, Qualifications)
                else Policy Blocked
                    ToolReg-->>AL: ToolResult (Error: Policy Violation)
                end
                deactivate ToolReg
                
                AL->>Log: Append Tool Result Record
            end
        else No Tool Calls (Apparent Completion)
            AL->>Waterfall: Trigger TurnStopping Serial Check
            alt Listener sets Inject and bounces remain
                Waterfall-->>AL: Inject + Verdict
                AL->>Log: Append harness message (Origin=harness)
                Note over AL: Turn stays open for one more step
            else No Inject, or the bounce budget is spent
                Waterfall-->>AL: Verdict -> Terminate Loop
            end
        end
    end
    
    AL-->>User: Turn Summary & Report
    deactivate AL
```

### 1a. The terminal checkpoint

`TurnStopping` is where a turn that *looks* finished is judged. It carries what
the turn actually did — whether a mutating tool ran, which paths were changed,
whether the final answer was cut off by the output cap — and a listener answers
by setting a verdict and, when it wants another step, the text the model is to
act on.

Four rules hold it together:

- **A listener supplies the reason, and the loop appends it.** The contract used
  to be "return an error to keep the turn open", and the loop's response was to
  re-ask the model with an unchanged history — a spin, not a gate. A listener
  that wants another step now says what changed.
- **The inject is marked.** It occupies the `user` role on the wire, because a
  natural stop leaves no other model-visible slot, so it is written with
  `Origin: harness`. The transcript, a resumed session and an evidence report all
  render it as the harness speaking rather than as the operator.
- **Bounces are bounded** at `MaxCheckpointBounces` (two) per turn. Past that the
  turn closes and `Outcome.BouncesExhausted` records that the objection was still
  standing — the alternative, riding the 500-step ceiling, finds the same dead end
  an hour later.
- **A listener error closes the turn as degraded.** A check that could not run has
  not asked for anything, and it is never reported as a pass.

The harness registers one listener on both faces: the end-of-turn check in
`manvi/cmd/manvi/sensor.go`. It skips a turn that changed nothing, verifies the
paths a mutating turn actually wrote, and escalates to an adversarial reviewer
only on the evidence that telling the model was not enough — a second failure on
the same turn, or a turn the loop already judges to be going in circles. That is
the same rule `manvi/agent/effort.go` uses to decide when to buy more reasoning,
and it is deliberate: classifying the prompt up front would be a judgement made
on the least information the harness will ever have.

`manvi serve` is out of scope for all of this. The host owns the model call
there, `agent.Loop` does not run, and the checkpoint never fires.

### 1b. Pre-Turn Baseline & Wire Resilience

1. **Concurrent Pre-Turn Baseline & Diff Seeding**:
   - The pre-turn baseline capture runs concurrently beside the first model request rather than delaying it, reducing time-to-first-token from ~50 ms to under 0.1 ms.
   - The turn's first tool call waits for the baseline capture if still in flight.
   - Two new lifecycle events bracket this phase: `ToolsDispatching` before response tool calls run, and `TurnEnded` on every exit of a turn whose `TurnStarting` was published.
   - The end-of-turn diff seeds from the pre-turn baseline's index rather than re-walking the tree, while preserving racy-git modification timestamps.
2. **Provider Wire Protocol Hardening**:
   - **Thinking Block Invariants**: Replayed Anthropic thinking blocks preserve their required `thinking` string and `signature` (and `data` on redacted thinking) even when empty, satisfying Messages API replay constraints.
   - **Empty / Whitespace Suppression**: Anthropic omits `content` on empty tool results (sending `(no output)` on error results) and drops whitespace-only text blocks. OpenAI-compatible endpoints always ensure tool messages carry a string `content`.
   - **Mid-Call Token Ceilings**: A stream cut off by `max_tokens` mid-tool-call settles as `StopMaxTokens` plus `MalformedCall`, prompting the model to resend a smaller request instead of failing the turn.
   - **Abnormal Exit Cleanup**: Panics, cancellations, or abnormal exits answer all open tool calls with interrupted error results, ensuring session history never holds dangling `tool_use` blocks.

---

## 2. Tool Execution Pipeline & Waterfalls

Every tool call dispatched by the model passes through a three-stage pipeline supporting reversible effects and policy gating:

```mermaid
flowchart TD
    ToolCall["Model Emits Tool Call (e.g. devcouncil_write_file)"] --> OfferCheck{"Was tool offered in step?<br/>(Enforces --chat-only & scopes)"}
    OfferCheck -- No --> OfferError["Refusal: Tool Call Not Offered in Step"]
    OfferCheck -- Yes --> PreExec["1. tools/pre-execute Waterfall"]
    PreExec --> PreHookCheck{"Pre-hook validation:<br/>Schema valid? Context alive?"}
    
    PreHookCheck -- Invalid --> PreError["Return Tool Error to Model"]
    PreHookCheck -- Valid --> GateCheck["2. Policy Gate Evaluation"]
    
    GateCheck --> DecisionTree{"Gate Decision?"}
    DecisionTree -- Blocked --> BlockResult["Tool Error: Policy Blocked with Rule & Reason"]
    DecisionTree -- Allowed --> BodyExec["3. Execute Tool Body"]
    
    BodyExec --> PostExec["4. tools/post-execute Waterfall"]
    PostExec --> AttachMetadata["Attach Qualification Metadata:<br/>GrantID, Demoted posture, Degraded checks"]
    AttachMetadata --> ResultSink["Log Result & Return to Model Context"]
```

---

## 3. Multi-Agent Roles & Hierarchy

MANVI ships six subagent roles, registered in `agents.NewRegistry`
(`manvi/agents/definition.go`). The parent turn is not one of them: it dispatches
these by name through `devcouncil_invoke_subagent` / `devcouncil_spawn_subagents`.

The tree is one level deep, and that is structural rather than counted. A child's
registry is built without the whole sub-agent dispatch group, so there is no path
to a grandchild for a counter to bound. Ordering between roles is the parent's
choice, not a pipeline the harness enforces.

```mermaid
flowchart TB
    Parent["Parent Turn<br/>(agent loop, holds the lease)<br/>fan-out bounded by agents.max_fanout,<br/>narrowed to where children are placed"]

    Research["research<br/>read-only, MCP allowed"]
    Planner["planner<br/>read-only, MCP allowed"]
    Reflector["reflector<br/>read-only, no MCP, no writes"]
    Critic["critic<br/>read-only, MCP denied"]
    Builder["builder<br/>writes, MCP allowed"]
    Stress["stress_tester<br/>writes, MCP allowed"]
    Self["self<br/>inherits parent config"]

    Parent --> Research & Planner & Critic & Reflector
    Parent --> Builder & Stress & Self
```

| Role Name | Declared Role | Tier | Write Tools | MCP Tools | Primary Responsibility |
|---|---|---|---|---|---|
| `research` | Codebase & Documentation Researcher | lite | No | Yes | Explores and comprehends the codebase, navigates symbols via the dev map, verifies official documentation, identifies structural gaps without mutating. |
| `builder` | Full-Stack Feature Builder | heavy | Yes | Yes | Builds on existing core functions without duplication, characterizes baseline behavior with tests first, verifies gap resolution. |
| `critic` | Adversarial Code & Security Reviewer | advisor | No | **No** | Audits proposed changes against invariants, edge cases (empty, nil, concurrent, timeout), credential safety, and regression risk. |
| `planner` | Problem Deconstructor & Hypothesis Architect | lite | No | Yes | Deconstructs requirements, formulates verifiable hypotheses, drafts structured plans under `.devcouncil/artifacts/` without code mutations. |
| `reflector` | Session Reflector & Lesson Attributor | lite | No | **No** | Reads one session's evidence packet, attributes each harness change as helped, hurt, no observed effect or confounded, and proposes at most one lesson or `NO_ENTRY`. Has no write exception: `manvi reflect` parses its answer and files it. Tools narrowed to the artifact index. |
| `stress_tester` | Adversarial Stress Tester & Hardener | heavy | Yes | Yes | Attacks solutions with boundary conditions, concurrent races, malformed inputs, and timeouts. |
| `self` | Autonomous Pair Subagent | heavy | Yes | Yes | Inherits the parent's configuration, tools, and system prompt for delegated concurrent work. |

Every shipped role states a tier (`agents.Tier`: basic < lite < heavy < main <
advisor), and none pins a model. The tier is the dearest the role runs on; the
tier a given dispatch gets is decided by `agents.Route` from facts known at
dispatch time, never from the prompt: a verdict request goes to advisor, a child
that may write to heavy, a read-only child to lite — or to basic in a fan-out of
`agents.BasicFanoutWidth` (4) or more — capped at the role's tier. No child routes
to main, which is the dispatching agent's own. The runner resolves the tier
against the session's tier profile; on a provider with no profile (anything but
`anthropic`), or for a tier the profile does not hold, the child inherits the
parent's model as before. A child on a tier runs at that tier's own effort, not
the parent's. `manvi agents` prints role → tier → model.

A dispatching model may name only a tier, never a model id or provider, and only
one at or below the role's tier: the per-call `model` of
`devcouncil_invoke_subagent` and the `model`/`tier` of
`devcouncil_define_subagent` refuse anything else, naming the tiers allowed. A
role a model defines is capped by its permissions — heavy if it may write, lite
if not. A role the operator registers in-process may still name a raw model, and
a per-call tier cannot move it.

The end-of-turn checkpoint critic runs as the `critic` role, read-only whatever
that role says, on the tier a verdict routes to (advisor).

A child that shows evidence of being out of its depth is moved up a tier once,
on evidence and never on a reading of its prompt. The ladder is: the loop raises
effort on the child's own model up to the session's ceiling; if the turn still
comes back with evidence (the step ceiling reached, an empty summary, no verdict
where one was asked for, or an end-of-turn check still failing), the child is
re-run on the nearest tier above its own that runs a *different* model — heavy
and main both being Opus, heavy escalates to the advisor — from a fresh
conversation. Thinking blocks belong to the model that produced them, so the
re-run gets the original prompt and a short account of what the first attempt
left, never its transcript. The advisor is the top and never escalates; neither
does a child placed on the parent's model or pinned by the operator. One cap per
fan-out, ceil(N/2) re-runs, is shared by its children under a lock-free counter.
A re-run showing the same evidence fails the child with `ExhaustedError`; a
re-run refused by the provider, the spend cap or a cancellation fails it with
`EscalationError`, and neither climbs further. The fan-out report names each
escalation (`escalations`: child, from, to, evidence, `cold_cache`): the re-run
is on another model, so its first request writes the whole prompt to that
model's cache again, and the extra cache writes in the usage line are that.
Within a tier the role's system prompt is the same bytes on every step.

Under an Anthropic spend cap a wide fan-out is narrowed rather than refused: a
request reserves a worst case, so the children's reservations together far
exceed what they spend. Children start in order while the running children's
reservations fit the cap; the rest wait for a running child to settle. Only a
child that cannot fit the cap even alone refuses the dispatch.

The tool-permission
columns above are the two switches a definition actually carries
(`EnableWriteTools`, `EnableMCPTools`); finer-grained per-tool permissions are
not part of a role definition.

A role's two switches are enforced by building the child's registry without the
denied tools, not by hiding schemas — a registry that still holds a tool
dispatches it by name whether or not the schema was offered. A definition may
also carry an `allowed_tools` allowlist, which only ever removes: it is
intersected with the caller's read-only floor and the structural absence of
sub-agent dispatch, never unioned with them, so naming a tool there cannot hand a
child something those rules took away.

Roles are registered by name, so defining a role reuses a shipped name rather
than shadowing it. Whether a *model* may overwrite one of the six is decided at
the tool boundary — the only layer that knows a call came from a model — and
whether it may define new roles at all is governed by `subagents.dynamic.enabled`.

`manvi probe PROVIDER` is a CLI command that makes one live request to check a
provider's wire contract. It is not a subagent role and cannot be dispatched.

---

## 4. Sub-Agent Fan-Out & Concurrency Pool

The `manvi/agents` package manages concurrent agent trees with explicit bounds and cleanup guarantees.

```mermaid
classDiagram
    class Pool {
        +MaxDepth: int
        +MaxFanout: int
        +Depth: int
        +Releaser: Releaser
        +ReleaseTimeout: Duration
        +Child() (Pool, error)
        +Run(ctx, tasks) (Result[], error)
    }

    class Holder {
        -leases: []Lease
        +Add(lease Lease)
        +Drop(taskID string)
        +Held() []Lease
    }

    class Task {
        +Label: string
        +Run: func(ctx, holder) (any, error)
    }

    class Lease {
        +TaskID: string
        +Token: string
    }

    Pool o-- Task
    Task --> Holder
    Holder o-- Lease
```

### Cancellation with Fresh Contexts

```mermaid
sequenceDiagram
    autonumber
    participant Parent as Orchestrator / Parent Loop
    participant Child as Builder Goroutine
    participant Holder as Lease Holder
    participant Store as Rust dcstore (SQLite)

    Parent->>Child: Launch Task in Pool (ctx)
    Child->>Store: Acquire Lease on TASK-001
    Store-->>Child: Granted (Token: abc-123)
    Child->>Holder: Add(TASK-001, token: abc-123)
    
    Note over Parent,Child: User presses Ctrl+C or Turn Deadline Expires
    Parent->>Child: Cancel Context (ctx.Done())
    Child-->>Parent: Goroutine Terminates
    
    Note over Parent,Store: Cleanup uses a FRESH, uncancelled Context
    Parent->>Holder: Held() -> [TASK-001, token: abc-123]
    Parent->>Store: Release(freshCtx, TASK-001, token: abc-123)
    Store-->>Parent: Lease Released (Status: released)
```

> [!IMPORTANT]
> If lease release ran on the cancelled context, the subprocess or database call would abort instantly, abandoning the active lease until its TTL expired and blocking future turns!

---

## 5. SQLite Task Lease Mutex Model

Task mutual exclusion is enforced in SQLite (`.devcouncil/state.sqlite`) using a partial unique index:

```sql
CREATE TABLE task_leases (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL,
    owner TEXT NOT NULL,
    token TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK(status IN ('active', 'released', 'expired', 'stolen')),
    acquired_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    renewed_at TEXT,
    released_at TEXT,
    metadata JSON
);

-- THE LOAD-BEARING PARTIAL UNIQUE INDEX:
CREATE UNIQUE INDEX ux_task_leases_active 
ON task_leases (task_id) 
WHERE status = 'active';
```

```mermaid
stateDiagram-v2
    [*] --> Active : Acquire (INSERT with status='active')
    Active --> Released : Normal Release (Matching token)
    Active --> Expired : Clock passes expires_at
    Active --> Stolen : Force Steal (Previous lease marked stolen)
    Active --> Active : Renew (Extends expires_at)
    Released --> [*]
    Expired --> [*]
    Stolen --> [*]
```

### Concurrency Guarantees

- **Single Winner**: When 12 threads attempt to acquire `TASK-001` simultaneously, SQLite's transactional partial index forces exactly 1 `INSERT` to succeed; 11 transactions fail with constraint errors.
- **Clock Drift Defense**: Lease expiration is evaluated on read and normalized using UTC ISO-8601 timestamps.
- **Safe Steal**: `dcstore force` atomically marks the previous holder's active lease as `stolen` and writes a new active row in a single immediate transaction.


## Context Compaction

Compaction shortens tool results to keep a turn inside the model's context
window. It does not rewrite the messages travelling through the pre-step
waterfall; it appends `context/tool_result_compacted` events to the session log
and re-derives history from the projection. Three properties follow, and each
one removes a defect:

**The log and the request cannot disagree.** Rewriting messages on the way out
meant the log recorded a request that was never sent, and the
model-visible-means-logged assertion — on by default — failed the first time
compaction touched a multi-line tool result. There is now no exemption in the
invariant check, because none is needed.

**A compaction names its result.** The event carries `result_seq`, the sequence
number of the `tool/result` event it shortens, so a call id that a provider
reuses across responses does not carry one result's compaction onto another and
does not make a later result look already compacted. The planner identifies a
result as its call id and its place among the results with that id, and the log
resolves that to the sequence when the plan is applied. A record without
`result_seq`, which is every journal written before the field, names the call id
only and applies to every result with it, as it always did.

**Images are compacted, and say so.** A result's images are priced in the
request like its text, so the planner counts them and can reclaim them: a result
of images alone (a computer-use screenshot) is eligible, and a compacted result
that held images carries `[N image(s) removed to fit the context window]` after
whatever text it kept, so the model is told it once saw them. A result without
images is shortened exactly as before.

**A result is compacted once.** The text it is given is the text it keeps for
the rest of the session. Recomputing compaction every step, with tiers that
escalate as the turn grows, changed the prompt prefix on every step.

**Compaction aims past the threshold, not at it.** A local server's KV cache is
keyed on an unchanged token prefix: editing history invalidates it and costs a
full re-prefill — 120s for a 14.7k-token prompt on a 4-bit 27B, against 1.5s
warm. Landing exactly on the threshold re-triggers on the next tool result and
pays that cost again, so compaction targets 70% of the threshold and becomes a
rare event. Measured over a 12-step turn against a live server, this reduced
full re-prefills from one per step to two per turn.

The original tool result is never deleted. The `tool/result` event that carries
it stays in the log, so an evidence report can still show what the tool actually
returned while the model's history carries the short form.

When everything compactable has been compacted and history still exceeds the
budget, a `context/overflow` event records the shortfall. The turn continues —
the server's own refusal is more informative than a pre-emptive one — but a
request that is going to be truncated is never indistinguishable from one that
fits.

### Token budget & Signed Reasoning Preservation

The budget counts the system prompt, the messages, **and the tool schemas**,
which are sent on every request and measure 1,755 real tokens on the shipped
DevCouncil surface. The estimator is calibrated against the raw uncalibrated token count
(`CountRequestTokens`) of the request the host sends rather than feeding the calibrator
its own corrected output, avoiding compounding distortion and converging accurately to true ratios.

**Signed reasoning preservation**: On advanced frontier models (Claude 3.7/Fable 5.1/Opus),
thinking block signatures bind previous messages and tools. Compaction shortens earlier tool
results, representing an edit to prior history. Live appends scrub reasoning before minting
new blocks, but retroactive log scrubs (`RestoreSensitiveLog`) drop subsequent signed reasoning blocks
(`llm.DropSignedReasoning`) along with their signatures, recording redaction in `reasoning_redacted`
to prevent invalid signatures from reaching providers.

---

## Related Documentation

- [Documentation Index](README.md)
- [Why MANVI is Different (Comparison)](COMPARISON.md)
- [Technical Architecture Specification](ARCHITECTURE.md)
- [Policy & Safety Engine Specification](POLICY_AND_SAFETY.md)
- [Running Against Local LLMs](LOCAL_LLMS.md)
- [Embedded Stdio Host Plane (`manvi serve`)](SERVE_HOST_PLANE.md)
- [Terminal UI & Event Subsystem](TUI_AND_EVENT_SUBSYSTEM.md)
- [DevCouncil Native Tool Suite](TOOLS_REFERENCE.md)
- [Hardening Ledger & Defects](HARDENING_LEDGER.md)

