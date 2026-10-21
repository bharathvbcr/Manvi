# CLI & Configuration Reference

This document provides a complete reference for MANVI's command-line interface, configuration catalog, mutability boundaries, posture matrix, and repository auto-initialization rules.

---

## CLI Command Reference

| Command | Syntax / Options | Description |
|---|---|---|
| **Interactive TUI** | `manvi` or `manvi tui` | Launch the modern full-screen Elm-loop TUI (tab strip, live theme switcher, session switcher, rich markdown & diff rendering). |
| **Build Identity** | `manvi --version [--json]` (or `version`) | Print which build this is: version and where it came from, Git revision and tree state, commit time, toolchain, platform. `--json` prints the same as one object (`name`, `version`, `version_from`, `revision`, `tree_state`, `committed`, `module`, `go`, `platform`; an identity fact the binary does not record is omitted) plus `withholds_provider_credentials` (boolean) and `withheld_provider_env` (sorted array of variable names): the provider credentials no child of this build inherits. A host deciding whether to start `manvi` with a credential in its environment checks that every name it would inject is in `withheld_provider_env`; a build that omits the two keys predates the guarantee and does not withhold. See [Child processes](#child-processes). |
| **System Diagnostics** | `manvi doctor` | Validate active configuration, verify SQLite store reachability, and detect weakened security gates. |
| **Headless Turn** | `manvi run -p "PROMPT"` | Drive a single agent turn without terminal interaction. Ideal for scripts, CI, and benchmarks.<br/>• `--json`: Output structured JSON turn summary.<br/>• `--max-steps N`: Cap maximum execution steps (default 500).<br/>• `--timeout D`: Bounded turn duration (e.g. `10m`).<br/>• `--quiet`: Suppress streaming prose. |
| **Settings Inspection** | `manvi flags [--all]` | Display all configuration flags, active values, safety ratings, and origin layers. |
| **Runtime Flag Modification** | `manvi flags set KEY VALUE` | Set a configuration value for the current process. Fails closed if the flag is marked `startup`-only or moves a safety setting illegally. |
| **Local LLM Discovery** | `manvi local [--resolve]` | Scan loopback ports to discover active local LLM servers (Ollama, MLX, vLLM, llama.cpp, LM Studio, Jan). `--resolve` displays the chosen candidate with origin annotations. |
| **Safe Pull** | `manvi pull` | Fast-forward the current branch with `git pull --ff-only`. Refuses to run while the working tree has staged, modified, or untracked files. |
| **Safe Push** | `manvi push` | Push the current branch to its configured upstream with no force or refspec override. |
| **Issue Report** | `manvi issues` | Report up to 100 open GitHub issues through the authenticated `gh` CLI, sorted by latest activity. A 100-row result is explicitly marked as capped. |
| **Stdio Host Server** | `manvi serve [--posture host\|devcouncil]` | Expose policy evaluation, context preparation, and completion parsing over NDJSON stdio for IDE extensions and external tools. |
| **Task Lease Listing** | `manvi lease list` | Display active task checkouts, lease owners, tokens, and remaining TTLs. |
| **Task Lease Acquisition** | `manvi lease acquire TASK OWNER [--ttl 15m]` | Atomically checkout a task and claim an exclusive lease. Fails if task is already held. |
| **Task Lease Release** | `manvi lease release TASK TOKEN` | Atomically relinquish a held task lease using its cryptographic token. |
| **Policy Pre-Check** | `manvi check PATH [--task ID]` | Probe write gate policy for a file path without touching the filesystem; outputs decision, fired rules, and explanation. |
| **Human Override** | `manvi allow PATH --reason TEXT` | Record an audited human grant in the ledger to unblock a soft policy denial. |
| **Native Tools List** | `manvi tools` | List all 45 native tools and their parameter schemas. |
| **Direct Tool Execution** | `manvi tool NAME [--json ARGS]` | Execute a native tool directly over the same dispatch path an agent takes. |
| **Provider Detection** | `manvi providers` | List supported LLM providers and detect present API keys/credentials in environment. |
| **Sub-agent Roles** | `manvi agents` | Show each sub-agent role, the tier it runs on, and the model that tier resolves to for the default provider (role → tier → model). |
| **Tier Profile** | `manvi tiers` | Print the model and effort each tier runs at — `advisor`, `main`, `heavy`, `lite`, `basic` — the setting and layer each value came from, and the effort ceiling each tier inherits. Runs the same check a session runs at startup, prints the table either way, and exits 1 on a profile a session would refuse. See [Tier profile](#tier-profile). |
| **Anthropic Spend** | `manvi budget` | Show Anthropic spend (settled, retained, in flight, remaining) against `MANVI_ANTHROPIC_BUDGET_USD`. With that variable set, every Anthropic request from any project or session draws on one ledger, and a request whose worst-case cost would pass the limit is refused before it is sent. The ledger defaults to `<user config dir>/manvi/budget/anthropic.json`; set `MANVI_ANTHROPIC_BUDGET_LEDGER` to an absolute path to move it. Managed Claude agents started by `manvi serve` never inherit `ANTHROPIC_API_KEY`, cap or no cap (see [Child processes](#child-processes)), so spend they could make on it never bypasses the ledger. `manvi budget reconcile` moves attempts left in flight by an exited process into retained spend (still counted against the limit); every reservation does the same before it is admitted. |
| **Event Wire Watcher** | `manvi watch [--json]` | Stream the typed `ui.Event` wire to terminal or emit raw NDJSON lines for CI pipelines. |
| **Code Graph Build** | `manvi map build` | Index repository symbols and AST relationships into `.devcouncil/code_graph.json`. |
| **Code Graph Rebuild** | `manvi map rebuild` | Discard existing derived index and regenerate the code graph from scratch. |
| **Code Graph Status** | `manvi map status` | Report code graph freshness, indexed symbols, areas, and rule permissiveness. |
| **Wire Probe** | `manvi probe PROVIDER` | Dispatch a live test request with a real tool call to certify provider wire contracts. |
| **Module Mark** | `manvi logo [--svg]` | Render the 7×7 module mark to the terminal or export pure SVG. |

---

## Headless Exit Codes (`manvi run`)

Scripts and CI systems should inspect exit codes from `manvi run`. Six statuses
are distinguished, because a caller that cannot tell them apart commits half-done
work as if it were finished. `manvi run --help` prints the same list.

| Exit Code | Meaning | Interpretation |
|---|---|---|
| `0` | **Success** | The turn finished on its own. |
| `1` | **Failure** | The run failed — an error, a policy rejection, or a tool failure. |
| `2` | **Step Ceiling Reached** | The step ceiling ended the turn before completion. Work is incomplete; CI should **not** commit partial changes. |
| `3` | **Output Cap Reached** | The output cap cut the final answer off mid-sentence. Read the answer before acting: one that was going somewhere wants a larger `llm.local.max_output_tokens`; one that was repeating itself was being held in check. Kept apart from `2` because the fix differs. |
| `4` | **No Answer** | The turn ended with no text and no tool call. Neither ceiling was reached, so raising one fixes nothing. |
| `5` | **Unfinished** | There is an answer, but the stream did not end on a stop reason meaning the work was done — a dropped connection, an unmapped status, or a refusal. stderr says which. Retrying helps the first and not the last. |

### Pre-Check Exit Codes (`manvi check`)

`manvi check` reports its decision as a status, so that `manvi check "$f" && commit`
does not treat every block as a pass:

| Exit Code | Meaning | Interpretation |
|---|---|---|
| `0` | **Not blocked** | The write would be allowed. |
| `6` | **Soft block** | Blocked by a rule a grant can clear. The `manvi allow` line that clears it is printed. |
| `7` | **Hard block** | Blocked by a hard rule, which no grant clears by any authority. Do not retry after issuing an override. |
| `1` | **Failure** | The command itself failed. |

These are pinned to the dispatch by `manvi/internal/contract`; a code added in one
place and not the other fails the suite.

---

## Configuration & Flag Catalogue

Every setting in MANVI can be configured via environment variables (`MANVI_<KEY>`), `.devcouncil/config.yaml`, or runtime flags.

### Mutability Scopes

Every setting has an explicit **mutability scope**:
- **`human`**: May be updated at runtime by a person via `manvi flags set KEY VALUE` or `/flags set KEY VALUE` in the interactive TUI.
- **`startup`**: Frozen once boot completes and the flag registry is sealed. Four flags are `startup`-only: `harness.init.enabled`, `mcp.config`, `policy.hard_rules.enabled`, and `log.model_visible_assert`. The last two are also safety-critical, so nothing can move them mid-turn.

Safety-critical and `startup` are separate properties. Most safety flags — `harness.posture`, both gate modes, the grant ceilings, `verify.rigor.enabled` — are `human`: movable at runtime, but only on human authority, never by an agent. `manvi flags --all` marks safety flags with `!` and prints each flag's mutability in the last column; that output, not this table, is the live answer.

> [!IMPORTANT]
> The model/agent execution plane has **no access** to the flag registry. An agent cannot alter its own gates.

> [!NOTE]
> `startup` bounds *when* a value may move, not *who* may supply it. Every flag, `startup` ones included, can be set before boot from `MANVI_<KEY>` or from `.devcouncil/config.yaml` — which the managed `.gitignore` rules deliberately keep committable (`.devcouncil/*` followed by `!.devcouncil/config.yaml`), so it arrives with a `git clone`. Settings resolve lowest-first: catalogue default, then `.devcouncil/config.yaml`, then `MANVI_<KEY>`. Run `manvi flags` to see which layer supplied each value in force. The `llm.tier.*` keys are the exception: they are refused in `.devcouncil/config.yaml` and read from the user config instead — see [Tier profile](#tier-profile).

### Full Flag Catalogue

## Operator-scope settings

Four values are read from the process environment and from nowhere else. All four are
deliberately absent from the settings table below, because settings load from
`.devcouncil/config.yaml` — a file inside the repository, protected by a rung
that a relaxed posture switches off. A value the agent can write into the
repository is not a value that can authorise anything.

| Variable | Effect |
|---|---|
| `MANVI_VERIFY_COMMAND` | The command the end-of-turn check runs over a mutating turn's changes. Unset means the built-in path-scoped gates run alone. |
| `MANVI_FETCH_HOSTS` | Comma- or space-separated hosts `devcouncil_fetch_url` may reach. **Unset means no outbound network access, and the tool is not offered at all.** |
| `MANVI_ANTHROPIC_BUDGET_USD` | Account-wide Anthropic spend cap in dollars (`200`, `49.50`). Unset means unmetered. A value that is set but unusable stops the Anthropic provider from starting. |
| `MANVI_ANTHROPIC_BUDGET_LEDGER` | Absolute path of the shared ledger. Unset means `<user config dir>/manvi/budget/anthropic.json`. The ledger is per machine: attempts whose process has exited are reclaimed by pid. |

```bash
MANVI_VERIFY_COMMAND="go test ./..." \
MANVI_FETCH_HOSTS="go.dev,pkg.go.dev" \
  manvi run -p "update the retry helper for the new API"
```

## Other environment variables the process reads

Beyond `MANVI_<KEY>` for every setting and the operator-scope values above, the
binary reads the names below. They are not settings: none is in the flag
catalogue, `manvi flags set` cannot change them, and each is read directly from the
environment. `manvi/internal/contract` (`TestEveryEnvironmentVariableTheHarnessReadsIsDocumented`)
parses the production source for every name the code reads and fails when one is
missing from this section, so a new read cannot land undocumented. It also reports
how many reads it could not resolve to a literal name, and refuses a new one unless
it is listed with the reason.

**Credentials.** For each provider the first variable listed that is set and not blank
wins; a variable that is set but blank is skipped, not treated as a credential. A value
held in the harness's credential store ahead of the environment outranks all of them.

| Variable | Provider | Notes |
|---|---|---|
| `ANTHROPIC_API_KEY` | `anthropic` | |
| `XAI_API_KEY`, `GROK_API_KEY` | `xai` | In that order. |
| `GEMINI_API_KEY`, `GOOGLE_API_KEY` | `gemini` | In that order. A Google Cloud console API key; the same `x-goog-api-key` header either way. |
| `LOCAL_API_KEY`, `OPENAI_API_KEY` | `local` | Optional: a local server that checks no key needs none. `OPENAI_API_KEY` is honoured only for a base URL on this machine, so a key set for another vendor's service is never sent to an arbitrary host. |
| `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`, `GITHUB_ENTERPRISE_TOKEN` | none | Never sent anywhere by the harness. They are read so the output scrubber of the quick-action commands can redact their values from what a command prints. |

**Endpoints.**

| Variable | Effect |
|---|---|
| `ANTHROPIC_BASE_URL` | Host root for the `anthropic` provider (`https://api.anthropic.com`, or a gateway's root), read the way the vendor SDK and Claude Code read it: requests go to `<root>/v1/messages`. A value that already ends in `/v1`, with or without a trailing slash, is accepted and that segment dropped, so `https://api.anthropic.com/v1` still works. Unset means Anthropic's own endpoint. The other providers' base URLs are settings (`llm.xai.base_url`, `llm.gemini.base_url`, `llm.local.base_url`), not variables of this kind. |
| `ANTHROPIC_WORKSPACE_ID` | Workspace an `anthropic` API key that is not scoped to one acts in (`wrkspc_...`), sent as the `anthropic-workspace-id` header. Unset or blank sends no header. A value that is not letters, digits, `_` and `-`, or is too long, refuses to build the provider, naming the variable, and `manvi providers` reports the key as misconfigured. |
| `ANTHROPIC_API_KEY_FALLBACK` | A second `anthropic` key, from a different Console organisation. Credit belongs to an organisation and cannot be pooled, so this is how a second funded organisation is used. When the primary key's request is refused with a 400 `invalid_request_error` saying the credit balance is too low, that same request is sent once on this key, and every later request in the process goes straight to it. Nothing else fails over: not a rate limit (429), a spend limit you set (also a 400, with a different message), a 402, a 404 workspace or a 5xx, and never an error that arrives inside a stream the API already accepted, so no tool call or partial response is ever replayed. `manvi run` and `manvi probe` print a line naming the variable when the switch happens; the TUI and `manvi serve` do not surface it yet. `manvi providers` and `manvi doctor` report the key's source and length beneath the `anthropic` row, never its value. Unset means no failover. |
| `ANTHROPIC_WORKSPACE_ID_FALLBACK` | Workspace the fallback key acts in, under the same rules as `ANTHROPIC_WORKSPACE_ID`. It is the fallback's own: the primary's workspace belongs to the primary's organisation and is a 404 `Workspace not found` under any other key. Set without `ANTHROPIC_API_KEY_FALLBACK`, it is reported as misconfigured. |

**Binaries.** Each names a path and is used as given when set; it is never second-guessed.
Unset, the harness looks the program up on `PATH`, then in this repository's own
`crates/target` builds, and otherwise reports it missing by its bare name.

| Variable | Program | Used by |
|---|---|---|
| `MANVI_MAP_BINARY` | `devmap` | code graph, neighbour rule, `manvi map` |
| `MANVI_STORE_BINARY` | `dcstore` | task and lease store, workbench store |
| `MANVI_VERIFY_BINARY` | `dcverify` | the end-of-turn check, `manvi serve` verification, `manvi doctor` |
| `MANVI_GREP_BINARY` | `dcgrep` | repository search |
| `MANVI_CLAUDE_BINARY` | `claude` | managed Claude coding-agent runs in `manvi serve` |
| `MANVI_CODEX_BINARY` | `codex` | managed Codex coding-agent runs in `manvi serve` |
| `MANVI_DEVCOUNCIL_BINARY` | DevCouncil inspect CLI | `devcouncil_dev_inspect` only. An explicit pin, never a `PATH` guess: unset, the tool reports itself unavailable. |

**Project root.** `MANVI_PROJECT_ROOT`, then `MANVI_ROOT`, then `DEVCOUNCIL_ROOT`: the
first that is set and not blank after trimming whitespace is the project root, ahead
of the nearest ancestor holding `.devcouncil` or `.git`. A path that does not exist
is still honoured, because naming a root is a deliberate act.

**Verification and coverage.**

| Variable | Effect |
|---|---|
| `MANVI_COVERAGE` | A Go coverprofile or LCOV report for the diff-coverage gate. Unset, every changed file is reported unmeasured and the run says so, rather than reading as covered. `--coverage` is the per-invocation form. |

**Terminal and display.** Conventions of the terminal, not of this harness, except
`MANVI_FX`.

| Variable | Effect |
|---|---|
| `MANVI_FX` | `off`, `0`, `false`, `no` or `disabled` turns ambient animation off. Anything else leaves it on. |
| `NO_COLOR` | Any value, set at all, turns colour off. It outranks every capability check. |
| `TERM` | `dumb` or empty turns colour off; a name containing `256`, `truecolor` or `direct` selects the colour depth. |
| `COLORTERM` | `truecolor` or `24bit` selects 24-bit colour. |
| `COLUMNS`, `LINES` | Fallback terminal size when the terminal cannot report one, and the width `manvi logo` draws at (default 80 by 24). |
| `LC_ALL`, `LC_CTYPE`, `LANG` | The first of these that is non-empty decides whether box-drawing and block characters are used: a value naming UTF-8 turns them on. |

**Platform conventions.** `HOME`, `TMPDIR`, `USER` and `PATH` are read for names the
operating system defines, and carry no harness meaning: `HOME` and `TMPDIR` are two of
the directories the project-root search stops at instead of adopting (a stray
`.devcouncil` or `.git` there is debris, not a project), `USER` is the operator name
recorded on a grant issued from the CLI, and `PATH` is program lookup.

<a id="child-processes"></a>**Child processes.** No child of this process inherits a
provider credential. The names are every variable in the credential catalogue —
each provider's key variables above plus `ANTHROPIC_WORKSPACE_ID`,
`ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY_FALLBACK` and
`ANTHROPIC_WORKSPACE_ID_FALLBACK` — and `manvi --version --json` prints them as
`withheld_provider_env` (the serve `hello` repeats it). A host may therefore put a
key and workspace in this process's environment for the `anthropic` provider
without git hooks, coding agents or anything else Manvi starts seeing them.
`manvi/childenv` owns the rule, and `TestNoChildInheritsThisProcessEnvironment`
fails on any production spawn site that does not assign an environment, or that
reads `os.Environ()` outside that package.

- A command the agent runs, and an MCP server it starts, receive only the names on
  an allowlist: `childEnvAllowlist` (`manvi/devcouncil/exec_tools.go`, for
  commands) and `baseEnvAllowlist` (`manvi/mcp/client.go`, for MCP servers, plus
  the names a server's own manifest declares in `EnvPassthrough`).
- Git — every git child of `manvi serve`, of the bootstrap and of the agent's git
  tools — receives only the names on `gitAllowlist` (`manvi/childenv/childenv.go`):
  program lookup, home and configuration, locale, temp, commit identity, the SSH
  agent socket and askpass, GPG, and proxies. A repository's hooks, filters and
  fsmonitor run with that, and a parent's `GIT_DIR` or `GIT_INDEX_FILE` no longer
  steers Manvi's own git.
- The operator's own tools — managed Codex and Claude agents and the `claude
  --version` probe, the desktop broker, the verifier, the `devcouncil` CLI, and
  `pull`/`push`/`issues` — inherit this process's environment with the withheld
  names removed, because what they need cannot be enumerated. A managed Claude
  agent therefore runs on the CLI's own login (`CLAUDE_CODE_OAUTH_TOKEN`, which is
  not withheld, or the login the CLI stored), never on API billing through the
  host's key.

The lists are policy, so the exact names live in them and nowhere else.

**Development only.** `manvi/cmd/geminiproxy` is a wire-capture proxy for
developing the Gemini adapter and is not part of the shipped binary.

| Variable | Default | Effect |
|---|---|---|
| `PROXY_ADDR` | `127.0.0.1:8899` | Address the proxy listens on. |
| `PROXY_CAPTURE` | `gemini-wire.log` | File the captured traffic is written to. |
| `PROXY_UPSTREAM` | the real Gemini API | Where requests are forwarded; point it at a stand-in to smoke-test without a live request. |

| Flag Key | Environment Variable | Default | Safety Critical | Mutability | Description |
|---|---|---|---|---|---|
| `harness.posture` | `MANVI_HARNESS_POSTURE` | `dev` | **Yes** | `human` | Security posture: `dev` (advisory scope), `strict` (enforce all), `yolo` (all gates off). |
| `harness.init.enabled` | `MANVI_HARNESS_INIT_ENABLED` | `true` | No | `startup` | Auto-create the state directory and its `.gitignore` rules before commands that keep state. |
| `policy.file.mode` | `MANVI_POLICY_FILE_MODE` | `enforce` | **Yes** | `human` | File write gate mode (`enforce`, `advisory`, `off`). |
| `policy.command.mode` | `MANVI_POLICY_COMMAND_MODE` | `enforce` | **Yes** | `human` | Shell command gate mode (`enforce`, `advisory`, `off`). |
| `policy.hard_rules.enabled` | `MANVI_POLICY_HARD_RULES_ENABLED` | `true` | **Yes** | `startup` | Hard safety rules (outside-root, secret paths, restricted paths, git safety). Off under `yolo`, and off under **any** posture when this flag is set to `false` explicitly — from the environment or from a committed `.devcouncil/config.yaml`. See the note under the posture matrix. |
| `policy.scope.allow_neighbors`| `MANVI_POLICY_SCOPE_ALLOW_NEIGHBORS`| `true` | No | `human` | Allow writes to adjacent subsystems per AST code graph. |
| `policy.scope.allow_same_dir` | `MANVI_POLICY_SCOPE_ALLOW_SAME_DIR` | `true` | No | `human` | Fallback: allow writes in the same directory as a planned file (recorded as degraded). |
| `grants.enabled` | `MANVI_GRANTS_ENABLED` | `true` | No | `human` | Master switch for human and agent override seam. |
| `grants.agent.enabled` | `MANVI_GRANTS_AGENT_ENABLED` | `true` | **Yes** | `human` | Permit sub-agents to issue short-lived overrides within scope. |
| `grants.agent.max_ttl` | `MANVI_GRANTS_AGENT_MAX_TTL` | `15m` | **Yes** | `human` | Maximum duration for agent-issued overrides. |
| `grants.human.max_ttl` | `MANVI_GRANTS_HUMAN_MAX_TTL` | `8h` | No | `human` | Maximum duration for human-issued overrides. |
| `grants.require_reason` | `MANVI_GRANTS_REQUIRE_REASON` | `true` | **Yes** | `human` | Require a non-empty reason on every grant, so the evidence report can say why a block was cleared. Movable at runtime by a person via `manvi flags set`. |
| `grants.agent.allow_commands` | `MANVI_GRANTS_AGENT_ALLOW_COMMANDS`| `false` | **Yes** | `human` | Permit agent overrides on command allowlist blocks. |
| `verify.diff_coverage.enforce`| `MANVI_VERIFY_DIFF_COVERAGE_ENFORCE`| `false` | No | `human` | Treat unexercised diff lines as blocking verification gaps. |
| `verify.rigor.enabled` | `MANVI_VERIFY_RIGOR_ENABLED` | `true` | **Yes** | `human` | Detect stubs, TODOs, and unimplemented markers in added diff lines. |
| `log.model_visible_assert` | `MANVI_LOG_MODEL_VISIBLE_ASSERT` | `true` | **Yes** | `startup` | Assert model-visible context matches append-only session log before LLM calls. |
| `llm.provider.default` | `MANVI_LLM_PROVIDER_DEFAULT` | `anthropic`| No | `human` | Default LLM provider (`anthropic`, `gemini`, `xai`, `local`). |
| `llm.effort` | `MANVI_LLM_EFFORT` | `""` | No | `human` | Reasoning effort level passed to provider (`low`, `medium`, `high`). On `anthropic`, when empty, the main tier's effort (`llm.tier.main.effort`) applies instead. |
| `llm.local.base_url` | `MANVI_LLM_LOCAL_BASE_URL` | `http://127.0.0.1:8000/v1` | No | `human` | Local OpenAI-compatible server URL. |
| `llm.local.model` | `MANVI_LLM_LOCAL_MODEL` | `""` | No | `human` | Local model ID. If empty, falls back to `MANVI_MODEL`. |
| `llm.local.context_window` | `MANVI_LLM_LOCAL_CONTEXT_WINDOW` | `32768` | No | `human` | Fallback token budget when server publishes no window. |
| `llm.local.max_output_tokens` | `MANVI_LLM_LOCAL_MAX_OUTPUT_TOKENS`| `16384` | No | `human` | Output token ceiling per request to prevent truncation. |
| `llm.local.temperature` | `MANVI_LLM_LOCAL_TEMPERATURE` | `0.7` | No | `human` | Sampling temperature for local model completions, 0 to 2. Deliberately below the `1.0` Qwen3's `generation_config.json` recommends, and above the `0.1` this once shipped: near-greedy decoding is a documented cause of repetition loops. Empty omits the field. |
| `llm.local.top_p` | `MANVI_LLM_LOCAL_TOP_P` | `0.95` | No | `human` | Nucleus sampling probability threshold. |
| `llm.local.top_k` | `MANVI_LLM_LOCAL_TOP_K` | `20` | No | `human` | Top-K candidate cap, as Qwen3's shipped `generation_config.json` declares. Must be 0 or more. Not an OpenAI field, so servers that do not implement it ignore it. Empty omits the field. |
| `llm.local.min_p` | `MANVI_LLM_LOCAL_MIN_P` | `""` | No | `human` | Min-P dynamic probability sampling threshold (e.g. `0.05`), 0 to 1. Unset by default: the field is omitted and the server's own default applies. |
| `llm.local.repetition_penalty`| `MANVI_LLM_LOCAL_REPETITION_PENALTY`| `""` | No | `human` | Repetition penalty factor, 0 or more. Unset by default: the field is omitted and the server's own default applies. |
| `llm.local.presence_penalty` | `MANVI_LLM_LOCAL_PRESENCE_PENALTY` | `""` | No | `human` | Presence penalty, -2 to 2. Unset by default: the field is omitted and the server's own default applies. |
| `llm.local.frequency_penalty`| `MANVI_LLM_LOCAL_FREQUENCY_PENALTY`| `""` | No | `human` | Frequency penalty, -2 to 2. Unset by default: the field is omitted and the server's own default applies. |
| `llm.local.seed` | `MANVI_LLM_LOCAL_SEED` | `""` | No | `human` | Deterministic sampling seed. Unset by default: the field is omitted, and runs are not reproducible until one is set. |
| `llm.local.stop` | `MANVI_LLM_LOCAL_STOP` | `""` | No | `human` | Comma-separated custom stop tokens. |
| `llm.local.stall_timeout` | `MANVI_LLM_LOCAL_STALL_TIMEOUT` | `5m` | No | `human` | Abandon a stream that produces no bytes for this long. Bounds the gap between tokens, not the length of the response: first-token latency on a large local model can exceed a hosted model's entire reply. `0`, `0s`, `off` or `none` disable it; a negative value is refused. |
| `llm.local.supports_tools` | `MANVI_LLM_LOCAL_SUPPORTS_TOOLS` | `true` | No | `human` | Declares whether local model endpoint supports function/tool calling. |
| `llm.local.supports_reasoning`| `MANVI_LLM_LOCAL_SUPPORTS_REASONING`| `false`| No | `human` | Declares whether local server accepts `reasoning_effort`. |
| `llm.local.assume_reasoning_prefill`| `MANVI_LLM_LOCAL_ASSUME_REASONING_PREFILL`| `false`| No | `human` | Reclassify unclosed thinking blocks assuming template prefilled `<think>`. |
| `llm.local.core_tools_only` | `MANVI_LLM_LOCAL_CORE_TOOLS_ONLY` | `false` | No | `human` | Narrow offered tools to core set for smaller local models. |
| `llm.local.trust_declared_context`| `MANVI_LLM_LOCAL_TRUST_DECLARED_CONTEXT`| `false`| No | `human` | Trust configured context window over server-published dimensions. |
| `llm.local.assume_model_served`| `MANVI_LLM_LOCAL_ASSUME_MODEL_SERVED`| `false`| No | `human` | Skip `/v1/models` discovery for servers missing the endpoint. |
| `agents.max_spawn_depth` | `MANVI_AGENTS_MAX_SPAWN_DEPTH` | `2` | No | `human` | Maximum sub-agent delegation depth. A ceiling in code, not a prompt instruction. **Forced to `0` when the default provider is `local`**, whatever this is set to: children there contend with the parent for one device's weights and memory, so a local session delegates nothing. The refusal names that reason rather than this setting. |
| `agents.max_fanout` | `MANVI_AGENTS_MAX_FANOUT` | `8` | No | `human` | Maximum concurrent sub-agents per parent. A fan-out request above it is refused, never silently trimmed. Narrowed further when any child is *placed* on `local` — the pool has one concurrency limit, and a frontier parent dispatching local builders would otherwise run the frontier width against one device. The narrowing is reported in the dispatch result. |
| `subagents.dynamic.enabled` | `MANVI_SUBAGENTS_DYNAMIC_ENABLED` | `true` | No | `human` | Let a model define new subagent role types at runtime. Off refuses `devcouncil_define_subagent`, naming this setting; the built-in roles stay invocable. |
| `pair.questions.enabled` | `MANVI_PAIR_QUESTIONS_ENABLED` | `true` | No | `human` | Enable interactive pair-programming question asking. |
| `mcp.enabled` | `MANVI_MCP_ENABLED` | `true` | No | `human` | Discover and register MCP 2.0 servers and Open Plugin 1.0 manifests. Off means no declaration file or plugin directory is read and every `mcp_*` tool refuses, naming this setting. Read when a tool surface is built, so a change applies to sessions opened after it. |
| `mcp.config` | `MANVI_MCP_CONFIG` | `.devcouncil/mcp.json` | No | `startup` | Server declaration file, relative to the repository root or absolute. Set explicitly, that file and only that file is read and it must exist. Left at the default, `./mcp.json` and `./.mcp.json` are scanned too and absence is not an error. A file that exists and does not parse fails the run. |
| `max_steps` | `MANVI_MAX_STEPS` | `500` | No | `human` | Step ceiling for one turn. Undotted and top-level so that the long-standing `MANVI_MAX_STEPS` keeps applying. Spent as a budget: a step that made observable progress costs one, a step whose tool calls changed nothing costs three. `manvi run --max-steps N` overrides it for one invocation. |
| `llm.effort.max` | `MANVI_LLM_EFFORT_MAX` | `""` | No | `human` | Highest reasoning effort the agent loop may raise `llm.effort` to within one turn. A tool call refused as a verbatim repeat, or for making no observable progress, raises the tier one rung. Empty never raises it. Requires `llm.effort` to be set, and must be above it. |
| `llm.xai.base_url` | `MANVI_LLM_XAI_BASE_URL` | `https://api.x.ai/v1` | No | `human` | Base URL for the xAI (Grok) adapter. Overridable for a proxy or gateway — see the warning below. |
| `llm.gemini.base_url` | `MANVI_LLM_GEMINI_BASE_URL` | `https://generativelanguage.googleapis.com/v1beta` | No | `human` | Base URL for the Gemini adapter. Overridable for a proxy or gateway — see the warning below. |
| `llm.local.dynamic_tools` | `MANVI_LLM_LOCAL_DYNAMIC_TOOLS` | `true` | No | `human` | On-demand tool loading for local models: start from a lean core set and load extended tools via `devcouncil_search_tools` / `devcouncil_activate_tools`. On the shipped registry that is 45 tools and ~5,345 estimated schema tokens reduced to 25 and ~2,688. Unlike `core_tools_only` this is a floor rather than a ceiling — an omitted tool can still be fetched. |
| `llm.local.guidance_router` | `MANVI_LLM_LOCAL_GUIDANCE_ROUTER` | `true` | No | `human` | Route the system prompt at compact density for local models: the same sections, worded tighter. Measured at ~901 estimated tokens down to ~696. Rules are never compressed or dropped — identity, posture, task rules, the policy rule and the tool contract survive any budget. |

> [!WARNING]
> `llm.xai.base_url` and `llm.gemini.base_url` name the host that receives the
> provider API key. Both are settable from a committed `.devcouncil/config.yaml`,
> and neither is marked safety-critical — so neither appears in the **WEAKENED**
> banner that `manvi doctor` and the run report print. A repository that ships a
> `config.yaml` pointing one of them at a host you did not choose will send your
> credential there without any gate saying so. Read a checked-out `config.yaml`
> before running against it, and confirm both values with `manvi flags --all`,
> which reports `config` as the origin of anything that file set.

`MANVI_TUI_THEME` (`dark`, `light`, `plain`) is **not** a catalogue flag. It is read
directly with `os.Getenv` in `ui/tui/theme.go`, so it does not appear in `manvi flags`,
carries no origin, and cannot be moved with `manvi flags set`. An unrecognised value
is not an error: it falls back silently to `dark` (or to `plain` when the terminal has
no colour). That is a deliberate exception to the registry's rule that an unknown key
is an error and never a no-op.

---

## Tier profile

MANVI runs a tiered harness: each class of work names a **tier**, and the
operator's profile says which model serves it. Tiers are ordered cheapest
first — `basic`, `lite`, `heavy`, `main`, `advisor` — and defined in
`manvi/agents/tier.go` with no provider knowledge; the shipped ids below are
checked against the provider catalogue when a session starts.

| Tier | Serves | Model key | Default | Effort key | Default |
|---|---|---|---|---|---|
| `advisor` | The reviewer the main agent consults | `llm.tier.advisor` | `claude-fable-5-1` | `llm.tier.advisor.effort` | `""` |
| `main` | The session's own agent | `llm.tier.main` | `claude-opus-5-5` | `llm.tier.main.effort` | `""` |
| `heavy` | Sub-agents that write | `llm.tier.heavy` | `claude-opus-5-5` | `llm.tier.heavy.effort` | `""` |
| `lite` | Read-only sub-agents | `llm.tier.lite` | `claude-sonnet-5-5` | `llm.tier.lite.effort` | `""` |
| `basic` | Wide, simple fan-out | `llm.tier.basic` | `claude-haiku-5-5` | `llm.tier.basic.effort` | `""` |

Every key is a string, mutability `human`, not a safety flag, and set from the
environment as `MANVI_LLM_TIER_<TIER>` and `MANVI_LLM_TIER_<TIER>_EFFORT`. An
empty effort means the tier sets none of its own: the main tier then uses
`llm.effort` (and omits the field when that is empty too), and a sub-agent
carries its parent's effort, as sub-agents always have.

**Operator scope only.** Tier keys decide spend, so they are read from three
places, lowest first:

1. The **user config**: `<user config dir>/manvi/config.yaml` (on macOS
   `~/Library/Application Support/manvi/config.yaml`, on Linux
   `~/.config/manvi/config.yaml`), or the absolute path in `MANVI_USER_CONFIG`.
   It uses the same format as `.devcouncil/config.yaml`, carries `llm.tier.*`
   keys only — any other key is refused, naming it — and is refused outright if
   it resolves, through the variable or a symlink, to a path inside the
   repository. Values from it show as origin `config` in `manvi flags` and as
   `user config <path>` in `manvi tiers`. A process with no user config
   directory (no `HOME`) and no `MANVI_USER_CONFIG` has no user config, as if
   the file were absent; a relative `MANVI_USER_CONFIG` is refused.
2. The environment, `MANVI_LLM_TIER_*`.
3. `manvi flags set llm.tier.<tier> MODEL` (or `/flags set` in the TUI), on human
   authority.

A `.devcouncil/config.yaml` that sets any `llm.tier.*` key is **refused at
startup, in every posture** — every command that loads settings, `manvi flags`
included, exits 1 naming the file and the keys. That file ships with a clone
and an agent can write it under a relaxed posture, so it must not be able to
choose a dearer model; refusing rather than ignoring it keeps a line that would
not apply from looking applied. A model's tool call cannot move a tier either:
the keys are `human`, and the registry refuses agent authority.

```yaml
# ~/.config/manvi/config.yaml
llm.tier.lite: claude-haiku-5-5
llm.tier.advisor.effort: max
```

**The main tier and the existing settings.** On `anthropic`, `llm.tier.main`
supplies the session's model when `MANVI_MODEL` is unset; `MANVI_MODEL` still
wins when set. Likewise `llm.effort` wins over `llm.tier.main.effort` when it is
non-empty. `manvi tiers` reports the main row as the session will run it,
naming `MANVI_MODEL` or `llm.effort` when one of them supplied the value.

**Startup checks.** `manvi run` exits 1 before contacting the provider, and a
TUI session refuses its first prompt when it attaches the provider, naming the
tier and the setting to change, when any tier:

- names a model the provider's catalogue does not serve;
- names a model with no price while `MANVI_ANTHROPIC_BUDGET_USD` caps spend,
  since the metered provider refuses a request it cannot price;
- sets an effort its model cannot honour: an effort on a model that does not
  reason, or one outside the model's effort ladder.

A tier inherits the session's `llm.effort.max` only where its model reasons,
the tier sets an effort, and the ceiling is above it. Otherwise the tier runs
at its own effort throughout rather than being refused for a ceiling it never
set. A ceiling the tier's model has no rung for at all is refused. `manvi tiers`
prints each tier's inherited ceiling.

**Providers.** The profile is catalogued against `anthropic` only, because a
session's sub-agents can be placed only on its own provider. Under any other
`llm.provider.default` the profile is reported by `manvi tiers` as inactive and
not applied, and `manvi run` notes any tier key that was set but does not apply.

---

## Posture Matrix

```
harness.posture = dev      soft rules report; hard rules block (shipped default)
harness.posture = strict   everything enforces; soft rules block
harness.posture = yolo     every gate off, hard rules and root containment included
```

| Feature / Gate | `strict` | `dev` (Default) | `yolo` |
|---|---|---|---|
| **Root Containment** | Enforced¹ | Enforced¹ | *Off* — writes can land anywhere this process can write |
| **Credential & Git Hard Rules** | **Enforced**¹ | **Enforced**¹ | *Off* (Recorded as degraded) |
| **Task Scope Soft Rules** | **Enforced** | *Advisory* (Recorded as demoted) | *Off* |
| **Operator Escalation (TUI)** | Prompts Modal | No Prompts | No Prompts |
| **Run Classification** | Strict | Weakened | Weakened |

> [!IMPORTANT]
> ¹ **The posture is not the only thing that reaches the hard rules.** Root
> containment and the credential, restricted-path and git-safety rules are one
> mechanism, governed by `policy.hard_rules.enabled`. The posture decides that
> flag only while the flag is still on its default: an explicitly set value wins
> over the posture in both directions. So `policy.hard_rules.enabled=false` turns
> root containment off under `dev` and under `strict` too, and it can be set from
> the environment *or* from `.devcouncil/config.yaml` — the one file under
> `.devcouncil/` that the managed `.gitignore` rules deliberately leave
> committable, so it arrives with a `git clone`.
>
> What is preserved is the record, not the containment. A run in that state
> reports `hard rules off (config)`, lists `policy.hard_rules.enabled = false` in
> the **WEAKENED** banner, and stamps `policy.hard_rules.disabled` on every
> decision it reached. Two refusals survive regardless, because neither is
> containment: a path whose text and whose meaning to the kernel differ (NUL or
> other control characters), and the repository root itself. Check a checked-out
> `.devcouncil/config.yaml`, or run `manvi doctor`, before trusting a repository
> boundary.

---

## MCP Server Authorization

Spawning an MCP server process requires an operator authorization that the
repository cannot grant itself. **This is a breaking change for existing setups:
servers declared in a checked-out tree that used to start now refuse until
authorized.**

Declarations are classed by where they came from:

| Origin | Source | Spawning |
|---|---|---|
| `program` | `RegisterServer`, or `RegisterPlugin` with a manifest not read off disk — the operator's own build | Allowed |
| `workspace` | Read out of the checked-out tree: `.devcouncil/mcp.json`, `./mcp.json`, `./.mcp.json`, or a plugin manifest under `plugins/`, `.mcp/plugins`, `.devcouncil/plugins` | Requires authorization |

Only **spawning** is gated. Discovery still reads declaration files and registers
what it finds, and `manvi flags`, server listing and the tool surface are
unaffected — an unauthorized server surfaces its refusal as that server's error
entry in the tool listing rather than failing the whole survey.

**The fingerprint** is `sha256:` over the server name, command, arguments, the
declared environment, and the variables the declaration asks to have forwarded
from this process. Changing any of those produces a different fingerprint and
needs authorizing again, so a repository cannot get a declaration approved and
then quietly add `ANTHROPIC_API_KEY` to its passthrough list. The working
directory is deliberately not covered — it differs between clones of the same
repository. What a fingerprint cannot establish is that the program named by the
command still contains what it contained when you read it: `node server.js`
fingerprints the same however `server.js` changes.

**Where authorizations live:**

| Location | Notes |
|---|---|
| `<user config dir>/manvi/mcp-trust.json` | The default. On Linux this is `~/.config/manvi/mcp-trust.json`. Outside any repository, which is the point. |
| `MANVI_MCP_TRUST_FILE` | Absolute path to an alternative file. A relative path is refused. |
| `MANVI_MCP_TRUST` | Fingerprints directly, whitespace- or comma-separated, for headless and CI runs with no home directory. |

The file is read fresh on every check rather than cached, so an authorization
takes effect without restarting the harness — and so does a revocation.

**A trust file that resolves to a path inside the repository is refused outright**
and no authorization is read from it: an authorization a checked-out tree can
write is not an authorization. The same fail-closed rule covers an unreadable or
unparseable file — a check that could not run does not return the same answer as
a check that ran and found nothing.

A refused spawn prints the whole declaration, because "authorize this
fingerprint" is only a safe instruction if the thing being authorized is in front
of the person doing it:

```
mcp: server "NAME" is declared by workspace content (SOURCE) and no operator has authorized it, so it was not started.
  it would run: "COMMAND" "ARG" ...
  fingerprint:  sha256:...
authorize it by adding that fingerprint to <trust file path>, as {"authorized":[{"fingerprint":"sha256:...","name":"NAME"}]}
or by listing it in MANVI_MCP_TRUST. Read the command above before you do: authorizing it lets that repository run it with your account's privileges.
```

---

## Repository Auto-Preparation Lifecycle

Commands that keep state prepare the repository in which they run, so there is no separate init step: the bare `manvi` face, `tui`, `run`, `serve`, `watch`, `doctor`, `tool`, `allow`, `lease list|acquire|release`, and `map build|rebuild`.

Every other command leaves the working tree exactly as it found it: `help`, `version`, `flags`, `logo`, `providers`, `tools`, `local`, `probe`, `check`, `map status`, `pull`/`push`/`issues`, `gusset-check`, `<command> --help`, an unknown command, and a missing or unknown subcommand. An agent can probe a checkout with any of them without dirtying it.

| Step | Behavior |
|---|---|
| **State Directory** | `.devcouncil/` is created, along with the parent directories of any artifact paths (`MANVI_STORE_DB`, `MANVI_GRAPH`). Setting `MANVI_STATE_DIR` relocates all state directories uniformly. |
| **`.gitignore` Integration** | Only inside a git working tree. The managed rules cover the harness's own state and nothing wider: `.devcouncil/*` with `config.yaml` re-included, plus the anchored path of a state directory or artifact relocated inside the repository (a relocated SQLite store brings its `-wal`, `-shm` and `-journal` files). Missing rules are appended, never duplicated. A rule that would ignore a file the repository tracks is left out, and stderr names the file. Outside a git working tree no `.gitignore` is created. |
| **AST Navigation Index** | In `manvi` / `manvi tui`, code graph indexing runs asynchronously in the background so the initial UI frame renders instantaneously without waiting on large repository analysis. |

The repository-wide rules earlier releases appended (`*.sqlite`, `*.db`, `.claude*`, `.cursor/`, `logs/`, `node_modules/`, `.env*`, and so on) are no longer written. A repository that already received them keeps them, and can delete any it does not want. The reasoning is recorded beside `Sections` in `manvi/bootstrap/bootstrap.go`.

To disable automatic working-tree modifications, set `MANVI_HARNESS_INIT_ENABLED=false`.
