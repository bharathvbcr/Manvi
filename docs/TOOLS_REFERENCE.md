# DevCouncil Native Tool Suite Reference

MANVI implements the DevCouncil development tool suite natively in Go and Rust — **44 tools**, as reported by `manvi tools`: 40 `devcouncil_*` tools plus 4 `mcp_*` tools. That is the parity core, a native git integration, one bridge to the external DevCouncil CLI, and the dynamically activated surface (search/activation, MCP, artifacts, questions, sub-agent management). Unlike traditional harnesses that shell out to Python scripts or external interpreters, native execution provides sub-millisecond dispatch, eliminates shell injection vectors, and enforces strict memory-safe parameter validation.

---

## Tool Category Summary

| Category | Count | Primary Purpose |
|---|---|---|
| **Task Lifecycle** | 5 | Discover tasks, manage exclusive SQLite leases, and track progress. |
| **Guarded Mutation & Workspace** | 9 | Read/write files, apply exact-match patches, run audited commands, and query directories. |
| **Multi-Agent Coordination** | 1 | Dispatch and coordinate concurrent sub-agent execution pools. |
| **Override Seam** | 1 | Request audited Human or Agent overrides for soft policy denials. |
| **Verification & Evidence** | 4 | Inspect git diffs, run verification rigor gates, and obtain typed repair actions. |
| **Code Graph & Navigation** | 3 | Query AST symbol definitions, detect dead code, and analyze blast radii. |
| **Git Integration** | 6 | Structured version-control reads (status, log, branches, show) plus gate-arbitrated staging and committing. |
| **External CLI Bridge** | 1 | Read-only queries against the incumbent `dev`/`devcouncil` CLI's project-level views. |
| **Tool Discovery & Activation** | 2 | Search the registry by capability and pull tools or whole groups into the model's active context. |
| **Sub-Agent Management** | 4 | Define, invoke, message, and terminate specialized sub-agents by conversation ID. |
| **Artifacts** | 3 | Create, list, and revise persistent structured artifacts under `.devcouncil/artifacts/`. |
| **Interactive Questions** | 1 | Ask the operator to resolve underspecified requirements or pick between options. |
| **MCP 2.0 & Open Plugins** | 4 | List and call tools, and list and read resources, on external MCP servers over stateless JSON-RPC. |

One further tool, `devcouncil_fetch_url`, is **conditional and not counted above**:
it is registered only when an operator sets `MANVI_FETCH_HOSTS`, so an
unconfigured harness offers the counted set and a configured one offers exactly
one more. See [Documentation lookup](#documentation-lookup).

The thirteen rows above sum to the 44 tools `manvi tools` reports.

**Specification coverage is not yet complete.** The first eight categories (30 tools) have full parameter and permission specifications in this document. The last five (14 tools — `devcouncil_search_tools`, `devcouncil_activate_tools`, `devcouncil_define_subagent`, `devcouncil_invoke_subagent`, `devcouncil_manage_subagents`, `devcouncil_send_message`, `devcouncil_create_artifact`, `devcouncil_list_artifacts`, `devcouncil_update_artifact`, `devcouncil_ask_question`, `mcp_list_tools`, `mcp_call_tool`, `mcp_list_resources`, `mcp_read_resource`) are registered in `manvi/devcouncil/` and dispatch normally, but are not written up below. Run `manvi tools` for their live schemas until they are.

---

## Detailed Tool Specifications

### 1. Task Lifecycle Tools

| Native Tool | Access | Parameters | Description |
|---|---|---|---|
| `devcouncil_next_task` | Read-only | *None* | Discovers the next ready, unallocated task in the project task queue. |
| `devcouncil_get_task` | Read-only | `task_id` (string) | Inspects declared task specification, planned files, forbidden paths, and requirements. |
| `devcouncil_checkout_task` | Write | `task_id` (string), `owner` (string), `ttl` (string, optional) | Claims an exclusive ACID lease on a task in SQLite. Required before modifying files in strict mode. |
| `devcouncil_renew_lease` | Write | `task_id` (string), `token` (string), `ttl` (string, optional) | Extends an active lease TTL prior to expiration. |
| `devcouncil_release_task` | Write | `task_id` (string), `token` (string) | Relinquishes held lease upon task completion or abandonment. |

---

### 2. Guarded Mutation & Workspace Tools

| Native Tool | Access | Parameters | Description |
|---|---|---|---|
| `devcouncil_policy_check_write` | Read-only | `path` (string), `task_id` (string, optional) | Probes the write gate policy without touching the filesystem. Returns decision and rule provenance. |
| `devcouncil_read_file` | Read-only | `path` (string), `offset` (int, opt), `limit` (int, opt) | Reads file contents safely from within the workspace root. |
| `devcouncil_write_file` | Write | `path` (string), `content` (string), `task_id` (string, optional) | Performs an atomic write passing the 5-tier policy gate. Requires active lease under strict posture. |
| `devcouncil_patch_file` | Write | `path` (string), `target` (string), `replacement` (string), `start_line` (int), `end_line` (int) | Performs targeted exact-block substring replacement bounded by line numbers. Fails closed on ambiguity. |
| `devcouncil_delete_file` | Write | `path` (string), `task_id` (string, optional) | Safely deletes a file after passing policy gate and lease checks. |
| `devcouncil_exec_command` | Write | `command` (string), `task_id` (string, optional), `timeout` (string, opt) | Executes shell commands guarded by the command gate and safety allowlists. |
| `devcouncil_list_dir` | Read-only | `path` (string), `recursive` (bool, optional) | Lists directory contents, file types, and sizes. |
| `devcouncil_find_files` | Read-only | `pattern` (string), `path` (string, optional) | Fast glob pattern matching (`*.go`, `src/**/*.rs`) for workspace discovery. |
| `devcouncil_grep` | Read-only | `query` (string), `path` (string, optional), `case_sensitive` (bool) | Regex and literal pattern searching across files in workspace. |

---

### 3. Multi-Agent Coordination Tools

| Native Tool | Access | Parameters | Description |
|---|---|---|---|
| `devcouncil_spawn_subagents` | Write | `subagents` (array of agent configs) | Concurrently dispatches sub-agents in a bounded worker pool. Manages child leases and cleans up held locks on cancellation. |

---

### 4. Override Seam Tools

| Native Tool | Access | Parameters | Description |
|---|---|---|---|
| `devcouncil_request_override` | Write | `rule` (string), `target` (string), `reason` (string), `ttl` (string, optional) | Requests a scoped, audited Human or Agent grant for soft policy blocks (e.g. `scope.unplanned`). |

---

### 5. Verification & Evidence Tools

| Native Tool | Access | Parameters | Description |
|---|---|---|---|
| `devcouncil_get_diff` | Read-only | `task_id` (string, optional) | Inspects the current working-tree git unified diff for the active task. |
| `devcouncil_verify_task` | Read-only | `task_id` (string) | Runs the Rust `dc-verify` engine: checks scope conformance, anti-stub rigor gates, and diff coverage. |
| `devcouncil_get_gaps` | Read-only | `task_id` (string) | Enumerates outstanding verification gaps blocking task acceptance. |
| `devcouncil_get_next_actions` | Read-only | `task_id` (string) | Returns machine-routable typed repair actions for identified verification gaps. |

---

### 6. Code Graph & Navigation Tools

| Native Tool | Access | Parameters | Description |
|---|---|---|---|
| `devcouncil_graph_query` | Read-only | `symbol` (string), `kind` (string, optional) | Searches AST symbol definitions and references with file paths and line spans. |
| `devcouncil_code_dead` | Read-only | `path` (string, optional) | Identifies callerless dead code functions and structs with exemption annotations. |
| `devcouncil_graph_context` | Read-only | `path` (string) | Performs blast-radius analysis: lists incoming callers and outgoing dependencies. |

---

### 7. Git Integration Tools

| Native Tool | Access | Parameters | Description |
|---|---|---|---|
| `devcouncil_git_status` | Read-only | *None* | Reports branch, HEAD, ahead/behind, and individually enumerated staged/unstaged/untracked files (`-uall`, NUL-safe parsing for awkward filenames). |
| `devcouncil_git_log` | Read-only | `max` (int, optional; default 10, capped 50) | Lists recent commits with hash, author, ISO date, and subject. |
| `devcouncil_git_branches` | Read-only | *None* | Lists local branches with the current one marked. |
| `devcouncil_git_show` | Read-only | `object` (string, optional; default HEAD) | Shows one commit's metadata and patch; the patch is size-capped with the truncation reported. Object arguments are validated against option injection. |
| `devcouncil_git_stage` | Write | `paths` (array of strings) | Stages named paths through the command policy gate — the same ladder as `devcouncil_exec_command`. Secret-path matches are refused as hard denials under every posture. |
| `devcouncil_git_commit` | Write | `message` (string), `allow_empty` (bool, optional) | Commits the staged set through the command policy gate; re-checks the index against secret-path patterns at commit time and returns the new HEAD on success. |

---

### 8. External CLI Bridge Tools

| Native Tool | Access | Parameters | Description |
|---|---|---|---|
| `devcouncil_dev_inspect` | Read-only | `section` (`status`\|`gaps`\|`check`), `task_id` (string, optional) | Queries the incumbent `dev`/`devcouncil` CLI over JSON (`MANVI_DEVCOUNCIL_BINARY` overrides discovery). `check` always runs the deterministic evidence gate (`--verify`), never the LLM audit; non-JSON output is returned labelled as degraded, never parsed as structure. |

---

## Direct CLI Tool Invocation

You can invoke any native tool directly from the CLI via `manvi tool`:

```bash
# Read a file
manvi tool devcouncil_read_file --json '{"path": "cmd/manvi/main.go"}'

# Find Go files
manvi tool devcouncil_find_files --json '{"pattern": "**/*.go"}'

# Verify active task
manvi tool devcouncil_verify_task --json '{"task_id": "TASK-001"}'
```

---

## Tool Execution Lifecycle & Qualification

Every tool execution follows a 4-phase pipeline:
1. **`tools/pre-execute`**: Schema validation, context deadline check, and argument normalization.
2. **Policy Gate Evaluation**: Evaluates Write Gate or Command Gate rules against active task scope and grants.
3. **Native Tool Body Execution**: Dispatches directly in Go or calls Rust analysis binaries over stdio IPC.
4. **`tools/post-execute` Qualification**: Attaches outcome metadata (`Passed`, `Blocked`, `Granted`, `Demoted`, `Degraded`) and appends result to the append-only session log.

## Documentation lookup

`devcouncil_fetch_url` is this harness's only outbound network path. It is
**off by default** and does not appear on the tool surface at all until an
operator sets a host allowlist:

```bash
MANVI_FETCH_HOSTS="go.dev,pkg.go.dev,docs.python.org"
```

The allowlist is read from the environment and from nowhere else. It is
deliberately not a settings key: settings load from `.devcouncil/config.yaml`,
the restricted-path rung protecting that file lives inside the hard-rules block,
and a relaxed posture switches that block off — so an allowlist expressed as a
setting would be one the agent could extend by writing a file.

Entries match a host and its subdomains on whole labels, so `go.dev` admits
`pkg.go.dev` and does not admit `notgo.dev`.

**What it refuses, on every request and on every redirect hop:**

| Rule | Reason |
|---|---|
| Anything but `https` | A network position that can rewrite a plaintext response can write the model's instructions. |
| Hosts outside the allowlist | The operator decides what this harness may reach. |
| Loopback, private, link-local, unique-local, multicast, and reserved ranges | `169.254.169.254` is cloud instance metadata; `127.0.0.1` is whatever else runs on the box. |
| Ports other than the default | The allowlist names hosts, not endpoints. |
| Credentials in the URL | This path reads public documentation and never needs one. |
| Non-text content types | A binary body reduced to "text" costs context and says nothing. |

Addresses are re-resolved and re-checked inside the dialer, and the connection
is made to the vetted literal address rather than to the name — resolving once,
checking, and then handing the name to the transport is the hole DNS rebinding
aims at. Proxies from the environment are ignored for the same reason: a proxy
would send the request somewhere nothing vetted.

Every response is bounded (20s, 2 MiB, 5 redirects) and returned wrapped in
`BEGIN/END UNTRUSTED WEB CONTENT` markers that tell the model the enclosed text
is evidence and not instruction. That framing is steering, not a boundary — the
boundary is the policy gate that every subsequent tool call still passes.
