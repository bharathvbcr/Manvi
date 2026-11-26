# Research & Overlap Audit: 001 - Agent Harness, Tool Registry, and `manvi serve` Host Plane

**Date**: 2026-11-26 | **Audit Scope**: Cross-check of `Manvi/manvi/devcouncil/*`, `Manvi/manvi/serve/*`, and `DevCouncil/backend/go_orchestrator/devcouncil/*`.

## 1. Overlap Audit (`OV-VERIFY-01`): `manvi/serve/verify.go` vs `manvi/devcouncil/verify.go` vs DevCouncil `verify` + `dcverify`

### Evidence Observed in Source

1. **Deterministic Rigor Invocation (`dcverify`)**:
   - `manvi/devcouncil/rigor.go` (`rigorClient`) runs `dcverify` over stdin/stdout with explicit schema handshake (`rigorClient.available` verifies `verifier == "dc-verify"` and matching `rigorSchemaVersion`), `16 MiB` stdout cap, `64 KiB` stderr cap, and per-`.lean` deadline bounding (`leanKernelMaxStaged = 64`, `minLeanRigorTimeout = 240s`).
   - `manvi/devcouncil/verify_paths.go` (`VerifyPresent`, `PathReport`) runs lease-free end-of-turn checks over only the turn's recorded changed paths, keeping `Skipped` (harness bookkeeping) strictly separate from `Omitted` (unexamined source files) so `PathReport.Passed()` returns `false` unless `Verdict == "passed"`.
   - `manvi/serve/verify.go` & `manvi/serve/verify_checks.go` (`VerifyRunner`) **do not** duplicate `dcverify` parsing or path verification: `VerifyRunner.runRigor` delegates directly to `devcouncil.VerifyPresent` (`manvi/devcouncil/verify_paths.go`), batches `devmap` `QueryAffected` calls at `maxDevmapAffectedBatch = 16`, bounds active jobs (`verifyMaxActive = 4`), and enforces a `2s` stop grace (`verifyStopGrace`) after context expiry before settling as `VerifyDegraded`.
2. **Identified Overlap (`manvi/devcouncil/verify.go` vs `DevCouncil/backend/go_orchestrator/devcouncil/verify/` & `dc`)**:
   - `DevCouncil/backend/go_orchestrator/dc/verify_actions.go` exports `RigorActionText`, `GapTypeForRigorGate`, and `CategoryForGapType`, which `DevCouncil/backend/go_orchestrator/devcouncil/verify/` and `manvi/devcouncil/rigor.go` (`gapsFrom`) now both call directly (closing the `rigor.go` wording/switch duplication while keeping `dc/dcverify` unimported in `manvi/`).
   - `manvi/devcouncil/verify.go` (`22-64`, `131-181`, `991-1040`) still defines parallel `Gap`, `NextAction`, and `Report` structs (`devcouncil_verify_task`), scope/orphan matching (`policy.MatchesPlannedPath`), and `changedFiles` diff parsing alongside `DevCouncil/backend/go_orchestrator/devcouncil/verify/types.go` (`20-84`) and `orchestrate.go`.
   - **Assessment**: Note that `manvi/serve/verify_checks.go:237-258` (`VerifyRunner.runRigor`) *already* delegates AST rigor and coverage to `dcverify` via `VerifyPresent`, keeping host-plane async job claiming (`manvi/serve/verify.go:167-422`) + read-only critic (`verify_checks.go:455-535`) in Manvi, and `manvi/devcouncil/rigor.go` now reuses `dc.RigorActionText` & `dc.CategoryForGapType`. The remaining Go-side DTO/matching duplication sits in `manvi/devcouncil/verify.go` (`OV-VERIFY-01` / `T-06`).

## 2. Overlap Audit (`OV-TOOLS-01`): Shared `devcouncil_*` Tools vs Standalone DevCouncil MCP

### Evidence Observed in Source

- `DevCouncil/backend/go_orchestrator/devcouncil/registry.go` registers **8** MCP tools for external MCP hosts:
  1. `devcouncil_get_diff`
  2. `devcouncil_checkout_task`
  3. `devcouncil_renew_lease`
  4. `devcouncil_release_task`
  5. `devcouncil_next_task`
  6. `devcouncil_verify_task`
  7. `devcouncil_get_gaps`
  8. `devcouncil_policy_check_write`
- `manvi/devcouncil/tools.go` (`258-560`, plus `navigate.go`, `subagent_tools.go`, `mcp_tools.go`, `artifact_tools.go`, `question_tools.go`, `git_tools.go`, `devbridge.go`, `web_tools.go`, `work_tools.go`, `debate.go`, `codegraph.go`) exposes **55** tools (`54` with no repo skills; `+1` conditional `devcouncil_fetch_url` when `MANVI_FETCH_HOSTS` is non-empty). All 8 shared names are verified against `dc.SharedToolSpecByName` via `sharedTool` in `tools.go` and pinned by `manvi/devcouncil/shared_tools_test.go`, preserving intentional session-bound input schema omissions (`devcouncil_checkout_task` uses `task_id` + optional `owner` vs DevCouncil's `task_id` + `client_id` + `agent` + `force`; `renew_lease`/`release_task`/`next_task`/`verify_task`/`get_gaps`/`get_diff` take `{}` on Manvi's single `Registry.session`). `devcouncil_policy_check_write` accepts optional `operation` (`"create"` -> `dc.OpCreate`, `"modify"`/default -> `dc.OpWrite`, `"delete"` -> `dc.OpDelete`) in `policy_tools.go`.

### Wave A/B Pin, Index & Labs Seam Findings (Lane B)

- **`DRIFT-PIN-01`**: `scripts/module-pins.txt:8-9` pins `DevCouncil` at `63ca82ea` (259 commits behind `DevCouncil` `HEAD` `5beb1e54`) and `gusset` at `19383ad1` (19 commits behind `0b2db23`), while `manvi/go.mod:19-21` uses local `replace` directives against sibling checkouts and `crates/dc-*` symlinks (`T-09`).
- **`DRIFT-INDEX-01`**: Refreshed via `devmap build --repo-map` (generation `#817`, 1,699 files, 19,361 symbols, 88,789 edges).
- **`GAP-LABS-APPROVAL-01` / `GAP-LABS-SPEND-01`**: `manvi/cmd/manvi/approve_fd*.go` provides terminal/FD approval and `manvi/session/log.go:224` emits `CostNanoUSD`, while `manvi/serve/` lacks remote command-approval and team spend-rollup endpoints for Waylode Labs (`T-10`).

### Alternatives Considered

1. **Alternative A — Proxy the 8 shared tools through DevCouncil's `devcouncil mcp` stdio child process**:
   - *Pros*: Single implementation of the 8 handlers.
   - *Cons*: Breaks in-process `authorisingTask` resolution (`manvi/devcouncil/policy_tools.go`), prevents live UI approval modals (`devcouncil_request_override` recovery hints), drops `PathReport` turn-path tracking, and forces JSON-RPC overhead on every lease/policy check.
   - *Verdict*: **Rejected** for in-process turns; `devcouncil mcp` remains canonical for external hosts (e.g., Claude/Cursor connecting directly to DevCouncil).
2. **Alternative B — Keep Manvi in-process `Registry` as canonical harness surface, enforce contract/schema parity tests for the 8 shared names**:
   - *Pros*: Preserves zero-IPC `authorisingTask` consistency, `okDegraded` propagation, and existing `TestDocumentedToolCountMatchesRegistry`; keeps DevCouncil decoupled from Manvi imports (`registry.go` line 48: *"It does not import Manvi packages."*).
   - *Verdict*: **Selected**, with parity check tracked in `T-07`.

## 3. Protocol & Seam Decisions (`manvi/serve/`)

- **Non-streaming NDJSON (`protocol.go`)**: Deliberately avoids streaming tokens over stdio/socket because the host owns HTTP LLM streams; removing a once-planned third `"event"` line type eliminated dead decoder branches in hosts.
- **Per-Server `Router` (`module.go`)**: Using per-`Server` `Register` / `Replace` instead of global handler maps stops sibling embedding hosts from mutating each other's protocol surface and catches misconfigured handlers at boot (`configureModule` recovers panics before `Serve` reads stdin).
- **Scoped `work_*` Agent Tools (`work_tools.go`)**: Isolated exclusively to Manvi's in-process `Registry` and bound host-side via `WorkBinding` (`RunID` validated against store identifier rules) rather than exposing `serve.WorkbenchModule`'s 77 host `work.*` operations directly to agents or managed external CLIs.
