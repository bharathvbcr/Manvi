# Data Model: 001 - Agent Harness, Tool Registry, and `manvi serve` Host Plane

**Date**: 2026-11-26 | **Source Verification**: `manvi/serve/{protocol,module,verify,workbench}.go`, `manvi/devcouncil/{verify_paths,rigor,tools,debate,work_tools}.go`, `manvi/agent/loop.go`, `manvi/cmd/manvi/tiers.go`.

## 1. Host Plane Wire Envelopes (`manvi/serve/protocol.go`)

| Entity | Fields | Invariants |
| --- | --- | --- |
| `serve.Request` | `id` (string, required non-empty), `op` (string), `params` (`json.RawMessage`, optional) | Empty `id` is refused before dispatch; unknown fields are ignored for forward compatibility (`ProtocolVersion = 1`). |
| `serve.Response` | `id` (string), `ok` (bool), `result` (`json.RawMessage`, optional), `error` (`*serve.Error`, optional) | Exactly one `Response` line per `Request`; failures return `ok: false` without closing the stream. |
| Token Bounds | `maxTokenCount = 16777216` (`1 << 24`) | Checked by `checkTokenCount`: `< 0` or `> maxTokenCount` is refused as `badRequest`, never silently clamped. |

## 2. Host Verification State (`manvi/serve/verify.go`, `verify_checks.go`)

| Entity | Fields / Bounds | Invariants |
| --- | --- | --- |
| `VerifyRecord` | `RequestID`, `ItemID`, `RunID` (`verify-<item>-<req>`), `State` (`running` / `settled`), `Outcome` (`passed` / `failed` / `degraded`), `Worktree`, `Base`, `Head`, `Changes` (`VerifyChanges`), `Rigor` (`*VerifyRigor`), `Tests` (`*VerifyTests`), `Degraded` (`[]string`), `Criteria` (`[]VerifyCriterion`), `CriticReadOnly` (`true`) | Replay with matching `(item_id, request_id)` and same `(worktree, base, head)` returns stored record (`Replayed: true`); mismatched range returns `request_conflict`. Active jobs capped at `verifyMaxActive = 4`. |
| `changeSet` | `paths` (`<= 2000`, `maxVerifyChanged`), `total`, `untracked`, `capped` | Computed strictly by `git diff --name-only -z` + `git ls-files -z`, never agent-supplied; incomplete trailing field on truncation is discarded and `Capped` sets `VerifyDegraded`. |
| `Affected` Query Batching | `maxAffectedTargets = 128`, `maxDevmapAffectedBatch = 16` | Queries DevMap in 16-path batches; missing/stale DevMap index appends to `Degraded`. |

## 3. In-Process Verification & Debate Shapes (`manvi/devcouncil/`)

| Entity | Fields / Constants | Invariants |
| --- | --- | --- |
| `PathReport` (`verify_paths.go`) | `verdict` (`passed` / `failed` / `degraded`), `source`, `examined`, `skipped`, `omitted`, `findings`, `degraded`, `selection` (`*TestSelection`) | `Passed()` requires `verdict == "passed"`. `omitted` (unexamined owed paths) triggers `degraded`; `skipped` (intentional harness bookkeeping) does not. |
| `rigorResult` (`rigor.go`) | `ok`, `error`, `files`, `in_scope`, `orphans`, `untouched_planned`, `findings` (`Finding`: `gate`, `severity`, `path`, `line`, `evidence`, `message`), `coverage_unmeasured`, `coverage_gaps` | Bounded by `maxRigorOutput = 16 MiB`, `maxRigorHealthOutput = 64 KiB`, `maxRigorStderr = 64 KiB`; health check requires `verifier == "dc-verify"`. |
| `devcouncil_debate` (`debate.go`) | `minDebaters = 2`, `maxDebaters = 4`, `maxDebateRounds = 3`, `maxDebateQuestion = 16 KiB`, `maxDebateContext = 64 KiB`, `maxCarriedAnswer = 8000` | Debaters run read-only without repository mutation authority. |
| `WorkBinding` (`work_tools.go`) | `RunID` (validated `[A-Za-z0-9_-]`) | Host-supplied only; nil disables all `work_*` agent tools. |

## 4. Turn Loop & Tier Configuration (`manvi/agent/loop.go`, `manvi/cmd/manvi/tiers.go`)

- **Turn Hooks**: `PreStep`, `LLMRequest`, `TurnStopping`, `TurnStarting`. `TurnStopping` executes under a detached/fresh settlement context on turn cancellation so leases and session totals persist cleanly.
- **Tier Keys**: `llm.tier.<tier>` (provider-qualified model spec), `llm.tier.roster`, `llm.tier.<tier>.fallback`.
