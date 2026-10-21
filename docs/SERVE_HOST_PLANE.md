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
manvi serve [--posture host|devcouncil] [--workbench-db /absolute/profile.sqlite]
```

---

## Supported Operations

| Operation (`op`) | Description |
|---|---|
| `hello` | Handshake reporting supported operations, server version, and capabilities. |
| `policy.check.file` | Evaluates a proposed file write or deletion against hard/soft policy rules without modifying the filesystem. |
| `policy.check.command` | Evaluates a proposed shell command against the command gate and safety allowlists. |
| `capability.probe` | Live-probes local endpoints to inspect tool calling, token budget, and context limits. A successful answer the server gave is reused by a repeat probe with identical parameters for up to the local adapter's discovery TTL (60 s, the freshness the harness itself accepts); a changed base URL or any other changed parameter probes afresh, and failures and declared-window fallbacks are never reused, so a retry after starting a server or pulling a model asks the server again. |
| `local.scan` | Discovers runnable local LLM servers and returns advertised models plus capability flags (`supports_tools`, `supports_vision`, `supports_reasoning`). |
| `chat.prepare` | Computes token budgets, applies self-calibration, and plans one-way compaction. |
| `chat.settle` | Parses completed model responses: reclassifies `<think>` tags, recovers tool calls, and handles truncations. |
| `chat.forget` | Explicitly drops a conversation's compaction and calibration ledger. |
| `harness.resolve` (optional) | Resolves a harness spec (`repo`, `named` or `inline`) into sections, roles, a bounded skills listing and a digest for the host to compose into its own `System`. Listed in `hello.ops` only when served; a host that does not see it runs with no harness. See [Host harness](#host-harness). |
| `devmap.status` | Reports devmap host-contract, schema, readiness, freshness, coverage gaps, and command capabilities. |
| `devmap.query` | Runs a bounded `explore`, `impact`, `trace`, or `affected` query and preserves the producer's completeness envelope. |
| `work.scan.gaps` / `work.scan.get` / `work.scan.cancel` (with `work.*`) | `work.scan.gaps {repo_root, since?, max_proposals?}` claims a no-model repository gap scan and answers at once with a `scan_id`; `work.scan.get {scan_id}` reads its `coverage`, `complete` flag and `proposals` (each with a `fingerprint`, a `new`/`known`/`attached` status and, for dead code, both signals); `work.scan.cancel` stops it. `repo_root` must be absolute, free of symlinks and the top of a git work tree, else `E_BAD_REQUEST`; an unbuilt or degraded DevMap index is refused with `E_DEPENDENCY` naming `devmap status`. The scan never writes the board: the host files a proposal with its `scan:<fp>` label and the `manvi-scan:` description line, which later scans dedupe on. The claim is in-process (it does not survive a sidecar restart). |
| `work.*` (opt-in) | Persistent profile workspaces, repositories, task CRUD, scoped boards, editable enhancement proposals and acceptance/undo, durable automatic settings/queue/preparation, revisions and events; enabled by `--workbench-db`. The configured CLI host provides asynchronous `work.enhancements.generate`, coalesced `work.enhancements.wake`, cached coordinator status through `work.enhancements.worker`, and selection metadata through `work.enhancements.configuration`. `work.verify.task` claims and answers at once, then verifies a board item on a job context of its own (changed paths from git, the operator's verification command and rigor gates, DevMap-affected tests, and a read-only critic scoring each stored acceptance criterion) and records a `passed`/`failed`/`degraded` verdict as dc-store evidence; `work.verify.get` reads it. A retry with the same `request_id` replays the record and never re-runs the critic; the request carries no command and no path list. Committed text saves wake automatic work; data/configuration reads do not start inference. Managed Codex uses host-only `work.runs.managed.prepare`, `activate` and `stop` operations; preparation returns protocol version 2 without initializing a thread, and activation records native process identity before the handshake and single task turn. See [Workbench](WORKBENCH.md). |

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
      "capability.probe",
      "chat.forget",
      "chat.prepare",
      "chat.settle",
      "devmap.query",
      "devmap.status",
      "hello",
      "local.scan",
      "policy.check.command",
      "policy.check.file"
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
Consumers must read `shown`, `total`, `hidden`, `truncated`, `tokens_used`,
`resolution`, and `walk_incomplete` where present; a capped or incomplete walk
is not complete coverage. Manvi verifies that `shown` equals the number of
returned items, `shown + hidden == total`, and `truncated == (hidden > 0)`.
It also refuses a query unless `host_contract_version` is `1`, both
`reader_ready` and `query_ready` are true, the requested capability is
advertised, and status supplies a non-empty database path and positive
generation. Schema numbers and `schema_relation` remain diagnostics: the
versioned readiness contract decides compatibility, so an additive compatible
schema can remain usable without changing this host.

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

### Host harness

`harness.resolve` is an optional op: a build serves it when its host-plane module is
configured, and says so in `hello.ops`. A host that does not find it there has no
harness to resolve and runs without one; it must not guess the op exists.

```json
{"id":"h1","op":"harness.resolve","params":{"session_id":"sess-abc","provider":"anthropic",
  "root":"/abs/repo","spec":{"kind":"repo","name":"team"}}}
```

`kind` is `repo` (`.devcouncil/harness/<name>.json` under `root`, read through the repository's
gated reader), `named` (a spec the process registered) or `inline` (`spec.inline`, the spec
document itself). `root` is required for `repo`; when given for any kind it is where the
skills listing is read. The result is `{sections, roles, skills, digest, rejected, notes?}`:
the host composes `System` from its own essentials, then `sections` in order. Pass `digest` as
`chat.prepare`'s `harness_digest`; a change between prepares is reported once as
`prefix_moved` and leaves the compaction ledger untouched.

A spec only adds text and narrows roles. It is decoded strictly (exact lowercase keys, no
duplicates, no unknown field) and validated like a repository spec, so a `write_exceptions`,
flag, tier, posture, MCP, verify or fetch field, or a section named like one the prompt owns, is
refused with `E_HARNESS_REFUSED` and every problem is listed. Params this build does not know
are refused for the same reason: a widening a host believes was applied must not be dropped.
`E_HARNESS_NOT_FOUND` says no such spec exists.

Bounds, as enforced: a spec document, inline or read from a repository file, is at most
256 KiB (`harness.MaxSpecBytes`); the request line carrying it is at most 8 MiB (the serve line
cap). Inside a document: at most 32 sections of at most 16 KiB each, 16 roles, and a total of
8192 estimated tokens of section text (a quarter of the prompt budget on a provider that has
one, whichever is less). A document over 256 KiB, or a section over 16 KiB, is refused with
`E_HARNESS_REFUSED`. The skills listing is the repository's own, at most 200 skills examined, cut to
the prompt listing's token share where the provider has a budget; what was cut is said in `notes`.

`root` is not confined to any workspace. Like the `root` and `repo_root` of the devmap and scan
ops, it is whatever absolute path the host names (validated as the top of a git work tree, with
no symlink), and the host is trusted to name only repositories it may read. A repository spec
under it is read through the same read gate as `read_file`.

A managed Claude run needs the Claude Code CLI itself, not a wrapper that starts it as a child
process. `work.runs.managed.prepare` refuses, before activation, a configured program
(`MANVI_CLAUDE_BINARY`, else `claude` on `PATH`; symlinks are followed) that is a shell script
(an account picker, an nvm or asdf shim) or a script under any interpreter other than `node`,
`bun` or `deno`. A native executable and an npm-installed `#!/usr/bin/env node` launcher are
accepted. The error names `MANVI_CLAUDE_BINARY` and the resolved path. A host behind a shell
wrapper sets `MANVI_CLAUDE_BINARY` to the Claude Code executable and carries any wrapper-set
environment (such as `CLAUDE_CONFIG_DIR`) itself. The check reads 256 bytes and never runs the
program; the post-start process-identity check is unchanged and stays fail-closed. See
`docs/WORKBENCH.md`.

Managed Codex (`MANVI_CODEX_BINARY`, else `codex` on `PATH`) has no such refusal, because Codex
reports no process identity for the host to compare. Set `MANVI_CODEX_BINARY` to the native
`codex` executable so the process id the host activates is the one running the model; a
launcher that starts it as a child is accepted but leaves that id describing the launcher.

Managed runs take `harness: {kind: "repo"|"named", name}` on `work.runs.managed.prepare`.
Inline is refused there: the text would go into the file an unattended agent obeys, and has no
reviewed artifact behind it. The whole prepare request is at most 4096 bytes (so a harness
member is a name and nothing more), the spec it names is read under the same 256 KiB bound, and
an instruction file over 4 MiB is refused rather than truncated. The sections are written as
one block at the end of `AGENTS.md` (Codex) or `CLAUDE.md` (Claude), after the claim and before
the process starts, and removed when the run ends. A failed write aborts the run with
`E_HARNESS_MATERIALISE`. `harness_digest` is in the prepare result and in
`effective_configuration`.

How the block is kept safe:

- Its BEGIN and END lines carry a fresh 128-bit random nonce. Only a span byte-identical to
  the bytes Manvi appended (separator included) is ever removed; text that merely looks like a
  block has no nonce Manvi issued and is ordinary content, never matched or removed. Restore
  does not strip newlines: a file with no trailing newline, CRLF or a BOM comes back byte for
  byte, and an agent's own edits before or after the block stay.
- If the agent edited inside the block, restore refuses with `E_HARNESS_MATERIALISE` ("the
  checkout carries a modified harness block"), leaves the file as it is, and keeps a record
  under `.devcouncil/managed-harness-pending.json` (tool state, which diffs already leave out).
  The next prepare on that checkout refuses once with the same typed error and clears the
  record, so it is never wedged by it.
- That record is written before the block. A run that dies with Manvi leaves it, and the next
  `work.runs.managed.prepare` on that checkout, with a harness or not, takes the block out
  first and reports it in `harness_recovery`.
- One live harness per checkout: an attempt with a harness is refused with `E_HARNESS_BUSY`
  while any other managed run is live on the same checkout, and an attempt without one is
  refused while a run with a harness is, because the block would be in its instruction file and
  its diff. Attempts without a harness share a checkout as before.
- `work.verify.task` and `work.scan.gaps` refuse with `E_HARNESS_BUSY` ("a managed harness run
  is in progress") while a block is live in the worktree, or a record of one has not been
  applied: a diff taken then would not be the agent's. They refuse rather than exclude the span.
  A verification that is already running when a harness run starts on the same worktree is not
  stopped.

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
