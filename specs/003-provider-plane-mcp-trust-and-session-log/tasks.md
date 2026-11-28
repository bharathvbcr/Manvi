# Tasks: 003 - Model Provider Plane, Spend Ledger, MCP Trust, and Session Log

**Spec**: `specs/003-provider-plane-mcp-trust-and-session-log/spec.md` | **Plan**: `specs/003-provider-plane-mcp-trust-and-session-log/plan.md`

## Phase 1: Shipped Provider Plane

- [x] [T-001] [US1] Implement `Provider`, `Registry.Resolve`, `Capability.Validate` and `Catalogue` in `manvi/llm/provider.go` tests: go -C manvi test ./llm -run Catalogue -count=1
- [x] [T-002] [US2] Implement the single tool-call settlement rule `SettleCall` in `manvi/llm/settle.go` and per-adapter conformance in `manvi/llm/adaptertest/settlement_conformance_test.go` depends_on: [T-001] tests: go -C manvi test ./llm/adaptertest/... -count=1
- [x] [T-003] [US2] Implement `TruncatedStream` and the SSE decoder guard in `manvi/llm/transport/streamguard.go` and `manvi/llm/transport/attempt_gate.go` tests: go -C manvi test ./llm/transport/... -count=1
- [x] [T-004] [US1] [P] Implement the five adapters and the deterministic replay provider in `manvi/llm/anthropic/adapter.go`, `manvi/llm/gemini/adapter.go`, `manvi/llm/openaicompat/openaicompat.go`, `manvi/llm/local/adapter.go`, `manvi/llm/xai/adapter.go` and `manvi/llm/replay/replay.go` depends_on: [T-002] tests: go -C manvi test ./llm/... -count=1
- [x] [T-005] [US4] [P] Implement loopback discovery and runtime identification in `manvi/llm/local/discover.go` and `manvi/llm/local/endpoints.go` depends_on: [T-004] tests: go -C manvi test ./llm/local/... -count=1

## Phase 2: Spend Ledger

- [x] [T-006] [US3] Implement the cross-process ledger with reserve, settle, retain and `Headroom` in `manvi/llm/budget/budget.go` and `manvi/llm/budget/lock.go` depends_on: [T-001] tests: go -C manvi test ./llm/budget/... -count=1
- [x] [T-007] [US3] Implement the budgeted provider wrapper, `Estimate` and price arithmetic in `manvi/llm/budget/provider.go` and `manvi/llm/budget/prices.go` depends_on: [T-006] tests: go -C manvi test ./llm/budget/... -count=1
- [x] [T-008] [US3] Implement owner-token liveness reclamation in `manvi/llm/budget/liveness.go` and `manvi/llm/budget/procstart_darwin.go` depends_on: [T-006] tests: go -C manvi test ./llm/budget/... -count=1

## Phase 3: MCP Trust and Session Log

- [x] [T-009] [US5] Implement declaration fingerprinting, the trust file and workspace-origin authorization in `manvi/mcp/manager.go` and spawn plumbing in `manvi/mcp/client.go` tests: go -C manvi test ./mcp/... -count=1
- [x] [T-010] [US6] Implement the append-only `Log`, projection and `AssertModelVisible` in `manvi/session/log.go` and `manvi/session/invariant.go` tests: go -C manvi test ./session/... -count=1
- [x] [T-011] [US6] Implement checksummed generation files with `os.Link` publication in `manvi/session/store.go` depends_on: [T-010] tests: go -C manvi test ./session/... -count=1

## Phase 4: Specification Gate

- [x] [T-012] [US1] Expose the spec gate as the `check:specs` script in `package.json` (runs `devcouncil spec validate --all --strict`) tests: devcouncil spec validate --all --strict
