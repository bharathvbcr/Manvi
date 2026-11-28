# Data Model: 003 - Model Provider Plane, Spend Ledger, MCP Trust, and Session Log

**Date**: 2026-11-28 | **Source Verification**: `manvi/llm/provider.go`, `manvi/llm/settle.go`, `manvi/llm/budget/{budget,prices}.go`, `manvi/mcp/manager.go`, `manvi/session/{log,store}.go`.

## 1. Provider Seam (`manvi/llm/provider.go`, `settle.go`)

| Entity | Fields | Invariants |
| --- | --- | --- |
| `llm.Capability` | `provider`, `model`, `context_window`, `max_output_tokens`, `supports_tools`, `supports_streaming`, `supports_images`, `supports_reasoning`, `mid_conversation_system`, `rejects_sampling`, `effort_levels` | `EffortLevels` are ordered least to most reasoning; the loop raises a stuck turn's tier by one position, so each adapter's order is tested in `manvi/llm/effortorder_test.go`. |
| `llm.Provider` | `Name()`, `Capability(model)`, `Stream(ctx, req)` | An unknown model returns `false`, never a permissive default. |
| `llm.Registry` | providers by name, registration order | `Register` rejects a duplicate name; `Resolve` returns provider and validated capability or an error naming the fault. |
| `CallFraming` | `FramingRequired` (anthropic), `FramingOptional` (gemini), `FramingNone` (OpenAI-compatible) | The only way adapters' settlement may differ. |
| `CallVerdict` | `CallComplete`, `CallCutByLimit`, `CallUnusable` | Only `CallComplete` carries `Arguments` (a whole JSON object) and may dispatch. |

## 2. Spend Ledger (`manvi/llm/budget/`)

| Entity | Fields / Bounds | Invariants |
| --- | --- | --- |
| `Snapshot` | `schema_version` (`2`), `spent_nano_usd`, `retained_nano_usd`, `settled`, `retained_attempts`, `refused_attempts`, `overruns`, `next_id`, `in_flight` | `retained_nano_usd` is an upper bound, not a measurement; `Overruns > 0` means the reservation bound was too tight. File size `<= 1 MiB`. |
| `Attempt` | `id`, `model`, `state` (`reserved`/`accepted`), `reserved_nano_usd`, `pid`, `started_unix`, `owner`, `owner_start` | `<= 256` in flight. Ownership is judged by `owner`+`owner_start`, with pid and `started_unix` only for records written before those fields existed. |
| `Prices` / `Rates` | `input`, `output`, `cache_write`, `cache_read` (nano-USD per token), `long_prompt_above`, `long_rates`, `reasoning_in_output`, `search_request` | `maxRate = 1_000_000`, `maxTokens = 10_000_000`; a budgeted provider without prices is refused. |

## 3. MCP Trust (`manvi/mcp/manager.go`)

| Entity | Fields | Invariants |
| --- | --- | --- |
| `TrustFile` | `authorized: [TrustEntry]` | Stored as `mcp-trust.json` under the user config dir, overridable by `MANVI_MCP_TRUST_FILE`; `MANVI_MCP_TRUST` lists fingerprints for headless runs. |
| `TrustEntry` | `fingerprint`, `name`, `note` | Only `fingerprint` is matched. |
| `Origin` | `program`, `workspace` | A `workspace` declaration spawns only when its `Fingerprint` is authorized. |

## 4. Session Log (`manvi/session/`)

| Entity | Fields | Invariants |
| --- | --- | --- |
| `session.Event` | sequence, `Type` (for example `turn/start`, `user/message`, `assistant/message`, `tool/call`, `tool/result`, `policy/denied`, `context/tool_result_compacted`, `llm/decoding_compensated`, `verify/report`), `Data` | The log is append-only; the model's messages are a projection (`DeriveMessages`). |
| Session file | `<id>.<generation>.json` with `manvi_session`, `id`, `created_at`, `checksum` (`sha256:`), `events` | Checksum covers the encoded event bytes; `Load` errors rather than returning an empty log. |
| `InvariantError` | `MessageIndex`, `Detail`, `Injected` | Names the first divergence between a request and the log. |
