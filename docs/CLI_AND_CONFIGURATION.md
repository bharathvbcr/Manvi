# CLI & Configuration Reference

This document provides a complete reference for MANVI's command-line interface, configuration catalog, mutability boundaries, posture matrix, and repository auto-initialization rules.

---

## CLI Command Reference

| Command | Syntax / Options | Description |
|---|---|---|
| **Interactive TUI** | `manvi` or `manvi tui` | Launch the modern full-screen Elm-loop TUI (tab strip, live theme switcher, session switcher, rich markdown & diff rendering). |
| **System Diagnostics** | `manvi doctor` | Validate active configuration, verify SQLite store reachability, and detect weakened security gates. |
| **Headless Turn** | `manvi run -p "PROMPT"` | Drive a single agent turn without terminal interaction. Ideal for scripts, CI, and benchmarks.<br/>• `--json`: Output structured JSON turn summary.<br/>• `--max-steps N`: Cap maximum execution steps (default 40).<br/>• `--timeout D`: Bounded turn duration (e.g. `10m`).<br/>• `--quiet`: Suppress streaming prose. |
| **Settings Inspection** | `manvi flags [--all]` | Display all configuration flags, active values, safety ratings, and origin layers. |
| **Runtime Flag Modification** | `manvi flags set KEY VALUE` | Set a configuration value for the current process. Fails closed if the flag is marked `startup`-only or moves a safety setting illegally. |
| **Local LLM Discovery** | `manvi local [--resolve]` | Scan loopback ports to discover active local LLM servers (Ollama, MLX, vLLM, llama.cpp, LM Studio, Jan). `--resolve` displays the chosen candidate with origin annotations. |
| **Stdio Host Server** | `manvi serve [--posture host\|devcouncil]` | Expose policy evaluation, context preparation, and completion parsing over NDJSON stdio for IDE extensions and external tools. |
| **Task Lease Listing** | `manvi lease list` | Display active task checkouts, lease owners, tokens, and remaining TTLs. |
| **Task Lease Acquisition** | `manvi lease acquire TASK OWNER [--ttl 15m]` | Atomically checkout a task and claim an exclusive lease. Fails if task is already held. |
| **Task Lease Release** | `manvi lease release TASK TOKEN` | Atomically relinquish a held task lease using its cryptographic token. |
| **Policy Pre-Check** | `manvi check PATH [--task ID]` | Probe write gate policy for a file path without touching the filesystem; outputs decision, fired rules, and explanation. |
| **Human Override** | `manvi allow PATH --reason TEXT` | Record an audited human grant in the ledger to unblock a soft policy denial. |
| **Native Tools List** | `manvi tools` | List all 44 native tools and their parameter schemas. |
| **Direct Tool Execution** | `manvi tool NAME [--json ARGS]` | Execute a native tool directly over the same dispatch path an agent takes. |
| **Provider Detection** | `manvi providers` | List supported LLM providers and detect present API keys/credentials in environment. |
| **Event Wire Watcher** | `manvi watch [--json]` | Stream the typed `ui.Event` wire to terminal or emit raw NDJSON lines for CI pipelines. |
| **Code Graph Build** | `manvi map build` | Index repository symbols and AST relationships into `.devcouncil/code_graph.json`. |
| **Code Graph Rebuild** | `manvi map rebuild` | Discard existing derived index and regenerate the code graph from scratch. |
| **Code Graph Status** | `manvi map status` | Report code graph freshness, indexed symbols, areas, and rule permissiveness. |
| **Wire Probe** | `manvi probe PROVIDER` | Dispatch a live test request with a real tool call to certify provider wire contracts. |
| **Module Mark** | `manvi logo [--svg]` | Render the 7×7 module mark to the terminal or export pure SVG. |

---

## Headless Exit Codes (`manvi run`)

Scripts and CI systems should inspect exit codes from `manvi run`:

| Exit Code | Meaning | Interpretation |
|---|---|---|
| `0` | **Success** | Turn completed successfully with evidence of completion. |
| `1` | **Failure** | Turn terminated due to an error, policy rejection, or tool failure. |
| `2` | **Step Ceiling Reached** | Turn exceeded `--max-steps` before completion. Work is incomplete; CI should **not** commit partial changes. |

---

## Configuration & Flag Catalogue

Every setting in MANVI can be configured via environment variables (`MANVI_<KEY>`), `.devcouncil/config.yaml`, or runtime flags.

### Mutability Scopes

Every setting has an explicit **mutability scope**:
- **`human`**: May be updated at runtime by a person via `manvi flags set KEY VALUE` or `/flags set KEY VALUE` in the interactive TUI.
- **`startup`**: Frozen once boot completes and the flag registry is sealed. Safety-critical flags (e.g. `policy.hard_rules.enabled`, `log.model_visible_assert`) are `startup`-only so that an agent cannot weaken its own sandbox.

> [!IMPORTANT]
> The model/agent execution plane has **no access** to the flag registry. An agent cannot alter its own gates.

### Full Flag Catalogue

| Flag Key | Environment Variable | Default | Safety Critical | Mutability | Description |
|---|---|---|---|---|---|
| `harness.posture` | `MANVI_HARNESS_POSTURE` | `dev` | **Yes** | `human` | Security posture: `dev` (advisory scope), `strict` (enforce all), `yolo` (all gates off). |
| `harness.init.enabled` | `MANVI_HARNESS_INIT_ENABLED` | `true` | No | `startup` | Auto-create state directory and `.gitignore` managed rules on startup. |
| `policy.file.mode` | `MANVI_POLICY_FILE_MODE` | `enforce` | **Yes** | `human` | File write gate mode (`enforce`, `advisory`, `off`). |
| `policy.command.mode` | `MANVI_POLICY_COMMAND_MODE` | `enforce` | **Yes** | `human` | Shell command gate mode (`enforce`, `advisory`, `off`). |
| `policy.hard_rules.enabled` | `MANVI_POLICY_HARD_RULES_ENABLED` | `true` | **Yes** | `startup` | Hard safety rules (outside-root, secrets, git safety). Disabled only in `yolo`. |
| `policy.scope.allow_neighbors`| `MANVI_POLICY_SCOPE_ALLOW_NEIGHBORS`| `true` | No | `human` | Allow writes to adjacent subsystems per AST code graph. |
| `policy.scope.allow_same_dir` | `MANVI_POLICY_SCOPE_ALLOW_SAME_DIR` | `true` | No | `human` | Fallback: allow writes in the same directory as a planned file (recorded as degraded). |
| `grants.enabled` | `MANVI_GRANTS_ENABLED` | `true` | No | `human` | Master switch for human and agent override seam. |
| `grants.agent.enabled` | `MANVI_GRANTS_AGENT_ENABLED` | `true` | **Yes** | `human` | Permit sub-agents to issue short-lived overrides within scope. |
| `grants.agent.max_ttl` | `MANVI_GRANTS_AGENT_MAX_TTL` | `15m` | **Yes** | `human` | Maximum duration for agent-issued overrides. |
| `grants.human.max_ttl` | `MANVI_GRANTS_HUMAN_MAX_TTL` | `8h` | No | `human` | Maximum duration for human-issued overrides. |
| `grants.require_reason` | `MANVI_GRANTS_REQUIRE_REASON` | `true` | **Yes** | `startup` | Require non-empty rationale for every grant. |
| `grants.agent.allow_commands` | `MANVI_GRANTS_AGENT_ALLOW_COMMANDS`| `false` | **Yes** | `human` | Permit agent overrides on command allowlist blocks. |
| `verify.diff_coverage.enforce`| `MANVI_VERIFY_DIFF_COVERAGE_ENFORCE`| `false` | No | `human` | Treat unexercised diff lines as blocking verification gaps. |
| `verify.rigor.enabled` | `MANVI_VERIFY_RIGOR_ENABLED` | `true` | **Yes** | `human` | Detect stubs, TODOs, and unimplemented markers in added diff lines. |
| `log.model_visible_assert` | `MANVI_LOG_MODEL_VISIBLE_ASSERT` | `true` | **Yes** | `startup` | Assert model-visible context matches append-only session log before LLM calls. |
| `llm.provider.default` | `MANVI_LLM_PROVIDER_DEFAULT` | `anthropic`| No | `human` | Default LLM provider (`anthropic`, `gemini`, `xai`, `local`). |
| `llm.effort` | `MANVI_LLM_EFFORT` | `""` | No | `human` | Reasoning effort level passed to provider (`low`, `medium`, `high`). |
| `llm.local.base_url` | `MANVI_LLM_LOCAL_BASE_URL` | `http://127.0.0.1:8000/v1` | No | `human` | Local OpenAI-compatible server URL. |
| `llm.local.model` | `MANVI_LLM_LOCAL_MODEL` | `""` | No | `human` | Local model ID. If empty, falls back to `MANVI_MODEL`. |
| `llm.local.context_window` | `MANVI_LLM_LOCAL_CONTEXT_WINDOW` | `32768` | No | `human` | Fallback token budget when server publishes no window. |
| `llm.local.max_output_tokens` | `MANVI_LLM_LOCAL_MAX_OUTPUT_TOKENS`| `16384` | No | `human` | Output token ceiling per request to prevent truncation. |
| `llm.local.temperature` | `MANVI_LLM_LOCAL_TEMPERATURE` | `0.1` | No | `human` | Sampling temperature for local model completions. |
| `llm.local.top_p` | `MANVI_LLM_LOCAL_TOP_P` | `0.95` | No | `human` | Nucleus sampling probability threshold. |
| `llm.local.top_k` | `MANVI_LLM_LOCAL_TOP_K` | `0` | No | `human` | Top-K sampling ceiling (0 disables). |
| `llm.local.min_p` | `MANVI_LLM_LOCAL_MIN_P` | `0.05` | No | `human` | Min-P dynamic probability sampling threshold. |
| `llm.local.repetition_penalty`| `MANVI_LLM_LOCAL_REPETITION_PENALTY`| `1.05` | No | `human` | Repetition penalty factor for local generation. |
| `llm.local.presence_penalty` | `MANVI_LLM_LOCAL_PRESENCE_PENALTY` | `0.0` | No | `human` | Presence penalty for local generation. |
| `llm.local.frequency_penalty`| `MANVI_LLM_LOCAL_FREQUENCY_PENALTY`| `0.0` | No | `human` | Frequency penalty for local generation. |
| `llm.local.seed` | `MANVI_LLM_LOCAL_SEED` | `0` | No | `human` | Deterministic sampling seed. |
| `llm.local.stop` | `MANVI_LLM_LOCAL_STOP` | `""` | No | `human` | Comma-separated custom stop tokens. |
| `llm.local.stall_timeout` | `MANVI_LLM_LOCAL_STALL_TIMEOUT` | `15s` | No | `human` | Maximum time between emitted streaming tokens before aborting. |
| `llm.local.supports_tools` | `MANVI_LLM_LOCAL_SUPPORTS_TOOLS` | `true` | No | `human` | Declares whether local model endpoint supports function/tool calling. |
| `llm.local.supports_reasoning`| `MANVI_LLM_LOCAL_SUPPORTS_REASONING`| `false`| No | `human` | Declares whether local server accepts `reasoning_effort`. |
| `llm.local.assume_reasoning_prefill`| `MANVI_LLM_LOCAL_ASSUME_REASONING_PREFILL`| `false`| No | `human` | Reclassify unclosed thinking blocks assuming template prefilled `<think>`. |
| `llm.local.core_tools_only` | `MANVI_LLM_LOCAL_CORE_TOOLS_ONLY` | `false` | No | `human` | Narrow offered tools to core set for smaller local models. |
| `llm.local.trust_declared_context`| `MANVI_LLM_LOCAL_TRUST_DECLARED_CONTEXT`| `false`| No | `human` | Trust configured context window over server-published dimensions. |
| `llm.local.assume_model_served`| `MANVI_LLM_LOCAL_ASSUME_MODEL_SERVED`| `false`| No | `human` | Skip `/v1/models` discovery for servers missing the endpoint. |
| `MANVI_TUI_THEME` | `MANVI_TUI_THEME` | `dark` | No | `human` | Default TUI color theme (`dark`, `light`, `plain`). |

---

## Posture Matrix

```
harness.posture = dev      soft rules report; hard rules block (shipped default)
harness.posture = strict   everything enforces; soft rules block
harness.posture = yolo     every gate off, hard rules and root containment included
```

| Feature / Gate | `strict` | `dev` (Default) | `yolo` |
|---|---|---|---|
| **Root Containment** | Absolute | Absolute | *Off* — writes can land anywhere this process can write |
| **Credential & Git Hard Rules** | **Enforced** | **Enforced** | *Off* (Recorded as degraded) |
| **Task Scope Soft Rules** | **Enforced** | *Advisory* (Recorded as demoted) | *Off* |
| **Operator Escalation (TUI)** | Prompts Modal | No Prompts | No Prompts |
| **Run Classification** | Strict | Weakened | Weakened |

---

## Repository Auto-Preparation Lifecycle

Every invocation of MANVI automatically prepares the repository in which it runs, eliminating manual initialization:

| Step | Behavior |
|---|---|
| **State Directory** | `.devcouncil/` is created, along with the parent directories of any artifact paths (`MANVI_STORE_DB`, `MANVI_GRAPH`). Setting `MANVI_STATE_DIR` relocates all state directories uniformly. |
| **`.gitignore` Integration** | Managed ignore rules are appended if missing—transcribed byte-for-byte from DevCouncil's canonical `ensure_gitignore`, avoiding duplicate entries. |
| **AST Navigation Index** | In `manvi` / `manvi tui`, code graph indexing runs asynchronously in the background so the initial UI frame renders instantaneously without waiting on large repository analysis. |

To disable automatic working-tree modifications, set `MANVI_HARNESS_INIT_ENABLED=false`.
