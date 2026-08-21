# Running Against Local LLMs (Ollama, MLX, vLLM, llama.cpp, LM Studio)

MANVI features a dedicated, highly optimized `local` provider designed specifically for local LLMs (e.g. Qwen 2.5 / 3.8 / 27B / 32B, DeepSeek-R1, LLaMA 3.x, Mistral, Gemma) running on Apple Silicon (MLX), vLLM, Ollama, LM Studio, Jan, or llama.cpp.

---

## Quickstart

```bash
# 1. Discover local LLM servers running on your machine
manvi local

# 2. Tell MANVI to default to the local provider
export MANVI_LLM_PROVIDER_DEFAULT=local

# 3. Inspect the resolved endpoint and model candidate
manvi local --resolve

# 4. Probe the live local server with a real tool call to certify the wire contract
manvi probe local

# 5. Execute an agent turn
manvi run -p "remove unused imports from helper.go"
```

---

## Configuration & Discovery

The server and the model are both discovered automatically when they are unambiguous. You can name them yourself when they are not, or to override discovery:

```bash
# Force a specific local endpoint (stops loopback port scan)
export MANVI_LLM_LOCAL_BASE_URL=http://127.0.0.1:11434/v1

# Target a specific model served by the endpoint
export MANVI_MODEL=mlx-community/Qwen3.8-27B-4bit
```

### Sampling Defaults

Sampling defaults follow what open-weight models ship beside their weights. For example, Qwen3's `generation_config.json` declares temperature 1.0, top_k 20, top_p 0.95:

```bash
export MANVI_LLM_LOCAL_TEMPERATURE=0.7                 # Near-deterministic without being greedy
export MANVI_LLM_LOCAL_TOP_K=20                        # Model's own recommendation
export MANVI_LLM_LOCAL_TOP_P=0.95                      # Nucleus sampling threshold
export MANVI_LLM_LOCAL_SEED=1234                       # Makes failing turns investigable
export MANVI_LLM_LOCAL_STOP="<|im_end|>,<|endoftext|>" # Custom stop tokens
```

### Durable Settings (`.devcouncil/config.yaml`)

Settings that outlive a shell go into `.devcouncil/config.yaml`, a flat mapping of dotted names to scalars (the one file under `.devcouncil/` intended to be committed to version control):

```yaml
llm.local.model: qwen3.8:27b-mlx
llm.local.temperature: 0.7
llm.local.core_tools_only: false
```

Resolution precedence is lowest-first:
1. Catalogue built-in defaults
2. `.devcouncil/config.yaml`
3. Environment variables (`MANVI_<FLAG>`)
4. Process runtime overrides (`/flags set` or `manvi flags set`)

`manvi flags` displays every setting and its origin layer. `manvi doctor` verifies that the config file was read correctly and reports active values.

---

## Certifying Wire Contracts (`manvi probe local`)

```bash
manvi probe local
```

`manvi probe local` offers the local model a tool and requires a tool call response, because tool calling is the path that actually breaks agent harnesses. It reports:
- Whether the **server** parsed the call natively into OpenAI tool call format, OR
- Whether the **harness** had to recover it from raw message text (e.g. Qwen XML, Hermes JSON).

> [!NOTE]
> Recovery works reliably, but indicates that the server lacks a native tool parser for the model it serves, which may incur slight latency or token overhead.

---

## Key Local LLM Optimizations in MANVI

### 1. Zero-Config Loopback Endpoint Discovery (~30ms)
`llm.local.base_url` defaults to vLLM's standard port (8000), but if untouched, MANVI automatically scans loopback ports commonly used by local servers:
- Ollama: `11434`
- vLLM: `8000`
- LM Studio: `1234`
- llama.cpp & `mlx_lm.server`: `8080`
- Jan: `1337`

Unused loopback ports refuse connections instantly, completing the entire scan in **~30ms**. The runtime is identified by querying native capability endpoints (`/api/version` on Ollama, `/props` on llama.cpp, `max_model_len` on vLLM model cards), never by guessing based on port numbers.

### 2. Zero-Config Model Discovery
If a local server hosts exactly one model capable of driving coding turns, no `MANVI_MODEL` setting is required. Non-coding and embedding models are automatically filtered out (e.g. via Ollama capability lists).

### 3. Discovered Context Dimensions with Declared Fallback
Context windows are dynamically retrieved from server metadata:
- Ollama: `/api/show` returns `<arch>.context_length` and capabilities
- vLLM: `max_model_len` from model card
- llama.cpp: `/props` returns active `n_ctx`
If the server exposes no dimensions, MANVI falls back to `llm.local.context_window` (default `32768`).

### 4. Prefix-Cache-Preserving Compaction (1.5s vs 120s)
Local server inference throughput relies heavily on prompt-prefix KV cache reuse. Rewriting history on every turn invalidates the KV cache, triggering a full prompt re-prefill (**120s for a 14.7k-token prompt on a 4-bit 27B model**). 

MANVI records one-way compactions in `session.jsonl` and projects them deterministically so that the token prefix **only ever grows**, reducing step latency to **1.5s warm**.

### 5. Self-Calibrating Token Budget
Token estimators can diverge from model tokenizers (e.g. ~25% high on Qwen, up to 58% on structured JSON tool outputs). MANVI reads the exact `prompt_tokens` returned by the server on each response and self-calibrates its estimation over successive steps. Tool schemas are explicitly budgeted (~1,755 tokens on the shipped surface).

### 6. Server Telemetry Ingestion
MANVI parses `prompt_tokens_details.cached_tokens` and server `timings` blocks from SSE streams, tracking prefix cache hit ratios and generation tokens/second directly on `llm.Usage`.

### 7. Wire-Level Parser Recovery
When local inference engines lack built-in tool call parsers, MANVI automatically parses:
- Hermes JSON format
- Qwen3 nested XML: `<function=name><parameter=key>value</parameter></function>`
- Markdown code fenced JSON

Argument types are validated and coerced using the tool's declared schema, preventing malformed types (e.g. octal string `"0755"` becoming integer `755`).

### 8. Prefilled Thinking Tag Sanitization
Models like Qwen 2.5 / 3.x prefill `<think>` tags in prompt templates, emitting only the closing `</think>` tag. MANVI detects unmatched closing tags, reclassifies reasoning content, and prevents raw CoT text from polluting subsequent conversation history.

### 9. Recoverable Output Truncation
Local servers frequently enforce output token caps (e.g. MLX defaults to 2048). If a tool call is truncated mid-arguments, MANVI catches the cut-off, formats a retryable hint for the model, and prompts for continuation without crashing the turn. Both `max_completion_tokens` and `max_tokens` are transmitted to ensure compatibility with llama.cpp.

### 10. Streaming Stall Detection
The delay between streamed tokens is bounded separately from overall turn timeout (`llm.local.stall_timeout`, default `15s`). If a local server emits one token and freezes due to GPU lockups, the turn fails fast with actionable diagnostics rather than hanging indefinitely.

### 11. Model-Aware Prompting
Local open-weight models receive explicit environment descriptions, tool contracts, and stopping conditions that larger frontier models infer automatically. The `llm.local.core_tools_only` flag allows restricting the offered tools to a minimal subset for smaller parameter models.

### 12. Bounded Concurrency & Loopback Transport
Sub-agent concurrency is clamped to 2 for `local` providers to prevent GPU/Unified Memory starvation. Loopback connections bypass proxy lookups and decompression overhead.

### 13. Fail-Closed Actionable Diagnostics
Connection refusals, missing models, and context overflows produce specific error messages with remediation commands rather than generic network timeouts.
