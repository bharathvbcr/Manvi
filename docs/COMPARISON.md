# Why MANVI is Different Compared to Other Harnesses

Most coding-agent frameworks and evaluation harnesses (e.g., SWE-agent, SWE-bench runners, OpenHands, Aider, Claude Code, AutoGen, CrewAI) were originally designed as rapid Python/Node prototypes. They prioritize loose prompt chaining over execution integrity, resulting in high interpreter overhead, fragile shell sandboxing, silent evaluation false-positives, broken local KV prefix caches, and volatile in-memory concurrency.

**MANVI is engineered from the ground up as a high-performance, deterministic dual-plane harness in pure Go and Rust.**

---

## Architectural & Feature Comparison Matrix

| Dimension | Typical Agent Harnesses (SWE-agent, OpenHands, Python Frameworks) | Tool-Calling Shell Wrappers (Aider, Claude Code) | **MANVI Harness** |
|---|---|---|---|
| **Runtime & Binary Footprint** | Heavy Python/Node environments, virtualenv churn, 500ms–2s cold starts, shell-out overhead per tool call. | Node.js or Python runtime; dependent on host package managers and environment shims. | **Pure Go 1.26 + Rust 1.85+**. Single static binary (`CGO_ENABLED=0`), sub-millisecond startup, zero external runtime dependencies. |
| **Safety & Policy Sandboxing** | Uncontained shell execution (relies purely on external Docker) or naive regex blocklists; easily bypassed by subshells. | Basic confirm prompts or global permissions; no multi-tiered policy engine. | **6-Rung Policy Ladder** (Root Containment → Immutable Hard Rules → Soft Rules → Grants Ledger → Postures → Operator Escalation). |
| **Outcome Semantics & Truth** | Binary pass/fail or unverified exit codes; missing tools or unexecuted checks often masquerade as passing. | Ad-hoc stdout messages; no formal classification of relaxed or degraded rules. | **5 Explicit Semantic States**: `Passed` (clean), `Blocked` (refusal), `Granted` (audited override), `Demoted` (relaxed by posture), `Degraded` (missing tooling). |
| **Non-Cheating Invariant** | Non-existent; tests that fail to run or skip assertions are commonly recorded as clean passes. | Non-existent; relies on human inspection in the loop. | **Strictly Enforced**: *"A check that did not run must never report the same result as a check that ran and passed. A grant that cleared a rule must never report as a clean pass."* |
| **Multi-Agent Mutual Exclusion** | In-memory queues, Python `asyncio` locks, or Redis; locks orphaned on process crashes. | Single-agent interactive focus; no distributed multi-task locking. | **SQLite Storage Mutex**: Enforced by a partial unique index (`WHERE status = 'active'`) with cryptographic tokens and bounded TTLs. |
| **Cancellation Handling** | Cancellation (`Ctrl+C`) aborts the process immediately, leaving held locks orphaned until timeout. | Process terminates abruptly. | **Clean Cancellation Guarantee**: Releases held leases using a fresh, uncancelled context on interrupt. |
| **Local LLM Engineering (Ollama / MLX / vLLM / llama.cpp)** | Generic OpenAI client. Rewriting history breaks KV prefix caching, causing **120s re-prefills on a 27B model**! Truncated tools cause fatal turn crashes. | Cloud-first; local models often suffer from output truncation and unparsed function call formats. | **First-Class Local Engine**: Append-only compaction preserving KV prefix reuse (**1.5s warm vs 120s cold!**), 30ms auto-discovery, wire-level XML/Hermes recovery, prefilled `<think>` sanitization, recoverable truncation. |
| **Verification & Rigor Beyond Exit Codes** | Only checks `exit code 0` of test runner. Agents can comment out tests, add fake returns, or inject `TODO` mocks. | Git diff shown to user; no automated anti-stub scanning or diff-coverage intersection. | **`dc-verify` Rust Engine**: Semantic diff-to-scope conformance, anti-stub/TODO rigor gates, and line-level diff coverage bitsets (`-coverprofile`, LCOV). |
| **Model Context Integrity** | Context accumulated in volatile memory arrays; model-visible prompt drifts from saved logs. | In-memory conversation state. | **Session Log Invariant**: Context projected on demand from append-only `session.jsonl`; asserted byte-for-byte before every LLM stream. |
| **Host Integration Protocol** | Monolithic CLI / web UI or Python imports only; embedding requires bundling the entire interpreter. | Proprietary CLI / protocol. | **`manvi serve` Stdio Host Plane**: Zero-cgo NDJSON protocol over stdio for IDEs, extensions, and host processes. |
| **Interactive Terminal Interface** | Basic scrolling terminal prints or heavyweight web UIs requiring browser/server stacks. | Standard line-by-line CLI. | **Modern Elm-Loop TUI**: Multi-session tab strip, dynamic live theme switcher (`/theme`), zero-allocation damage-diff rendering. |

---

## The Core Differentiators in Detail

### 1. Dual-Plane Engine: Concurrency Meets Deterministic Analysis (Zero Cgo)

MANVI partitions its architecture strictly on the axis of **IO-bound concurrency vs CPU-bound determinism**:
- **Go Execution Plane (`CGO_ENABLED=0`)**: Drives high-concurrency event loops, SSE streams, multi-provider LLM adapters, policy gates, and damage-diffed terminal rendering without garbage collection stalls or cgo overhead.
- **Rust Analysis Plane (`dc-verify`, `dc-store`, `dc-glob`, `devmap`)**: Executes CPU-intensive unified diff parsing, regex-free glob matching, AST code graph indexing, and SQLite ACID state persistence.
- **Strict Process Boundary**: The two planes communicate exclusively over child process boundaries (`fork`/`exec`) with line-delimited JSON over stdio. This preserves instantaneous static cross-compilation, avoids shared-memory safety pitfalls, and eliminates runtime dependency hell.

See [`ARCHITECTURE.md`](ARCHITECTURE.md) for the complete architecture specification.

---

### 2. The Non-Cheating Safety Invariant & 5-State Policy Ladder

Standard evaluation harnesses frequently report false positives when an agent modifies files outside its scope, skips broken test suites, or leverages shell aliases to bypass checks. MANVI enforces a 5-tier evaluation ladder:

```
Boundary Check ➔ Hard Rules (Secrets/.git) ➔ Soft Rules (Scope/AST) ➔ Grants Ledger ➔ Postures (dev/yolo) ➔ Operator Escalation
```

Every tool call and file mutation is resolved into one of **5 explicit semantic outcomes**:
1. **`Passed`** (`✓` Green): All security and scope rules evaluated and cleanly passed.
2. **`Blocked`** (`✗` Red): Prevented by an immutable hard rule or ungranted soft rule.
3. **`Granted`** (`✓ [granted]` Yellow): Permitted via an explicit, audited Human or Agent grant with reason and TTL.
4. **`Demoted`** (`✓ [demoted]` Amber): Permitted only because the harness was explicitly booted with relaxed posture (`dev` or `yolo`).
5. **`Degraded`** (`✓ [degraded]` Magenta): Evaluated with disabled safety tools or missing AST indices.

> **Cardinal Rule**: A check that did not run is *never* reported as passed. A weakened check is *never* reported as clean.

See [`POLICY_AND_SAFETY.md`](POLICY_AND_SAFETY.md) for the complete policy engine specification.

---

### 3. First-Class Local LLM Engineering & KV Prefix-Cache Preservation

Local inference on modern hardware (Apple Silicon via MLX, NVIDIA GPUs via vLLM/llama.cpp) is bottlenecked by prompt prefill times.
- **Prefix-Cache Preservation**: Standard agent harnesses rewrite or reorder conversation history during compaction, which invalidates the server's KV cache and forces a full re-prefill on every single turn step (**120s for a 14.7k-token prompt on a 4-bit 27B model**). MANVI records one-way compactions in the session log and projects them consistently so the token prefix *only ever grows*, reducing step latency to **1.5s warm**.
- **30ms Zero-Config Endpoint Discovery**: Probes loopback ports to automatically identify running Ollama, vLLM, LM Studio, llama.cpp, Jan, or MLX instances and queries runtime capabilities (`/api/version`, `/props`, `max_model_len`).
- **Wire-Level Parser Recovery**: Recovers function calls when local servers lack native model parsers—handling Hermes JSON, Qwen3 nested XML `<function=…><parameter=…>`, and markdown fences, with schema-driven type coercion (preventing `0755` from becoming integer `755`).
- **Prefilled `<think>` Tag Sanitization**: Strips unclosed CoT thinking tags so chain-of-thought is not polluted into replay history.
- **Recoverable Truncation**: Cut-off tool arguments due to server context ceilings are fed back as retryable diagnostic hints rather than aborting the entire turn.

See [`LOCAL_LLMS.md`](LOCAL_LLMS.md) for the complete local model guide.

---

### 4. Semantic Verification & Rigor Over Raw Test Exit Codes

In conventional benchmarks and harnesses, an agent can achieve "green" test results by deleting failing test cases, replacing complex methods with `return true;`, or inserting `TODO` stubs. MANVI’s `dc-verify` Rust engine performs deep multi-layered verification:
- **Diff-to-Scope Conformance**: Validates that modifications strictly conform to the checked-out task scope or its AST code graph neighborhood.
- **Rigor Gates**: Analyzes added diff hunks to block mock stubs, `TODO` markers, fake passes, or credential leaks.
- **Diff-Coverage Intersection**: Intersects git diff lines with line-level code coverage bitsets (`go test -coverprofile`, LCOV) to verify that newly added code was actually exercised by tests.

See [`VERIFICATION_AND_PARITY.md`](VERIFICATION_AND_PARITY.md) for verification details.

---

### 5. Storage-Enforced Multi-Agent Concurrency & Clean Cancellation

- Multi-agent orchestration is backed by SQLite ACID transactions using a partial unique index (`CREATE UNIQUE INDEX ux_task_leases_active ON task_leases (task_id) WHERE status = 'active'`). Leases are governed by cryptographic tokens and bounded TTLs.
- **Clean Cancellation**: When an agent run is interrupted via `Ctrl+C`, MANVI invokes lease cleanup on a dedicated, uncancelled context—ensuring tasks are unlocked immediately rather than wedging subsequent runs until TTL expiration.

See [`AGENT_AND_TURN_LIFECYCLE.md`](AGENT_AND_TURN_LIFECYCLE.md) for lifecycle details.

---

### 6. Universal Host Plane (`manvi serve`) & Tri-Mode Interaction

MANVI offers three distinct interaction surfaces:
1. **Elm-Loop Full-Screen TUI** (`manvi` / `manvi tui`): Built-in multi-session tab strip, dynamic live theme switching (`/theme`, `Ctrl+Y`), searchable session switcher modal (`Ctrl+S`), markdown highlighting, and zero-allocation diff rendering (0 bytes on idle ticks). See [`TUI_AND_EVENT_SUBSYSTEM.md`](TUI_AND_EVENT_SUBSYSTEM.md).
2. **Headless Execution Plane** (`manvi run`): Scripts and CI pipelines benefit from distinct exit codes: `0` (success), `1` (failure, including output that could not be written), `2` (step ceiling reached, preventing partial commit of incomplete work), `3` (output cap), `4` (no answer) and `5` (an unfinished stop reason). See [`CLI_AND_CONFIGURATION.md`](CLI_AND_CONFIGURATION.md).
3. **Embedded Stdio Host Plane** (`manvi serve`): Exposes policy enforcement, capability discovery, token budget preparation, and completion settling over NDJSON on stdio—allowing IDEs (VS Code, JetBrains), editors, and desktop applications to embed MANVI with zero cgo or shared library linking. See [`SERVE_HOST_PLANE.md`](SERVE_HOST_PLANE.md).
