# Feature: 001 - Agent Harness, Tool Registry, and `manvi serve` Host Plane

**Status**: Active Specification | **Canonical Owner**: Manvi (`github.com/bharathvbcr/Manvi`) | **Date**: 2026-11-26 | **Implementation State**: Shipped runtime core (`manvi/agent`, `manvi/devcouncil`, `manvi/serve`, `manvi/ui`), with two tracked overlap cleanups in `tasks.md`.

## 1. Problem Statement & Motivation

DevCouncil provides the deterministic control plane (policy rules, task leases, SQLite board store, `dcverify` content rigor gates, and `devmap` structural index), while Waylode provides the desktop application shell, workbench UI, and git hooks. Between them, an execution runtime is needed that can drive agent turns directly (both interactive TUI and headless `manvi run`), expose a uniform in-process tool surface gated by DevCouncil rules, orchestrate bounded multi-model subagent/debate workflows, and expose local-LLM, policy, verification, and managed-session operations over a stable stdio/socket boundary to non-Go hosts such as Waylode without `cgo` coupling or duplicated business logic.

`specs/001` codifies Manvi's canonical ownership of the agent harness, tool registry, terminal UI/event stream, and `manvi serve` host plane, while establishing strict non-overlap boundaries with DevCouncil, Waylode, Gusset, and Jarvis.

## 2. Canonical Ownership & Non-Overlap Boundary

### Ownership Matrix

| Layer / Capability | Canonical Owner | Manvi Role | Explicit Non-Goals / Delegations |
| --- | --- | --- | --- |
| **Agent Turn Loop, Context Compaction & Budget** | **Manvi** (`manvi/agent/`, `manvi/cmd/manvi/`) | Owns step execution (`PreStep`, `LLMRequest`, `TurnStopping`, `TurnStarting`), bounded compaction, local-model-first resolution (`manvi/cmd/manvi/tiers.go`), and SQLite lease/checkpoint persistence. | Does not own board backlog prioritization or desktop window state (Waylode). |
| **Multi-Agent Harness (`feat/team-harness`)** | **Manvi** (`manvi/devcouncil/debate.go`, `manvi/devcouncil/subagent_tools.go`, `manvi/cmd/manvi/tiers.go`) | Owns `llm.tier.roster`, `llm.tier.<tier>.fallback`, role-to-tier routing, bounded subagent pools, and `devcouncil_debate` (`2`–`4` debaters, max `3` rounds). | Does not own cross-worktree GitPulse/Waylode session coordination or host-side worktree creation (`manvi/serve/managed.go` invokes `work.runs.managed.*` contracts). |
| **In-Process Native Tool Registry** | **Manvi** (`manvi/devcouncil/tools.go` & siblings) | Owns the 55-tool runtime registry (54 without repo skills, +1 conditional `devcouncil_fetch_url`), batch/script tools, 3 simplified graph wrappers (`devcouncil_graph_query`, `devcouncil_code_dead`, `devcouncil_graph_context` in `manvi/devcouncil/navigate.go` over `DevCouncil/backend/go_orchestrator/dc/devmap/warm.go`), `work_*` scoped board tools (`manvi/devcouncil/work_tools.go`), and unified `authorisingTask` / `okDegraded` semantics. | Delegates policy evaluation to DevCouncil (`policy`, `gate`), verification rigor to `dcverify` (`DevCouncil`), and code graph indexing/MCP to `devmap` (`DevCouncil`: 21 MCP tools, Schema `29`, extract `"102"`). |
| **Terminal UI & Event Stream** | **Manvi** (`manvi/ui/`) | Owns the Elm-architecture TUI (`ui.Event`, `ui.Sanitize`), single-line status bar, paste chips, and headless NDJSON (`manvi watch --json`). | Does not render desktop SwiftUI/Electron surfaces (Waylode). |
| **`manvi serve` Host Plane (`ProtocolVersion = 1`)** | **Manvi** (`manvi/serve/{server,module,protocol,workbench,verify,managed,devmap}.go`) | Owns the bidirectional NDJSON stdio/Unix-socket server, `serve.Module` / `serve.Router` extension seam, `work.*` host-plane client over `dc/store` (`SCHEMA_VERSION = 11`, `WORKBENCH_SCHEMA = 17`), `VerifyRunner` (`work.verify.{task,get}`), and `ManagedRunner`. | Never defines wire policy verdict states (owned by `DevCouncil/backend/contracts/verdict.schema.json`), alters `dc-store` DDL, or streams partial HTTP LLM tokens over NDJSON (`protocol.go`). |
| **Standalone Phase-4 MCP Tool Server** | **DevCouncil** (`DevCouncil/backend/go_orchestrator/devcouncil/registry.go`) | Interoperates over shared store/gate; never forks the standalone 8-tool MCP server (`OV-TOOLS-01` tracks importing shared specs). | Owned by DevCouncil for external MCP clients (`devcouncil_{get_diff,checkout_task,renew_lease,release_task,next_task,verify_task,get_gaps,policy_check_write}`). |
| **Verification Rules & `dcverify` Rigor Engine** | **DevCouncil** (`DevCouncil/rust/dc-verify`, `DevCouncil/rust/dc-evidence`, `DevCouncil/backend/go_orchestrator/devcouncil/verify/`) | Invokes `dcverify` via `rigorClient` (`manvi/devcouncil/rigor.go`) and runs lease-free turn verification (`VerifyPresent` in `manvi/devcouncil/verify_paths.go`) plus `VerifyRunner` (`manvi/serve/verify.go`). | Must not re-implement stub/secret/coverage parsing in Go or drift `Gap`/`NextAction` shapes from DevCouncil's canonical types (`OV-VERIFY-01`). |
| **Waylode Editions Seam (`Primary` Free vs. `Labs` Host Workflows)** | **Manvi (Zero-Billing Core) + Waylode (`GitPulse` / `GitPulse-labs`)** | Manvi stays 100% free, open source (`MIT OR Apache-2.0`), local-first, and zero-sign-in across local turn loops, `manvi serve`, `CostNanoUSD` (`manvi/session/log.go`) + `StallCost` solo spend tracking, and local JSON audit export. | Manvi contains zero billing, account, or paywall code; Waylode Primary runs all local & BYOK flows free, while Waylode Labs layers optional remote approval (`Geek`/`Geek Squad`), team spend rollups (`Geek Squad`/`Enterprise`), and org-signed `policy_bundles` (`Enterprise`) outside Manvi via `manvi serve` / `gitpulse-relay`. |

### Verified Overlap & Drift Ledger (Aligned with `DevCouncil/specs/003-stack-boundaries-and-non-overlap/contracts/ownership-matrix.json`)

1. **`OV-VERIFY-01` — Verification DTO & Runner Seam (`manvi/serve/verify.go` & `manvi/devcouncil/verify.go` vs `DevCouncil/backend/go_orchestrator/devcouncil/verify/` & `dcverify`, DevCouncil `specs/003` `T-006`)**:
   - `manvi/serve/verify.go` / `verify_checks.go` (`VerifyRunner` for `work.verify.task`) keeps job claiming/state and delegates path/command/rigor verification to `devcouncil.VerifyPresent` (`manvi/devcouncil/verify_paths.go`) + `rigorClient` (`dcverify`), alongside batched `devmap` `QueryAffected` calls (`maxDevmapAffectedBatch = 16`). `manvi/devcouncil/rigor.go` (`gapsFrom`) now delegates action text and gap categorization to `dc.RigorActionText` & `dc.CategoryForGapType` (`DevCouncil/backend/go_orchestrator/dc/verify_actions.go`). Remaining `Gap`/`NextAction`/`Report` structs and `changedFiles` in `manvi/devcouncil/verify.go` (`devcouncil_verify_task`) stay tracked in `T-06`.
2. **`OV-TOOLS-01` — 8 Shared `devcouncil_*` Tools vs 55-Tool Harness Surface (`DevCouncil/registry.go` vs `manvi/devcouncil/tools.go`, DevCouncil `specs/003` `T-007`)**:
   - `DevCouncil` serves 8 canonical task/policy MCP tools (`devcouncil_get_diff`, `devcouncil_checkout_task`, `devcouncil_renew_lease`, `devcouncil_release_task`, `devcouncil_next_task`, `devcouncil_verify_task`, `devcouncil_get_gaps`, `devcouncil_policy_check_write`) exported via `dc.SharedToolSpecs()`.
   - `Manvi` registers those 8 tools in `manvi/devcouncil/tools.go` via `sharedTool` (`dc.SharedToolSpecByName`, tested in `manvi/devcouncil/shared_tools_test.go`), sharing `authorisingTask` & `WorkBinding` while pinning intentional session-bound parameter omissions (`T-07`).

## 3. User Stories & Acceptance Scenarios

### US1: Local-Model-First Agent Turn Loop & Team Harness (Priority: P1)

As an operator or parent host, I can run an agent turn with automatic local-first or provider-qualified tier routing (`llm.tier.roster`, `llm.tier.<tier>.fallback`) and bounded compaction so turns never hang, silently drop errors, or leak uncancelled subagents.

- **Scenario 1: Provider-qualified tier resolution and cross-provider fallback**
  - **Given** a repository configuration defining `llm.tier.roster` and `llm.tier.<tier>.fallback` entries in `manvi/cmd/manvi/tiers.go`
  - **When** the primary tier provider is unavailable or exhausts capacity during turn step resolution
  - **Then** the harness falls back deterministically along the configured fallback chain, records tier provenance in `manvi tiers`, and surfaces model usage without altering policy gates.
- **Scenario 2: Terminal turn checkpoint on cancellation**
  - **Given** an in-flight turn in `manvi/agent/loop.go` with active tool calls or child subagents
  - **When** context cancellation occurs
  - **Then** the loop runs `TurnStopping` with a fresh settlement context so `task_leases`, usage accounting, and degraded notes persist before returning.

### US2: Non-Overlapping Native Tool Registry & `devcouncil_debate` (Priority: P1)

As an in-process agent, I receive deterministic, schema-validated tool surfaces gated by active policy posture and task scope, including bounded multi-perspective reasoning (`devcouncil_debate`) and scoped board access (`work_*`), with explicit `degraded` reporting whenever inputs are capped or checks cannot run.

- **Scenario 1: Bounded `devcouncil_debate` execution**
  - **Given** a turn invoking `devcouncil_debate` in `manvi/devcouncil/debate.go`
  - **When** parameters stay within `[2, 4]` debaters, at most `3` rounds, `16 KiB` question, `64 KiB` context, and `8000` carried answer characters
  - **Then** each debater runs cleanly on its tier-resolved model and returns structured synthesis without mutating files directly.
- **Scenario 2: Honest degradation on partial checks**
  - **Given** a tool call where `dcverify` is missing, timed out, or a diff exceeds capture bounds (`maxUntrackedRendered = 256` or byte cap in `manvi/devcouncil/verify.go`)
  - **When** `okDegraded` or `unavailable` constructs the result
  - **Then** the JSON payload sets `degraded` (or returns `IsError: true`) before marshalling so neither the model nor `PathReport.Passed()` mistakes an incomplete check for a clean pass.

### US3: `manvi serve` NDJSON Host Plane & Module Seam (Priority: P1)

As a desktop host (such as Waylode), I can attach to `manvi serve` over stdio or Unix domain socket using `ProtocolVersion = 1` NDJSON messages, extend routes cleanly via `serve.Module` / `Router`, and run asynchronous workbench scans, refinements, managed sessions, and background verification (`work.verify.task`) without process crashes.

- **Scenario 1: Strict NDJSON request correlation and fail-closed error handling**
  - **Given** a `serve.Server` (`manvi/serve/server.go`) configured with `WorkbenchModule` (`manvi/serve/workbench.go`)
  - **When** a malformed request, negative/oversized token count (`> 16,777,216` per `maxTokenCount`), or failing handler occurs
  - **Then** the server emits a single correlated `serve.Response` line with `ok: false` and keeps the connection alive for sibling in-flight requests.
- **Scenario 2: Bounded `VerifyRunner` background execution**
  - **Given** a `work.verify.task` request in `manvi/serve/verify.go` with valid worktree root and `base`..`head` commits matching `HEAD`
  - **When** active jobs are below `verifyMaxActive` (`4`) and changed paths are collected
  - **Then** `VerifyRunner` claims the worktree read-only, runs `VerifyPresent` + `devmap` affected-test batching (`maxDevmapAffectedBatch = 16`), invokes the read-only critic, and settles within `timeout + 2s` grace as `passed`, `failed`, or `degraded`.

### US4: Terminal UI Event Stream & Terminal Checkpointing (Priority: P2)

As an interactive operator or CI consumer, I observe sanitized, bounded status and approval events via the Elm TUI (`manvi/ui/`) or headless `manvi watch --json` without ANSI control injection or layout drift.

- **Scenario 1: Single-line footer status and ANSI sanitization**
  - **Given** model output or tool stderr containing terminal control sequences arriving at `manvi/ui/`
  - **When** events pass through `ui.Sanitize` into the status bar and transcript views
  - **Then** unsafe escape sequences are stripped while paste chips (`input.Paste`), token/spend telemetry, and approval modals render cleanly in single-line bounds.

## 4. Success Metrics & Testability Criteria

- **SC-001**: `go -C manvi test ./devcouncil -run TestDocumentedToolCountMatchesRegistry -count=1` passes, proving `README.md` and `docs/TOOLS_REFERENCE.md` match the runtime `Registry.Tools()` surface (`55` / `54` / `+1`).
- **SC-002**: `GOMAXPROCS=4 go -C manvi test ./internal/contract/...` passes all contract, doc-link, Mermaid, and residue checks.
- **SC-003**: `manvi/serve` protocol invariants (`ProtocolVersion = 1`, `maxTokenCount = 1<<24`, `Register` collision refusal, `VerifyRunner` bounded settlement) pass `go -C manvi test ./serve/...`.
- **SC-004**: `VerifyPaths` and `devcouncil_verify_task` never emit `passed` when `Degraded` contains any non-`coverageUnmeasuredNote` entry or when `Omitted` is non-empty.
- **SC-005**: `devcouncil spec validate 001 --strict --project-root /Users/bharath/Code/devtools/Manvi` exits `0` with zero blocking or advisory findings.
- **SC-006**: Cross-repo audit keeps `DevCouncil/backend/go_orchestrator/devcouncil/registry.go` (8 MCP tools) and `manvi/devcouncil/verify.go` free of silent schema drift via tracked tasks `T-06` and `T-07`.

## 5. Edge Cases, Exclusions & Open Questions

- **Exclusions**:
  - `manvi serve` deliberately omits token-streaming NDJSON lines (`manvi/serve/protocol.go`); HTTP LLM streaming remains host-owned.
  - Managed Codex/Claude runs execute in separate OS processes and are intentionally denied direct access to in-process `work_*` agent tools (`manvi/devcouncil/work_tools.go`).
  - `manvi/serve/module.go` freezes the `Router` after server configuration; dynamic runtime route injection is disallowed.
  - Manvi contains zero billing, subscription, or sign-in gates (`MIT OR Apache-2.0`); Waylode Primary (Free) uses Manvi locally and on BYOK compute without sign-in, while Waylode Labs tiers (`pro` / `team` / `enterprise`) orchestrate relay-backed remote steering, team `CostNanoUSD` rollups, and `policy_bundles` strictly on the host/relay side.
- **Edge Cases**:
  - `listChanges` (`manvi/serve/verify_checks.go`) truncating git `-z` output trims the trailing incomplete NUL field (`lastField`) and marks `Capped = true` -> `VerifyDegraded`.
  - Unborn `HEAD` repositories in `manvi/devcouncil/verify.go` compute the empty tree object via `git hash-object -t tree /dev/null` rather than running bare `git diff`.
- **Open Questions**: None (all ambiguities and boundaries are resolved; remaining deduplication work is explicitly scoped in `tasks.md`).
