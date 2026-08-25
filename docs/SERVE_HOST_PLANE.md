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
| `chat.prepare` | Computes token budgets, applies self-calibration, and plans one-way compaction. |
| `chat.settle` | Parses completed model responses: reclassifies `<think>` tags, recovers tool calls, and handles truncations. |
| `chat.forget` | Explicitly drops a conversation's compaction and calibration ledger. |

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
      "chat.prepare",
      "chat.settle",
      "chat.forget"
    ]
  }
}
```

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

### 4. `chat.prepare` (Context & Token Budget Preparation)

**Request:**
```json
{
  "id": "req-4",
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
  "id": "req-4",
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

### 5. `chat.settle` (Completion Ingestion & Recovery)

**Request:**
```json
{
  "id": "req-5",
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
  "id": "req-5",
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
4. Pass model completions through `chat.settle` to transparently recover function calls and sanitize chain-of-thought blocks.
