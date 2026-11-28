# Implementation Plan: 003 - Model Provider Plane, Spend Ledger, MCP Trust, and Session Log

**Branch**: `main` | **Date**: 2026-11-28 | **Spec**: `specs/003-provider-plane-mcp-trust-and-session-log/spec.md`

## 1. Summary & Architectural Topology

The three packages are shipped and tested; this plan records how they fit together so later changes keep the seams.

```text
  Turn loop (manvi/agent/, specs/001)
       |  llm.Request / llm.Stream
       v
  llm.Registry.Resolve  --Capability.Validate-->  refusal at assembly
       |
       v
  budget.Provider (optional wrapper, manvi/llm/budget/)   <-- Ledger file shared by sessions
       |
       v
  adapter: anthropic | gemini | openaicompat | local | xai   (or replay.Provider in tests)
       |            \-- SettleCall (manvi/llm/settle.go): one rule for "complete tool call"
       v
  transport (manvi/llm/transport/): attempt gate, SSE, streamguard (TruncatedStream)

  Tool results <-- mcp.Manager (manvi/mcp/) spawns only authorized declarations
  Everything model-visible --> session.Log (append-only) --> session.Store (checksummed generations)
```

## 2. Verified File Map

- **Seam**: `manvi/llm/provider.go`, `manvi/llm/settle.go`, `manvi/llm/message.go`, `manvi/llm/codec.go`, `manvi/llm/clone.go`
- **Adapters**: `manvi/llm/anthropic/adapter.go`, `manvi/llm/gemini/adapter.go`, `manvi/llm/openaicompat/openaicompat.go`, `manvi/llm/local/adapter.go`, `manvi/llm/xai/adapter.go`, `manvi/llm/replay/replay.go`
- **Transport**: `manvi/llm/transport/transport.go`, `manvi/llm/transport/attempt_gate.go`, `manvi/llm/transport/streamguard.go`
- **Ledger**: `manvi/llm/budget/budget.go`, `manvi/llm/budget/provider.go`, `manvi/llm/budget/prices.go`, `manvi/llm/budget/liveness.go`, `manvi/llm/budget/lock.go`
- **Local discovery**: `manvi/llm/local/discover.go`, `manvi/llm/local/endpoints.go`; CLI surface `manvi/cmd/manvi/localmodels.go`
- **MCP**: `manvi/mcp/manager.go`, `manvi/mcp/client.go`, `manvi/mcp/plugin.go`, `manvi/mcp/protocol.go`
- **Session**: `manvi/session/log.go`, `manvi/session/store.go`, `manvi/session/invariant.go`, `manvi/session/privacy.go`, `manvi/session/stale.go`
- **Docs**: `docs/LOCAL_LLMS.md`, `docs/AGENT_AND_TURN_LIFECYCLE.md`, `docs/PHILOSOPHY.md`

## 3. Constitution Alignment Check

Judged against `specs/003-provider-plane-mcp-trust-and-session-log/constitution.md`, which is derived from `docs/PHILOSOPHY.md`.

| Article | Judgment | Evidence |
| --- | --- | --- |
| I. Incomplete is never complete | PASS | `SettleCall` and `TruncatedStream` refuse a call or response with no terminal framing; `Load` refuses a damaged file instead of returning an empty log. |
| II. Over-report spend, never under-report | PASS | Only proof of nothing sent or authoritative usage reduces a reservation (`manvi/llm/budget/budget.go` package doc). |
| III. Untrusted trees cannot authorize themselves | PASS | Trust file lives under the user's config dir (`trustFileName = "mcp-trust.json"`); a case-variant path into the repository is not honoured (`pathinside_test.go`). |
| IV. Model-visible means logged | PASS | `AssertModelVisible` in `manvi/session/invariant.go`. |
| V. Local-model-first, no paywall | PASS | `local` adapter with discovery; no billing code in these packages. |

## 4. Complexity Tracking

| Deviation / Seam | Why Needed Now | Simpler Alternative Rejected | Closure / Guard |
| --- | --- | --- | --- |
| Per-adapter `CallFraming` argument to `SettleCall` | Wires genuinely differ in whether they close a call (anthropic required, gemini optional, OpenAI-compatible none) | One framing rule for all wires would dispatch half-received calls on some wires | Framing is the only permitted difference; conformance tests in `manvi/llm/adaptertest/` run every adapter |
| Ledger file with cross-process lock (`lock_unix.go`, `lock_windows.go`, `lock_other.go`) | Several sessions share one spend cap | A per-process counter lets N sessions spend N times the limit | `Owner`/`OwnerStart` liveness in `manvi/llm/budget/liveness.go` reclaims dead owners |
