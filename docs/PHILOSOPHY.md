# Design Philosophy

> **Waylode is where you work. Manvi runs the agent. DevCouncil knows the code and checks the result.**
>
> **Manvi's thesis:** *An embeddable, local-model-first agent harness under explicit policy.*

## 1. Thesis

*Manvi* (Sanskrit: *of Manu*, the rule-giver) is the execution harness in the stack. It runs the model loop, dispatches native tools, maintains terminal state, and exposes local-model and policy-enforcement planes to external hosts such as Waylode and desktop computer-use backends such as Jarvis.

The design assumes two constraints that general-purpose coding agents usually trade away: an agent should work well against open-weight models running on local hardware, and every mutation or shell command it executes should pass deterministic policy and verification gates that fail closed when evidence is missing.

## 2. Core Principles

### 1. Dual-Plane Determinism Without FFI Fragility

Manvi splits responsibilities by what each toolchain guarantees best:

- **Go runs the harness plane**: turn loops (`manvi/agent/loop.go`), provider streaming, cancellation, TUI rendering (`manvi/ui/`), and the host server (`manvi/serve/`).
- **Rust runs deterministic data and verification invariants** inside DevCouncil's components: the SQLite task-lease partial unique index (`dcstore`), AST stub/secret/coverage gates (`dcverify`), ignored search (`dcgrep`), and the code index (`devmap`).

Instead of linking Rust stores and analyzers via `cgo`, `purego`, or WASI `wazero` (each of which breaks static single-binary distribution, POSIX WAL SQLite locking, or fault isolation, as documented in [Trade-offs](TRADE_OFFS.md)), Manvi talks to DevCouncil binaries over stdio JSON. To eliminate per-call fork latency without reintroducing FFI, DevCouncil's Go store client holds a small pool of long-lived `dcstore serve` processes (`~3.9ms` per-fork down to `~0.03ms` p50 on an idle machine). The single linked exception is Gusset's `dc-glob` pattern engine, verified at runtime via `manvi gusset-check`, while `CGO_ENABLED=0` builds remain tested with Go's `fnmatch` fallback.

### 2. Local-Model-First Engineering

Cloud frontier models tolerate bloated prompts, broken reasoning tags, and naive step cutoffs; local open-weight models running under MLX, Ollama, llama.cpp, or vLLM do not. Manvi treats local inference as the primary design target rather than a degraded fallback:

- **Prefix-cache preservation**: append-only session projection and compaction keep prompt prefixes stable so warm local runs avoid repeated full-context prefill penalties.
- **Wire & reasoning sanitization**: unmatched closing `</think>` tags and Hermes/Qwen3 tool-call formatting quirks are normalized cleanly instead of polluting follow-up turns ([Local LLMs](LOCAL_LLMS.md)).
- **Live wire certification**: `manvi probe local` verifies a server's real tool-calling wire contract before turning an agent loose on it.
- **Progress-weighted step budgets**: rather than cutting off turns at an arbitrary small step count, `manvi/agent/` pairs a `500`-step ceiling with progress stall tracking (`StallCost = 3` in `manvi/agent/progress.go`) and a streaming token stall timeout (`llm.local.stall_timeout`).
- **Multi-provider tiers & structured debate**: `llm.tier.roster` and `llm.tier.<tier>.fallback` (`manvi/cmd/manvi/tiers.go`) route roles cleanly across local and cloud models, while `devcouncil_debate` (`manvi/devcouncil/debate.go`) runs bounded `2`–`4` debater critiques (max `3` rounds) without mutating files directly.

### 3. Write Discipline Without Blinding Exploration

Strict task scope constrains **mutation**, not **observation**. Read-only discovery tools (`devcouncil_read_file`, `devcouncil_list_dir`, `devcouncil_grep`, `devcouncil_find_files`) stay unobstructed under root containment in `strict` posture just as in `dev`.

When mutation occurs, Manvi enforces a deliberate **command allowlist asymmetry**:

- **Bounded unplanned file writes can be self-granted**: past the subsystem/neighbour rung, `scope.unplanned` fires as a soft block that the agent may clear via `devcouncil_request_override` with a 15-minute TTL and a recorded reason—audited as `granted`, never `clean`.
- **Unlisted shell commands cannot be self-granted**: `command.no_lease` and `command.not_allowed` are soft rules that refuse automatic agent self-overrides unless an operator explicitly enables `grants.agent.allow_commands`, because arbitrary shell execution is the channel capable of bypassing downstream file gates ([Trade-offs](TRADE_OFFS.md)).
- **Preview matches execution**: `devcouncil_policy_check_write`, `manvi check`, and `devcouncil_write_file` all resolve `authorisingTask` identically (`manvi/devcouncil/policy_tools.go`), so pre-flight checks never promise an allow that an edit refuses.

### 4. Defense-in-Depth Containment & Fail-Closed Boundaries

No single check is treated as sufficient once a shell or external desktop surface is involved, and an incomplete check never reports as a clean pass:

- **Layered secret containment**: `memguard` pre-`mlock` probes bounded at `10s` (`manvi/credentials/lockedmem.go`), `ui.Sanitize` output scrubbing, `command.secret_read` operand separation (`63ca82ea`, keeping regex search patterns distinct from file reads), macOS Seatbelt `/usr/bin/sandbox-exec` realpath symlink containment (`manvi/devcouncil/containment.go`), staged blob & `+`-line secret scanning (`manvi/devcouncil/commitscan.go`), and pre/post-exec git ref rollback (`manvi/devcouncil/refguard.go`).
- **Fail-closed verification & RPC**: missing `dcverify`, timed-out captures, truncated listings, or malformed checker/worker RPC messages (`b27`–`b32`) produce `degraded`, `ruleRefUnscanned`, or explicit error returns (`unavailable`, `okDegraded`, `PathReport.Passed()` in `manvi/devcouncil/verify_paths.go`) rather than empty successes.
- **Host-owned computer-use privacy**: `PrivacyPolicy` and `EffectivePolicy` (`manvi/computer/privacy.go`, `mask.go`) apply folded Unicode matching (`foldSensitive`) and control region memory (`maskMemory`) strictly from host/workflow inputs that model tool arguments cannot inspect or weaken.

### 5. Strict Embeddable Scope, Zero Paywalls & Single-Owner Wrapping

Manvi ships as both an interactive CLI/TUI (`manvi`) and an embeddable sidecar (`manvi serve` over `ProtocolVersion = 1` NDJSON with per-server `serve.Module` / `Router` seams in `manvi/serve/module.go`). It is 100% free, open source (`MIT OR Apache-2.0`), local-first, and requires zero sign-in (`manvi` contains zero billing, account, or paywall code). Local turn execution, policy enforcement + Gusset, local JSON audit export, and solo AI spend tracking (`CostNanoUSD` in `manvi/session/log.go`, `StallCost` in `manvi/agent/progress.go`) work identically for standalone operators and **Waylode Primary (Free)** users—including BYOK SSH runners and model keys.

When organizations opt into **Waylode Labs** (`Labs Geek`, `Labs Geek Squad`, or `Enterprise` over `gitpulse-relay`), those server-hosted features layer strictly *outside* Manvi's local core via typed host seams: remote mobile/web command approval via `manvi serve`, Space-level team spend rollups aggregating `CostNanoUSD`, and Enterprise org-signed `policy_bundles` enforced through `manvi serve` / Gusset. Because hosts select Manvi alongside DevCouncil modules independently, Manvi refuses to absorb responsibilities belonging to sibling layers:

- Policy rule definitions, `dcstore` schemas, `dcverify` parsing, and `devmap` indexing belong to DevCouncil.
- Desktop ADE UI, git worktree management, CoW cloning, PTY hosting, load governance, and Waylode Labs relay/subscription workflows belong to Waylode.

## 3. Deliberate Trade-offs & What We Refuse to Do

- **No silent fallbacks for missing engines**: if `dcgrep` is absent, `devcouncil_grep` fails explicitly rather than walking files in Go and risking a false `{"count": 0}` answer; if `dcverify` is missing or timed out, verification settles as `degraded`.
- **No FFI or WASI store shortcuts**: preserving static binary portability and real POSIX WAL locking (`dcstore`) outweighs eliminating the minimal `0.03ms` pooled stdio boundary.
- **No hidden token clamping**: oversized token windows (`> 16,777,216` via `maxTokenCount` in `manvi/serve/protocol.go`) are rejected as `badRequest` rather than silently clamped to a smaller plan that misleads the host.
- **No LLM HTTP streaming in `manvi serve`**: the host owns its HTTP connection and partial token stream; `manvi serve` deliberately sends correlated JSON request/response lines without a partial-event channel (`manvi/serve/protocol.go`).
- **No `work_*` escalation to managed external CLIs**: host-scoped `work_*` agent tools (`WorkBinding` in `manvi/devcouncil/work_tools.go`) exist strictly inside Manvi's in-process `Registry` and are never exposed to separate managed Codex or Claude subprocesses.
- **No parallel board or code-graph ownership**: Manvi consumes `contracts/verdict.schema.json` and `contracts/event.schema.json` read-only from DevCouncil and closes remaining tool/verifier DTO overlaps (`OV-VERIFY-01`, `OV-TOOLS-01`) in place rather than maintaining parallel engines.

## 4. How This Fits the Stack

- **Stack-Wide Ownership Matrix**: `DevCouncil/specs/003-stack-boundaries-and-non-overlap` (canonical boundaries across DevCouncil, Manvi, Waylode, Gusset, and Jarvis)
- **Edition Boundary**: `GitPulse/specs/003-primary-and-labs-editions` (Waylode Primary Free vs. Waylode Labs over `gitpulse-relay`; Manvi stays 100% free, zero-sign-in, and zero-billing)
- **Sibling Philosophies**:
  - DevCouncil — `DevCouncil/docs/PHILOSOPHY.md` (*Deterministic truth and modular engines before the edit*)
  - Waylode — `GitPulse/docs/PHILOSOPHY.md` (*A workspace, not a chat window*)
- **Manvi Engineering Index**:
  - [Specifications](../specs/README.md) ([001: Harness & Host Plane](../specs/001-agent-harness-and-serve-plane/spec.md), [002: Policy Enforcement & Hardening](../specs/002-policy-enforcement-and-hardening/spec.md))
  - [Components and Harness](COMPONENTS_AND_HARNESS.md) · [Architectural Trade-offs](TRADE_OFFS.md) · [Policy & Safety](POLICY_AND_SAFETY.md) · [Local LLMs](LOCAL_LLMS.md)
