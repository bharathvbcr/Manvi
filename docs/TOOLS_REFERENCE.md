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

Every tool in every row is specified below, and
`TestToolsReferenceSpecifiesEveryTool` fails the build if one is added without
a row here. `manvi tools` remains the live schema; this document is what the
schema means.

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
| `devcouncil_list_dir` | Read-only | `path` (string), `recursive` (bool, optional), `include_ignored` (bool, optional) | Without `recursive`, reports the literal directory as it is on disk — no ignore rules, because "what is in this directory" is a different question. With `recursive`, enumerates **exactly the files `devcouncil_grep` would search**, honouring ignore rules unless `include_ignored` is set. |
| `devcouncil_find_files` | Read-only | `pattern` (string), `path` (string, optional), `max_results` (int, optional), `include_ignored` (bool, optional) | Glob matching (`*.go`, `src/**/*.rs`) over **exactly the files `devcouncil_grep` would search** — one walk, one set of ignore rules, so the two tools cannot describe different repositories. Globs are matched by the harness's own `fnmatch`, pinned to the 775-case CPython parity fixture. Reports `files_considered` and flags a truncated candidate list separately from a truncated match list. |
| `devcouncil_grep` | Read-only | `pattern` (string), `path` (string, optional), `max_results` (int, optional), `include_ignored` (bool, optional), `case_insensitive` (bool, optional) | Regular-expression search across the workspace, on ripgrep's engine in the analysis plane. Honours `.gitignore`, `.ignore`, `.git/info/exclude` and hidden files unless `include_ignored` is set; never reads `.git` or `.devcouncil`. Reports files searched and files skipped, so a result is never mistaken for full coverage. An unparseable pattern is an error, never an empty match list. |

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

### 9. Tool Discovery & Activation Tools

These two exist for `llm.local.dynamic_tools` (default `true`): a small local
model starts with a lean core working set, and the rest of the surface is
loaded on demand rather than spent on context up front. A tool that is
registered but not active is not silently absent — calling it returns an error
naming the exact `devcouncil_activate_tools` call that would admit it.

| Native Tool | Access | Parameters | Description |
|---|---|---|---|
| `devcouncil_search_tools` | Read-only | `query` (string, optional) | Searches the registry by keyword against names, groups, and descriptions, returning name, group, description, and active state per hit plus a count — without loading full schemas into context. An unattached registry reports *unavailable* rather than an empty result set, so "found nothing" and "could not look" stay distinguishable. |
| `devcouncil_activate_tools` | Read-only | `tools` (array of strings, required) | Activates tools by name or whole groups by name (`task`, `nav`, `subagent`, `artifact`, `mcp`). Prerequisites declared in a tool's `requires` are resolved and activated with it. Returns the activated names and the new active count. An empty array is an error, not a no-op. |

---

### 10. Sub-Agent Management Tools

Category 3 dispatches a fan-out; this category defines the roles it dispatches
and inspects what is running. Depth and width come from `agents.max_spawn_depth`
(default 2, max 8) and `agents.max_fanout` (default 8, max 32).

| Native Tool | Access | Parameters | Description |
|---|---|---|---|
| `devcouncil_define_subagent` | Write | `name` (string), `description` (string), `system_prompt` (string), `role` (string, optional), `model` (string, optional — a model name or `inherit`/`flash`/`pro`), `enable_write_tools` (bool, optional), `enable_mcp_tools` (bool, optional), `allowed_tools` (array of strings, optional) | Registers or updates a runtime role. Refused entirely when `subagents.dynamic.enabled=false`, with the already-registered roles named so dispatch by `type_name` remains possible. A role this harness *ships* can never be redefined at runtime whatever that setting says — otherwise "define a role" and "rewrite the reviewed read-only critic to permit writes" would be the same call, under a name every later dispatch still reads as the reviewed one. `allowed_tools` only ever narrows; it cannot widen what the two booleans permit. |
| `devcouncil_invoke_subagent` | Write | `subagents` (array of objects: `type_name`, `role`, `prompt` required; `model` optional) | Dispatches the named roles concurrently and returns a conversation ID and outcome per child. `agents.max_spawn_depth=0` means this harness delegates nothing at all: nothing is dispatched, and the refusal says so explicitly rather than returning an empty result that reads as zero findings. A `workspace` key is still decoded purely so it can be refused — a silently dropped isolation request is the defect its removal was for. A child that returns without a summary is a failure, never a completion. |
| `devcouncil_manage_subagents` | Write | `action` (`list`\|`kill`\|`kill_all`), `conversation_ids` (array of strings, required for `kill`) | `list` returns a snapshot of each instance's live state — a snapshot rather than the instances themselves, because marshalling those raced with the pool goroutines writing them. `kill` accounts for every ID it was given: any that could not be terminated come back under `not_terminated` with the reason, so a caller cannot mistake "never existed" for "stopped". `kill_all` over a manager holding nothing is an error, not a claim to have terminated children that do not exist. |
| `devcouncil_send_message` | Write | `recipient` (string), `message` (string) | **Refuses every call, by design.** A sub-agent here runs as one prompt in and one result out; there is no seam through which an instruction arriving mid-run could be delivered. This tool previously answered `{"delivered": true}` into a channel nothing reads. The refusal states whether the recipient is a registered sub-agent, and directs the caller to put the instruction in the dispatch prompt or to wait for the result and dispatch a follow-up. |

---

### 11. Artifact Tools

Artifacts live under `.devcouncil/artifacts/`, which `devcouncil_write_file`
refuses as a hard rule no override clears. These tools are not an exception to
that rule — the general-purpose write tool pointed into `.devcouncil/` reaches
the task store, the config, and the grant ledger, and that is what the rule
protects. What these three have instead is *confinement*: a store-sanitised
name under one subtree that holds nothing security-bearing.

They still answer the two questions the write gate would ask. A write with no
task checked out is refused under `policy.RuleNoTask` — an artifact recording
work nothing can attribute — unless the file gate mode is `advisory` or `off`,
in which case the demotion is named in the result. Every allow carries
`scope.artifact_store` in its degraded list, so an allow reached through the
store's confinement is never indistinguishable from one the task plan
authorised.

| Native Tool | Access | Parameters | Description |
|---|---|---|---|
| `devcouncil_create_artifact` | Write | `name` (string), `content` (string), `metadata` (object: `summary` required; `user_facing`, `request_feedback` optional booleans) | Creates a persistent structured artifact — implementation plan, walkthrough, research notes, design document — under `.devcouncil/artifacts/`. |
| `devcouncil_update_artifact` | Write | `name` (string), `content` (string), `metadata` (object, optional) | Replaces an existing artifact's content and metadata, incrementing its revision. |
| `devcouncil_list_artifacts` | Read-only | *None* | Lists every artifact currently recorded in `.devcouncil/artifacts/`. |

---

### 12. Interactive Question Tools

| Native Tool | Access | Parameters | Description |
|---|---|---|---|
| `devcouncil_ask_question` | Write | `questions` (array of objects: `question` (string) and `options` (array of strings) required, `is_multi_select` (bool) optional) | Puts one or more questions to the operator to resolve underspecified requirements or choose between designs. An empty array is refused, as is any question with empty text or fewer than two options. The result says **who answered**: `answered:true` with `answered_by:"human"` carries real choices under `answers`; `answered:false` with `answered_by:"none"` means no human was available, no question was put to anyone, and the values under `assumed_defaults` are assumptions the run proceeded on — never to be reported back as a decision the user made. |

---

### 13. MCP 2.0 & Open Plugin Tools

An MCP server is a separate program whose replies are entirely its own choice,
so both bounds and the gate apply on this path.

`mcp_call_tool` is arbitrated by the **command** gate, not the write gate,
because that is what it is: an instruction to another program to act outside
this harness, with effects the harness cannot observe. A server advertising
`run_shell` or `write_file` would otherwise have been a complete route around
the command gate, the write gate, and the approval prompt at once. The target
is rendered as `mcp_call_tool <server>/<tool>`, which is stable and
allowlistable — an operator who wants one server's tools to run unprompted adds
`mcp_call_tool weather/*` to the task or global allowed commands, exactly as for
any other command. Server and tool names that could not be rendered into that
target unambiguously are rejected before the call is made.

One tool call may return at most **256 KiB** across at most **512 content
parts**. When no MCP manager was supplied at construction, calls are refused by
a disabled manager that names *that* as the cause — deliberately distinct from
the refusal `mcp.enabled=false` produces, because the remedies differ.

| Native Tool | Access | Parameters | Description |
|---|---|---|---|
| `mcp_list_tools` | Read-only | `server_name` (string, optional) | Lists tools from configured MCP 2.0 servers and Open Plugins. With a server named, returns that server's tools; without one, returns every server's. |
| `mcp_call_tool` | Write | `server_name` (string), `tool_name` (string), `arguments` (object) | Calls a tool on an external server over stateless JSON-RPC, through the command gate and the approver, with the result bounded as above. |
| `mcp_list_resources` | Read-only | `server_name` (string) | Lists the resources a target server exposes. |
| `mcp_read_resource` | Read-only | `server_name` (string), `uri` (string) | Reads one resource's contents from a target server by URI. |

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
