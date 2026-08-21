# Hardening Ledger & Defect Catalogue

MANVI enforces a strict engineering philosophy:
- **Fix the class, not the case**: Root causes are resolved at the canonical owner layer.
- **Fail closed and loud**: Ambiguous inputs, malformed responses, or corrupt coverage files produce explicit failures, never silent passes.
- **Assert invariants byte-for-byte**: Model-visible prompts and session logs are verified before every LLM stream.
- **Every fix ships with regression tests**: Every hardened invariant is backed by automated adversarial unit and integration tests.

---

## The Hardened Invariants Ledger

| Defect Pattern | Failure Mode & Consequence | Resolution & Hardened Invariant | Verified In |
|---|---|---|---|
| **Case-Insensitive Secret Matching** | `.ENV` or `.CLAUDE/settings.json` matched on case-insensitive filesystems (APFS/NTFS), allowing unauthorized credential writes. | Canonical case-folded path comparison across all path rule evaluations. | `policy/adversarial_test.go` |
| **Relative Test Discovery Paths** | `dc/store` tests located Rust workspace via hand-counted relative paths, silently skipping Go↔Rust tests when executed from subdirs. | Shared `testsupport.RustWorkspaceRoot()` locator using Cargo manifest anchors. | `internal/testsupport` |
| **Orphaned Child Subprocesses** | Child processes inherited standard output pipes and remained alive after timeout, hanging the agent turn indefinitely. | Process-group isolation (`Setpgid`) and group kill (`-pgid`) on context cancellation. | `dc/store/adversarial_test.go` |
| **Permissive `Available()` Probes** | Mock or corrupted binaries outputting `{"ok":true}` passed as functional SQLite lease stores. | Strict schema and operational handshake probe requiring round-trip lease generation. | `dc/store/adversarial_test.go` |
| **Empty Tool Result Re-Dispatch** | File write operations returning empty success strings were dispatched twice by the agent loop. | Explicit empty-result sentinel handling in tool dispatcher. | `tools/tools_test.go` |
| **Literal Paths Parsed as Globs** | Human grants created for files named `a[bc].go` accidentally matched `ab.go` and `ac.go`. | Strict glob-escaping when constructing path-literal grants. | `grants/adversarial_test.go` |
| **Bare `.git` Worktree Pointers** | In git worktrees, `.git` is a pointer file rather than a directory; writes to `.git` repointed the entire repository. | Explicit protection for both `.git` directories and `.git` worktree pointer files. | `policy/adversarial_test.go` |
| **History Rewriting Breaking KV Cache** | Conventional compaction rewrote history on every step, invalidating server KV caches (**120s prefill vs 1.5s warm**). | Append-only compaction events projected strictly forward so prefix tokens only ever grow. | `agent/stress_test.go` |
| **Compaction Invariant Mismatch** | Compaction of multi-line tool outputs caused model-visible history to drift from logged events, triggering turn panics. | Re-projected history from compacted log records and validated byte-for-byte equality. | `session/invariant_test.go` |
| **Replaying Unreplayable Reasoning** | Replaying assistant reasoning on OpenAI-compatible endpoints that forbid `<think>` history caused HTTP 400 rejections. | Model-aware provenance tracking stripping non-replayable reasoning blocks before dispatch. | `llm/replay_guard_test.go` |
| **Unused `ReplayableOn` Declarations** | Adapters declared replay rules but the agent loop used a dead hardcoded heuristic instead. | Wired `ReplayableOn` as the canonical authority for history projection. | `llm/replay_guard_test.go` |
| **Unbudgeted Tool Schemas** | 1,755 real tokens of tool schemas were excluded from context estimators, causing prompt budget overflow. | Explicitly added tool schema token measurements to pre-flight context calculations. | `agent/compaction_test.go` |
| **Parallel Tool-Call Delta Overwrites** | Multiple tool calls started in a single SSE chunk overwrote each other in the UI and event bus. | Indexed chunk buffer indexing tool calls by call-ID and sequence index. | `llm/openaicompat/localmodel_test.go` |
| **Unparsed XML Tool Calls (Qwen3)** | Qwen3 nested XML format was treated as ordinary prose, leaving tool calls unexecuted while reporting success. | Wire-level regex and XML streaming recovery parser extracting tool invocations. | `llm/openaicompat/localmodel_test.go` |
| **Untyped XML Parameter Coercion** | XML parameters were parsed with generic string heuristics, converting octal `"0755"` to integer `755`. | Schema-driven type coercion utilizing the tool's declared JSON schema types. | `llm/openaicompat/localmodel_test.go` |
| **Prefilled `<think>` Tag Pollution** | Qwen template ending in `<think>` caused the model to emit only `</think>`, leaking CoT into conversation history. | Stream parser detects unmatched closing tags and reclassifies content as reasoning. | `llm/openaicompat/localmodel_test.go` |
| **Stray & Nested Thinking Tags** | Fuzzing revealed byte-split edge cases where nested `<think>` tags leaked raw markdown into output text. | Finite-state machine byte-level streaming filter for reasoning delimiters. | `llm/openaicompat/adversarial_test.go` |
| **Fatal Output Truncation on Tool Calls** | Model completions cut off by server token ceilings aborted the entire turn and discarded prior steps. | Recoverable truncation handler catches partial calls and injects retryable continuation hints. | `llm/xai/adapter_test.go` |
| **Missing `max_tokens` in llama.cpp** | llama.cpp ignored `max_completion_tokens` and generated unbounded text when `max_tokens` was missing. | Dual transmission of both `max_tokens` and `max_completion_tokens` on all requests. | `llm/openaicompat/localmodel_test.go` |
| **Indefinite Streaming Stalls** | A frozen local server emitting one token and hanging consumed the entire turn timeout in silence. | Token-gap stall watchdog timer (`llm.local.stall_timeout`, default `15s`). | `llm/transport/stall_test.go` |
| **Silent Discard of Invalid Sampling Values** | Setting `MANVI_LLM_LOCAL_TEMPERATURE=0.7x` was silently ignored, masquerading as applied configuration. | Strict parser validating all numeric settings and failing closed with line numbers. | `cmd/manvi/localconfig_test.go` |
| **Bloated Compaction Elision Notices** | Compacting small tool results added elision text larger than the original payload, wasting context. | Length-delta threshold: compaction only triggers if net tokens saved exceed overhead. | `agent/stress_test.go` |
| **Superficial Wire Probes** | `manvi probe` tested text generation only, passing even when tool calling was broken. | Wire probe mandates a real tool call response from the model before reporting success. | `cmd/manvi/probe.go` |
| **Negative Grant TTL Promotion** | Negative duration math bugs wrapped around to integer maximum, creating immortal grants. | Bounded validation rejecting non-positive TTL durations. | `grants/adversarial_test.go` |
| **Diff Parser Header Requirement** | Unified diffs missing `diff --git` headers were parsed as empty files, passing scope checks as clean. | Fail-closed diff parser returning explicit parse errors for non-conforming diff inputs. | `crates/dc-verify` |
| **Silent Ignored Flags in `dcstore`** | Unknown CLI arguments to `dcstore` were silently ignored, causing empty task queries. | Strict flag parsing with `clap` failing immediately on unrecognised arguments. | `dc-store/src/bin/dcstore.rs` |
| **Silent 2048-Token Output Cap in MLX** | Local `mlx-vlm` server silently truncated long code blocks at 2048 tokens. | Configurable `llm.local.max_output_tokens` (default 16384) sent on every request. | `manvi/llm/local` |
| **Interrupted Lease Cleanup Context** | `Ctrl+C` cancelled the context used to release SQLite task leases, leaving tasks locked for 15m. | Dedicated uncancelled background context used for teardown and lease release. | `manvi/agent` |
| **Cold-Start WAL Conversion Contention** | High-concurrency tests hit SQLite `database is locked` during initial WAL journal mode creation. | Retry loop with exponential backoff on SQLite connection initialization. | `crates/dc-store/src/lib.rs` |
| **Stdio Server Session Isolation** | Re-instantiating stdio server between requests lost prompt token calibration history. | Long-lived unified session driver preserving calibration and compaction ledgers. | `manvi/serve/chat_test.go` |
| **Quoted Path Trailing Slash Non-Aliasing** | Paths like `"\"0\"/"` settled across two normalization passes, violating non-aliasing invariants. | Single-pass idempotent path cleaner resolving quotes and trailing slashes atomically. | `manvi/policy/file.go` |
