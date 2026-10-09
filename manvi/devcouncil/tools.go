// Package devcouncil is the native tool surface.
//
// These are DevCouncil's tools reimplemented in Go against the Rust state
// store. The Python `dev` CLI is deleted. An agent calling
// `devcouncil_checkout_task` reaches this code, and the lease it gets back is
// the same row `dcstore` and the native task tools read.
//
// Two properties hold across every tool here, and they are what make the
// surface safe to hand to an autonomous builder:
//
//   - **A tool that could not run never reports success.** An unreachable
//     store, an unparseable reply, a check that was skipped — each returns an
//     error result, never an empty-but-fine one. An empty list and "I could not
//     look" must not be the same answer.
//   - **Every refusal is routable.** Results carry the rule, the severity, and
//     — when an override could clear it — the exact request that would. An
//     agent should never have to parse prose to decide what to do next.
package devcouncil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/dcgrep"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/gate"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/repomap"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/artifacts"
	"github.com/bharathvbcr/Manvi/manvi/fetch"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/mcp"
	"github.com/bharathvbcr/Manvi/manvi/tools"
	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// Deps are what the tools need to do their work.
type Deps struct {
	Store *store.Client
	Gate  *gate.Gate
	// Root is the repository the tools read and write inside.
	Root string
	// LeaseTTL is how long a checkout lasts before it must be renewed.
	LeaseTTL time.Duration
	// Fetch is the documentation-lookup client. A nil or unconfigured one
	// removes the fetch tool from the surface entirely.
	//
	// SECURITY IMPACT: this is the only outbound network path in the harness.
	// It is off unless an operator names hosts out of band — see
	// fetch.New — because a harness nobody configured for network access does
	// not have network access.
	Fetch *fetch.Client
	// Map is the repo-navigation client. Nil disables the navigation tools,
	// which then report themselves unavailable rather than returning empty
	// results that read like answers.
	Map *devmap.Client
	// Grep is the repository-search client — the boundary to ripgrep's engine
	// in the analysis plane. Nil makes devcouncil_grep report that no search
	// ran, for the same reason Map's absence is reported: an empty match list
	// is an answer about the repository, and a harness that cannot search must
	// not be able to produce one.
	Grep *dcgrep.Client
	// Subsystems is the area map the write gate consults. The navigation tools
	// read the same one, so what graph_context reports about a file's
	// neighbourhood and what the gate will actually permit are one answer.
	Subsystems *repomap.Map
	// CoverageFile is a Go coverprofile or LCOV report. Empty means none was
	// supplied, which the verifier reports rather than treating as a
	// measurement of zero.
	CoverageFile string
	// VerifierBinary is the path to dcverify, the analysis plane's rigor
	// gates. When it is empty or unreachable, verification still runs its
	// scope check and names the gates that did not run — it never reports a
	// pass it did not establish.
	VerifierBinary string
	// Approver, when set, is asked before a soft block is refused, so an
	// attended run can clear it without the agent abandoning the turn.
	Approver ui.Approver
	// SubAgent runs delegated work for devcouncil_spawn_subagents and subagents.
	SubAgent SubAgentRunner

	// Extended dynamic subsystems:
	Artifacts        *artifacts.Store
	MCP              *mcp.Manager
	SubagentRegistry *agents.Registry
	SubagentMgr      *agents.InstanceManager
	QuestionAsker    QuestionAsker
}

// Registry holds the tool set and the per-agent session.
type Registry struct {
	deps     Deps
	session  *Session
	toolsReg *tools.Registry
}

// New builds the tool surface.
func New(deps Deps) (*Registry, error) {
	if deps.Store == nil {
		return nil, errors.New("devcouncil: no store")
	}
	if deps.Gate == nil {
		return nil, errors.New("devcouncil: no gate")
	}
	if deps.Root == "" {
		return nil, errors.New("devcouncil: no repository root")
	}
	if deps.LeaseTTL <= 0 {
		deps.LeaseTTL = 15 * time.Minute
	}
	return &Registry{deps: deps, session: &Session{}}, nil
}

// Gate is the gate that decides this surface's writes.
//
// Exported so an attended face can ask the same gate the agent is being judged
// by, rather than building a second one. A `check` answered by a different gate
// is a different question, and a grant recorded in a different ledger clears
// nothing the agent is about to attempt — both of which read as the harness
// disagreeing with itself for no visible reason.
func (r *Registry) Gate() *gate.Gate {
	return r.deps.Gate
}

// Session exposes the top-level agent's lease, for a caller that needs to
// release it on shutdown.
//
// It reports the Registry's own session deliberately, not whichever session a
// call was dispatched under: the caller is a face shutting the process down,
// and the only lease it is entitled to give back is the one the top-level
// agent took. A child's lease is released by the fan-out that dispatched it.
func (r *Registry) Session() SessionState { return r.session.State() }

// RootSession is the top-level agent's session, from which a dispatched
// sub-agent's own is derived.
func (r *Registry) RootSession() *Session { return r.session }

// Register installs every tool into the harness registry.
//
// Read-only tools are marked as such so a search agent can be handed the safe
// subset without a second list to keep in sync.
func (r *Registry) Register(reg *tools.Registry) error {
	r.toolsReg = reg
	for _, t := range r.Tools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// Tools returns the full native surface.
//
// Extended marks a tool the core profile may drop — `llm.local.core_tools_only`
// offers only the unmarked ones. What may be dropped is decided by one rule:
// **the core profile must be able to finish what it starts.** A tool stays core
// if a core tool's only route out of a refusal runs through it, or if a core
// tool cannot succeed without it. Everything else is genuinely peripheral.
//
// The rule is here because the first cut of the flag ignored it and produced a
// dead end rather than a smaller surface. devcouncil_verify_task was core and
// needs a lease; devcouncil_checkout_task, the only tool that takes one, was
// Extended. So the reduced set offered a tool that could never answer, and its
// refusal named the tool that had just been removed. Under harness.posture=
// strict it closed completely: every write was refused for task.absent, the
// refusal pointed at devcouncil_checkout_task, and its only stated alternative
// was devcouncil_request_override — also removed.
//
// So the task lifecycle's acquire/release pair, the tool that lists real work,
// and the override seam are core: not because they are part of editing, but
// because they are the exits from refusals the core tools produce. What stays
// Extended is what nothing else routes to — reading a task's scope, renewing a
// lease, the gap projections devcouncil_verify_task already returns inline, the
// code graph, and sub-agent dispatch. tools.Registry.UnsatisfiedIn enforces the
// half of this rule that can be stated mechanically; devcouncil's profile tests
// enforce the other half against this file's own source.
func (r *Registry) Tools() []tools.Tool {
	set := []tools.Tool{
		// --- task lifecycle ---
		{
			Schema: schema("devcouncil_next_task",
				"Find the next task that is ready to work and not already held by another agent. "+
					"Call this first when you have no task; it does not claim anything.",
				`{"type":"object","properties":{}}`),
			ReadOnly: true,
			Group:    tools.GroupTask,
			Handler:  r.nextTask,
		},
		{
			Schema: schema("devcouncil_get_task",
				"Read a task's scope: planned files, allowed commands, expected tests, and prohibitions. "+
					"Call this before writing anything, so you know what the task authorises.",
				`{"type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"]}`),
			ReadOnly: true,
			Group:    tools.GroupTask,
			Handler:  r.getTask,
			Extended: true,
		},
		{
			Schema: schema("devcouncil_checkout_task",
				"Claim a task by taking its lease. One agent holds a task at a time. "+
					"Call this before any write; writes without a lease are refused.",
				`{"type":"object","properties":{"task_id":{"type":"string"},"owner":{"type":"string"}},"required":["task_id"]}`),
			Group:   tools.GroupTask,
			Handler: r.checkout,
			// A checkout that cannot be undone is a one-way door: the task
			// stays locked against every other builder until the TTL lapses,
			// which is minutes of a queue stuck behind an agent that has
			// already finished. Whatever profile offers the acquire must offer
			// the release.
			Requires: []string{"devcouncil_release_task"},
		},
		{
			Schema: schema("devcouncil_renew_lease",
				"Extend the current lease before it expires. Renewal only works before expiry; "+
					"after that, check the task out again.",
				`{"type":"object","properties":{}}`),
			Group:    tools.GroupTask,
			Handler:  r.renew,
			Extended: true,
			Requires: []string{"devcouncil_checkout_task"},
		},
		{
			Schema: schema("devcouncil_release_task",
				"Release the current lease so another agent can take the task. "+
					"Call this when you finish, or when you are abandoning the work.",
				`{"type":"object","properties":{}}`),
			Group:    tools.GroupTask,
			Handler:  r.release,
			Requires: []string{"devcouncil_checkout_task"},
		},

		// --- guarded mutation ---
		{
			Schema: schema("devcouncil_policy_check_write",
				"Ask whether a write would be allowed, without performing it. "+
					"Call this when you are unsure whether a path is in scope — it is cheaper than a refused write.",
				`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.policyCheck,
		},
		{
			Schema: schema("devcouncil_read_file",
				"Read a file from the repository.",
				`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.readFile,
		},
		{
			Schema: schema("devcouncil_write_file",
				"Write a file. The write passes the policy gate and requires a valid lease. "+
					"A refusal tells you the rule and whether an override could clear it.",
				`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
			Group:   tools.GroupCore,
			Handler: r.writeFile,
		},
		{
			Schema: schema("devcouncil_patch_file",
				"Replace an exact target text substring with replacement text in a file. "+
					"Avoids rewriting entire large files. Passes write policy gate (dc.OpModify) and requires a valid lease.",
				`{"type":"object","properties":{"path":{"type":"string","description":"path of file to modify"},"target_content":{"type":"string","description":"exact text block to replace"},"replacement_content":{"type":"string","description":"new text to replace target_content with"},"start_line":{"type":"integer","description":"optional start line (1-indexed) to bound search"},"end_line":{"type":"integer","description":"optional end line (1-indexed) to bound search"},"allow_multiple":{"type":"boolean","description":"allow replacing multiple occurrences if found"}},"required":["path","target_content","replacement_content"]}`),
			Group:   tools.GroupCore,
			Handler: r.patchFile,
		},
		{
			Schema: schema("devcouncil_delete_file",
				"Delete a file from the repository. Passes the write policy gate (dc.OpDelete) "+
					"and requires a valid task lease.",
				`{"type":"object","properties":{"path":{"type":"string","description":"path to delete"}},"required":["path"]}`),
			Group:   tools.GroupCore,
			Handler: r.deleteFile,
		},
		{
			Schema: schema("devcouncil_exec_command",
				"Execute a shell command inside the repository. Requires an active task lease. "+
					"Commands pass the command policy gate before execution.",
				`{"type":"object","properties":{"command":{"type":"string","description":"shell command to run"}},"required":["command"]}`),
			Group:   tools.GroupCore,
			Handler: r.execCommand,
		},
		{
			Schema: schema("devcouncil_list_dir",
				"List contents of a directory in the repository. Without recursive, reports the literal "+
					"directory as it is on disk. With recursive, enumerates exactly the files "+
					"devcouncil_grep would search — ignore rules apply, and include_ignored lifts them.",
				`{"type":"object","properties":{"path":{"type":"string","description":"directory path (default: root)"},"recursive":{"type":"boolean","description":"recursively enumerate the repository under this path, honouring ignore rules"},"include_ignored":{"type":"boolean","description":"with recursive, also list files the ignore rules exclude (default: false)"}},"required":[]}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.listDir,
		},
		{
			Schema: schema("devcouncil_find_files",
				"Find files in the repository matching a glob pattern (e.g. `*.go`, `src/**/*.rs`, `*test*`). "+
					"Lists exactly the files devcouncil_grep would search: ignore rules apply by default, so "+
					".gitignore'd and hidden files are excluded unless include_ignored is set.",
				`{"type":"object","properties":{"pattern":{"type":"string","description":"glob pattern to match"},"path":{"type":"string","description":"directory path to search within (default: root)"},"max_results":{"type":"integer","description":"maximum results to return (default: 100)"},"include_ignored":{"type":"boolean","description":"also list files the ignore rules exclude, hidden files included (default: false)"}},"required":["pattern"]}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.findFiles,
		},
		{
			Schema: schema("devcouncil_grep",
				"Search for regular-expression matches across files in the repository. "+
					"Ignore rules apply by default: .gitignore, .ignore, .git/info/exclude and "+
					"hidden files are skipped, as are binary files. Set include_ignored to search "+
					"them anyway. Every reply says how many files were searched and how many were "+
					"skipped, so a result is never mistaken for full coverage.",
				`{"type":"object","properties":{"pattern":{"type":"string","description":"regular expression to search for (RE2 syntax; an invalid one is an error, never an empty result)"},"path":{"type":"string","description":"directory or file path to search within (default: root)"},"max_results":{"type":"integer","description":"maximum matches to return (default: 50)"},"include_ignored":{"type":"boolean","description":"also search files the ignore rules exclude, hidden files included (default: false)"},"case_insensitive":{"type":"boolean","description":"match without regard to case; the inline (?i) prefix also works (default: false)"}},"required":["pattern"]}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.grepSearch,
		},
		{
			Schema: schema("devcouncil_search_tools",
				"Search registered tools by capability or keyword without loading full schemas into context. "+
					"Returns names, groups, descriptions, and active state.",
				`{"type":"object","properties":{"query":{"type":"string","description":"search query (e.g. 'subagent', 'nav', 'mcp', 'artifact', 'task')"}},"required":[]}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.searchTools,
		},
		{
			Schema: schema("devcouncil_activate_tools",
				"Dynamically activate tools or entire tool groups into the model's active context. "+
					"Prerequisites declared in requires are resolved and activated automatically.",
				`{"type":"object","properties":{"tools":{"type":"array","items":{"type":"string"},"description":"names of tools or tool groups (e.g. ['subagent', 'nav', 'mcp', 'artifact', 'task'])"}},"required":["tools"]}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.activateTools,
		},
		{
			Schema: schema("devcouncil_spawn_subagents",
				"Concurrently dispatch sub-agent tasks in bounded worker pool with lease tracking and cancellation cleanup. "+
					"Give a task a type to run it as a defined role, on that role's model.",
				`{"type":"object","properties":{"tasks":{"type":"array","items":{"type":"object","properties":{"label":{"type":"string"},"prompt":{"type":"string"},"task_id":{"type":"string"},"read_only":{"type":"boolean"},"type":{"type":"string","description":"subagent role name; the role's model and system prompt are used (default: inherit this agent's)"}},"required":["label","prompt"]}}},"required":["tasks"]}`),
			Group:    tools.GroupSubagent,
			Handler:  r.spawnSubagents,
			Extended: true,
		},

		// --- the override seam ---
		{
			Schema: schema("devcouncil_request_override",
				"Ask for an exception to a soft policy block you just hit — for example a helper file "+
					"the plan did not enumerate. Copy `rule` from the block's payload, put its `target` "+
					"in `path` for a file block or in `command` for a command block, and state why in "+
					"`reason`; it is recorded and reviewed. Hard rules (secrets, paths outside the "+
					"repository, agent configs) are never grantable, and some soft rules are human-only "+
					"— a block's payload says which by reporting `agent_grantable`.",
				`{"type":"object","properties":{`+
					`"path":{"type":"string","description":"the blocked file path, for a block whose subject is path"},`+
					`"command":{"type":"string","description":"the blocked command line, for a block whose subject is command"},`+
					`"rule":{"type":"string","description":"the rule id from the block's payload, spelled exactly"},`+
					`"reason":{"type":"string"}},`+
					`"required":["rule","reason"]}`),
			Group:   tools.GroupTask,
			Handler: r.requestOverride,
			// An override is argued against a task's declared scope, so it
			// needs the lease that establishes which task that is.
			Requires: []string{"devcouncil_checkout_task"},
		},

		// --- evidence ---
		{
			Schema: schema("devcouncil_get_diff",
				"Show the working-tree changes for the current task, with the files each touches.",
				`{"type":"object","properties":{}}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.getDiff,
		},
		{
			Schema: schema("devcouncil_verify_task",
				"Run the deterministic verifier over the working tree: are the changed files inside "+
					"the task's declared scope? Returns passed, blocking gaps, and typed next actions. "+
					"Call this before claiming the work is done — evidence decides that, not confidence.",
				`{"type":"object","properties":{}}`),
			ReadOnly: true,
			Group:    tools.GroupTask,
			Handler:  r.verify,
			// Verification compares the working tree against *the task's*
			// planned files, so with no lease there is nothing to compare
			// against. That is intrinsic rather than incidental: unlike a write,
			// which the gate can judge on a nil task, a scope report with no
			// scope is not a weaker answer, it is no answer.
			Requires: []string{"devcouncil_checkout_task"},
		},
		{
			Schema: schema("devcouncil_get_gaps",
				"List the verification gaps blocking the current task.",
				`{"type":"object","properties":{}}`),
			ReadOnly: true,
			Group:    tools.GroupTask,
			Handler:  r.getGaps,
			Extended: true,
			Requires: []string{"devcouncil_checkout_task"},
		},
		{
			Schema: schema("devcouncil_get_next_actions",
				"Get the typed, machine-routable repair steps for the current task's gaps. "+
					"Each carries a category you can branch on rather than prose to parse.",
				`{"type":"object","properties":{}}`),
			ReadOnly: true,
			Group:    tools.GroupTask,
			Handler:  r.getNextActions,
			Extended: true,
			Requires: []string{"devcouncil_checkout_task"},
		},
	}
	// The repo-navigation tools, from the same index the write gate's
	// neighbour rule reads.
	set = append(set, r.navigationTools()...)
	// Subagent orchestration tools.
	set = append(set, r.subagentTools()...)
	// MCP 2.0 and Open Plugin tools.
	set = append(set, r.mcpTools()...)
	// Structured artifact management tools.
	set = append(set, r.artifactTools()...)
	// Interactive pair programming and question tools.
	set = append(set, r.askQuestionTools()...)
	// Native git integration: structured reads plus gate-arbitrated staging
	// and committing.
	set = append(set, r.gitTools()...)
	// Bridge to the external DevCouncil CLI's project-level views.
	set = append(set, r.devTools()...)
	// Documentation lookup. Contributes nothing unless an operator configured
	// a host allowlist, so an unconfigured harness offers no network tool at
	// all rather than one that always refuses.
	set = append(set, r.webTools()...)
	return set
}

func schema(name, description, input string) llm.ToolSchema {
	return llm.ToolSchema{
		Name:        name,
		Description: description,
		InputSchema: json.RawMessage(input),
	}
}

// --- helpers ---

func decode(call tools.Call, into any) error {
	if len(call.Arguments) == 0 {
		return nil
	}
	return json.Unmarshal(call.Arguments, into)
}

func ok(payload any) tools.Result {
	data, err := json.Marshal(payload)
	if err != nil {
		return tools.Errorf("encoding result: %v", err)
	}
	return tools.Result{Text: string(data)}
}

// settingOn reads a boolean setting the way every caller in this package needs
// it read: one answer, with the fallback stated at the call site.
//
// whenUnknown is what to believe when the flag registry cannot be consulted at
// all — no gate, no registry, or a key it does not define. It is a parameter
// rather than a constant because the two directions mean opposite things. For a
// capability switch (`subagents.dynamic.enabled`) and for a check that is on by
// default (`verify.rigor.enabled`), unknown means on: those flags exist so an
// operator can switch something off, and a registry that cannot answer is not
// an operator saying no — defaulting the other way would make a misconfigured
// registry indistinguishable from a deliberate lockdown, and would silence a
// gate nobody asked to silence. For a promotion to blocking
// (`verify.diff_coverage.enforce`) unknown means off, because a broken registry
// must not start failing every task; the check still runs and still reports,
// only its severity is at stake.
//
// Note what this does *not* decide: whether an off setting has to be announced.
// That is the caller's job, and for anything that suppresses a check it is not
// optional — see runRigor.
func (r *Registry) settingOn(key string, whenUnknown bool) bool {
	if r.deps.Gate == nil || r.deps.Gate.Flags == nil {
		return whenUnknown
	}
	on, _, err := r.deps.Gate.Flags.Bool(key)
	if err != nil {
		return whenUnknown
	}
	return on
}

// failure returns a payload the caller can still read, marked as an error.
//
// Kept apart from unavailable: that one is for a check that could not run, this
// one is for work that ran and did not succeed. Both must be error results, for
// the same reason — the model reads a non-error result as the thing having
// worked, and so does the no-progress detector.
func failure(payload map[string]any, reason string) tools.Result {
	// The reason goes *inside* the payload, not in front of it. Result.Text on
	// these tools is JSON the model parses; prefixing prose makes it
	// unparseable, which trades a misreported success for an unreadable one.
	payload["error"] = reason
	res := ok(payload)
	res.IsError = true
	return res
}

// unavailable is how every "could not run" answer is built. It is a distinct
// helper so the rule is applied uniformly: a failed check is an error result,
// never an empty success.
func unavailable(what string, err error) tools.Result {
	return tools.Result{
		Text:    fmt.Sprintf("%s could not be determined: %v — this is not a negative result, the check did not run", what, err),
		IsError: true,
	}
}

// releaseQuietly gives back a lease taken moments ago on a path that is about
// to refuse. The error is deliberately dropped: the caller is already returning
// a refusal that says what went wrong, and a second failure about cleanup would
// bury it. A lease that cannot be released expires on its TTL, which is the
// worst case this can produce.
func (r *Registry) releaseQuietly(ctx context.Context, taskID, token string) {
	_, _ = r.deps.Store.Release(ctx, taskID, token)
}

func held(state SessionState) error {
	if state.TaskID == "" || state.Token == "" {
		return errors.New("no task is checked out; call devcouncil_checkout_task first")
	}
	return nil
}

// authorisingTask resolves the task, if any, that a mutating operation runs
// under. It is the one owner of that question for writes, deletes and commands
// alike.
//
// It exists because those three used to answer it differently, and the
// difference was a hole. Commands did what this does: no session meant a nil
// task, which the gate turns into a soft rule the posture can demote and a
// grant can clear. Writes and deletes instead refused up front, before the gate
// ran, with a severity of "hard" that no posture reached. So under --yolo — the
// posture whose entire meaning is that the gate is not containing the agent —
// devcouncil_write_file was refused while devcouncil_exec_command would happily
// run `printf ... > file`. The gated path was shut and the ungated one was
// open, which is the wrong way round: it pushed a model off the path that
// records a decision, tracks the diff and can be verified, onto a shell that
// does none of those.
//
// The lease's *validity* is still hard, and deliberately. An expired lease is
// not an absent one: it is a lease another agent may already have taken, and
// writing under it is how two builders come to believe they own the same file.
// Posture does not soften that, because it is not a scope judgement.
//
// A nil task with a nil result means "no lease, let the gate decide".
func (r *Registry) authorisingTask(ctx context.Context, verb string) (*dc.Task, *tools.Result) {
	state := r.sessionFor(ctx).State()
	if state.TaskID == "" || state.Token == "" {
		return nil, nil
	}
	valid, err := r.deps.Store.Valid(ctx, state.TaskID, state.Token)
	if err != nil {
		res := unavailable("lease validity", err)
		return nil, &res
	}
	if !valid {
		return nil, &tools.Result{
			Text: fmt.Sprintf("lease on %s is no longer valid; check the task out again before %s",
				state.TaskID, verb),
			IsError:  true,
			Rule:     "lease.invalid",
			Severity: "hard",
		}
	}
	task, err := r.currentTask(ctx)
	if err != nil {
		res := tools.Errorf("%v", err)
		return nil, &res
	}
	return task, nil
}

func (r *Registry) searchTools(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Query string `json:"query"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if r.toolsReg == nil {
		return unavailable("tool search", errors.New("tool registry not attached"))
	}
	summaries := r.toolsReg.Search(args.Query)
	return ok(map[string]any{
		"results": summaries,
		"count":   len(summaries),
	})
}

func (r *Registry) activateTools(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Tools []string `json:"tools"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if len(args.Tools) == 0 {
		return tools.Errorf("tools array is required and must not be empty")
	}
	if r.toolsReg == nil {
		return unavailable("tool activation", errors.New("tool registry not attached"))
	}
	activated, err := r.toolsReg.Activate(args.Tools...)
	if err != nil {
		return tools.Errorf("activation failed: %v", err)
	}
	return ok(map[string]any{
		"activated":    activated,
		"active_count": len(r.toolsReg.ActiveSchemas()),
		"status":       "tools successfully loaded into active context",
	})
}
