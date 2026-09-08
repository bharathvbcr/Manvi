# Embedded Stdio Host Plane (`manvi serve`)

`manvi serve` exposes MANVI's policy enforcement engine, local-LLM preparation pipeline, and capability discovery to external host processes (editors, IDEs, desktop applications, VS Code/JetBrains extensions) over standard input/output (`stdin`/`stdout`).

---

## Wire Protocol & Transport

- **Transport**: Line-delimited JSON (NDJSON) over standard input (`stdin`) and standard output (`stdout`).
- **Request Format**: One JSON object per line on `stdin`, containing a caller-assigned `"id"`, an `"op"` (operation name), and optional `"params"`.
- **Response Format**: One JSON object per line on `stdout`, containing matching `"id"`, `"ok"` (boolean), optional `"result"`, and optional `"error"`.
- **Diagnostics**: Warnings, logs, and trace telemetry are emitted exclusively to standard error (`stderr`), never corrupting the `stdout` NDJSON stream.
- **Zero Cgo / Cross-Language Boundary**: Non-Go host applications (TypeScript/Node, Rust, Python, Kotlin, C#) integrate directly without C shared libraries (`.so`/`.dylib`/`.dll`) or FFI overhead.
- **Graceful Shutdown**: The server handles `SIGINT`, `SIGTERM`, and clean `EOF` on `stdin` without writing truncated lines.
- **Line Cap**: A request line past 8 MiB is refused with `E_TOO_LARGE` — correlated by id where one can be recovered from the line's head — and the session **continues**; other in-flight calls are unaffected.

```bash
# Launch the host plane server
manvi serve [--posture host|devcouncil]
```

---

## Supported Operations

| Operation (`op`) | Description |
|---|---|
| `hello` | Handshake reporting supported operations, server version, and capabilities. |
| `policy.check.file` | Evaluates a proposed file write or deletion against hard/soft policy rules without modifying the filesystem. |
| `policy.check.command` | Evaluates a proposed shell command against the command gate and safety allowlists. |
| `capability.probe` | Live-probes local endpoints to inspect tool calling, token budget, and context limits. |
| `local.scan` | Discovers runnable local LLM servers and returns advertised models plus capability flags (`supports_tools`, `supports_vision`, `supports_reasoning`). |
| `chat.prepare` | Computes token budgets, applies self-calibration, and plans one-way compaction. |
| `chat.settle` | Parses completed model responses: reclassifies `<think>` tags, recovers tool calls, and handles truncations. |
| `chat.forget` | Explicitly drops a conversation's compaction and calibration ledger. |
| `devmap.status` | Reports devmap host-contract, schema, readiness, freshness, coverage gaps, and command capabilities. |
| `devmap.query` | Runs a bounded `explore`, `impact`, `trace`, or `affected` query and preserves the producer's completeness envelope. |

---

## Operation Schemas & Examples

### 1. `hello` (Handshake)

**Request:**
```json
{"id": "req-1", "op": "hello"}
```

**Response:**
```json
{
  "id": "req-1",
  "ok": true,
  "result": {
    "protocol": 1,
    "posture": "host",
    "ops": [
      "hello",
      "policy.check.file",
      "policy.check.command",
      "capability.probe",
      "local.scan",
      "chat.prepare",
      "chat.settle",
      "chat.forget",
      "devmap.query",
      "devmap.status"
    ]
  }
}
```

### Deep devmap queries

The stock `manvi serve` command installs the devmap module with the repository
root discovered by Manvi and the binary selected by `MANVI_MAP_BINARY` (or
`devmap` on `PATH`). The process boundary enforces its own timeout and output
cap because devmap has no timeout flag. Kernel `budget`, `depth`, target-count,
confidence, and rung bounds are validated before the process starts.

```json
{"id":"map-1","op":"devmap.status","params":{}}
{"id":"map-2","op":"devmap.query","params":{"kind":"impact","query":"Router","depth":3,"budget":2000,"min_rung":"high"}}
```

`devmap.query` returns `data`, the producer's JSON object unchanged, and
`index`, the status observed immediately after it. Manvi also reads status
before the query and refuses the result if the database path or generation
changed between observations. This detects ordinary concurrent rebuilds; it
does not claim a database transaction spans the three subprocess calls.
Consumers must read
`shown`, `total`, `hidden`, `truncated`, `resolution`, and `walk_incomplete`
where present; a capped or incomplete walk is not complete coverage. Manvi
refuses a query unless `host_contract_version` is `1`, the stored and expected
schemas match, `schema_relation` is `current`, and both `reader_ready` and
`query_ready` are true.

| `kind` | Required fields | Optional bounded fields |
|---|---|---|
| `explore` | `query`, `depth` | `budget`, `min_confidence` |
| `impact` | `query`, `depth` | `budget`, `min_rung` |
| `trace` | `query` (from), `to`, `depth` | `budget`, `min_rung` |
| `affected` | `targets` (1–128), `depth` | `budget`, `min_confidence` |

### Embedding and customization

Go hosts construct `serve.Server` directly and add one configuration line per
module. A module registers handlers through `Configure(*serve.Router)`.
Registration is per server, collision checked, and frozen before any protocol
input is read. `Register` adds a new operation; `Replace` must name an existing
one, which prevents a misspelling from creating a parallel path. The `hello`
operation is reserved and cannot be replaced.

```go
srv := serve.New(stdout, serve.Options{
    HardRules: true,
    Modules: []serve.Module{
        serve.DevmapModule{Client: devmap.New(binary, repositoryRoot)},
        hostModule,
    },
})
err := srv.Serve(ctx, stdin)
```

Replacing `policy.check.file` or `policy.check.command` changes the host's
security boundary. Manvi never replaces them by default; an embedding host
that does so owns the replacement's authorization behavior. `HardRules` must
also be set explicitly when constructing `serve.Server` directly. The CLI
derives it from Manvi's effective policy configuration and announces a
weakened setting on stderr.

---

### 2. `policy.check.file` (File Write Gate)

**Request:**
```json
{
  "id": "req-2",
  "op": "policy.check.file",
  "params": {
    "path": "src/helper.go",
    "task_id": "TASK-001"
  }
}
```

**Response:**
```json
{
  "id": "req-2",
  "ok": true,
  "result": {
    "decision": "allow",
    "outcome": "Passed",
    "rule": "",
    "reason": ""
  }
}
```

---

### 3. `policy.check.command` (Command Gate)

**Request:**
```json
{
  "id": "req-3",
  "op": "policy.check.command",
  "params": {
    "command": "rm -rf /",
    "task_id": "TASK-001"
  }
}
```

**Response:**
```json
{
  "id": "req-3",
  "ok": true,
  "result": {
    "decision": "block",
    "outcome": "Blocked",
    "rule": "command.destructive",
    "reason": "command violates destructive command safety filter"
  }
}
```

---

### 4. `local.scan` (Local LLM Server Discovery)

**Request:**
```json
{
  "id": "req-4",
  "op": "local.scan",
  "params": {
    "timeout_ms": 2000,
    "capabilities": true
  }
}
```

**Response:**
```json
{
  "id": "req-4",
  "ok": true,
  "result": {
    "servers": [
      {
        "base_url": "http://127.0.0.1:11434",
        "runtime": "ollama",
        "version": "0.7.2",
        "models": [
          {
            "id": "qwen3:30b",
            "context_window": 32768,
            "context_window_source": "server",
            "capabilities_known": true,
            "supports_tools": false,
            "supports_reasoning": true,
            "supports_vision": false,
            "supports_completion": true
          }
        ]
      }
    ],
    "scanned": 12,
    "capabilities": true
  }
}
```

---

### 5. `chat.prepare` (Context & Token Budget Preparation)

**Request:**
```json
{
  "id": "req-5",
  "op": "chat.prepare",
  "params": {
    "session_id": "sess-abc",
    "system": "You are a coding agent.",
    "tools": [{"name": "Read", "input_schema": {"type": "object"}}],
    "messages": [
      {"role": "user", "text": "fix the failing test in math.go"},
      {"role": "assistant", "tool_calls": [{"id": "c1", "name": "Grep", "arguments": "{\"pattern\":\"x\"}"}]},
      {"role": "tool", "tool_call_id": "c1", "text": "...grep output..."}
    ],
    "context_window": 32768,
    "observed_prompt_tokens": 150
  }
}
```

**Response:**
```json
{
  "id": "req-5",
  "ok": true,
  "result": {
    "steps": [
      {"tool_call_id": "c1", "text": "[compacted]", "from_bytes": 8100, "to_bytes": 11}
    ],
    "before_tokens": 2100,
    "after_tokens": 640,
    "threshold_tokens": 24576,
    "target_tokens": 16384,
    "insufficient": false,
    "calibration_ratio": 0.78,
    "calibration_samples": 3
  }
}
```

---

### 6. `chat.settle` (Completion Ingestion & Recovery)

**Request:**
```json
{
  "id": "req-6",
  "op": "chat.settle",
  "params": {
    "content": "<think>Inspect math.go first</think><tool_call>{\"name\":\"Read\",\"arguments\":{\"file_path\":\"math.go\"}}</tool_call>",
    "tools": [{"name": "Read", "input_schema": {"type": "object"}}],
    "server_parsed_calls": false,
    "finish_reason": "stop"
  }
}
```

**Response:**
```json
{
  "id": "req-6",
  "ok": true,
  "result": {
    "text": "",
    "reasoning": "Inspect math.go first",
    "calls": [
      {"name": "Read", "arguments": "{\"file_path\":\"math.go\"}"}
    ],
    "format": "hermes-json",
    "truncated": false
  }
}
```

---

### 7. `chat.forget` (Session Ledger Reset)

**Request:**
```json
{
  "id": "req-7",
  "op": "chat.forget",
  "params": {
    "session_id": "sess-abc"
  }
}
```

**Response:**
```json
{
  "id": "req-7",
  "ok": true,
  "result": {
    "forgotten": true
  }
}
```

---

## Postures in `manvi serve`

The `--posture` flag controls how the host server treats operations outside an active task scope:

- **`host` (Default)**: 
  Hard safety rules (secret paths, `.git` protection, root containment) are strictly enforced. Soft denials that only indicate *"no task authorises this"* are demoted to explicit `allow [demoted]` records with rule provenance preserved. Ideal for general-purpose editor extensions and desktop pair programmers.
- **`devcouncil`**: 
  Requires an active task lease for modifications; absence of an authorizing task produces a strict `deny`. Ideal for automated benchmark runners and strict CI multi-agent tasks.

---

## Embedding in External Applications

To embed MANVI in a VS Code extension, JetBrains plugin, or Python script:
1. Spawn `manvi serve` as a subprocess with piped `stdin`, `stdout`, and `stderr`.
2. Send a `hello` handshake to verify connectivity and negotiate capabilities.
3. Stream file writes and command attempts through `policy.check.file` and `policy.check.command` before performing disk IO.
4. Optionally call `local.scan` to discover runnable local servers before probing capabilities.
5. Pass model completions through `chat.settle` to transparently recover function calls and sanitize chain-of-thought blocks.
