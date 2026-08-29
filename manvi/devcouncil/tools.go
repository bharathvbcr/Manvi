// Package devcouncil is the native tool surface.
//
// These are DevCouncil's tools reimplemented in Go against the Rust state
// store, rather than shelled out to the Python CLI. That is the point of the
// port: an agent calling `devcouncil_checkout_task` reaches this code, and the
// lease it gets back is the same row `dev tasks` reads.
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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"manvi/agents"
	"manvi/artifacts"
	"manvi/dc"
	"manvi/dc/devmap"
	"manvi/dc/store"
	"manvi/fetch"
	"manvi/flags"
	"manvi/gate"
	"manvi/grants"
	"manvi/internal/fnmatch"
	"manvi/llm"
	"manvi/mcp"
	"manvi/policy"
	"manvi/repomap"
	"manvi/tools"
	"manvi/ui"
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

// SubAgentRequest is one unit of delegated work, as devcouncil_spawn_subagents
// received it from the model.
type SubAgentRequest struct {
	// Label names the work in the fan-out report.
	Label string
	// Prompt is the instruction the sub-agent is to carry out. It is the whole
	// point of the call: a dispatch that does not deliver it has not delegated
	// anything.
	Prompt string
	// TaskID, when set, is the task the child is to hold a lease on while it
	// works.
	TaskID string
	// ReadOnly asks for a child that cannot mutate — an analysis fan-out rather
	// than a build one. A runner that cannot honour it must fail rather than
	// quietly hand back a child that can write.
	ReadOnly bool
	// ModelSpec is where the child is to run, in the form Definition.Model
	// uses: "" or "inherit" for the parent's provider and model,
	// "provider/model", a bare provider, or a bare model. It is carried
	// unparsed because resolving it needs the provider registry, which lives
	// with the runner and not in this package — see agents.ParsePlacement.
	//
	// This field is what makes a role's Model mean anything. Before it existed
	// the dispatch read a definition, resolved its role and its write
	// permission, and dropped the rest on the floor: every child ran on the
	// parent's provider whatever its role said, so a planner on a frontier
	// model dispatching workers to a local one was not expressible.
	ModelSpec string
	// SystemPrompt is the role's own prompt. Empty means the child inherits the
	// parent's, which is what the "self" role wants and what a fan-out with no
	// named type gets.
	//
	// A role is its instructions. Carrying Model without this would let an
	// operator place a critic on a cheaper model while the child still ran with
	// the parent's builder prompt — the bill would move and the behaviour would
	// not.
	SystemPrompt string
	// Surface is the tool set the role declared: whether it admits the MCP
	// group, and any allowlist it named. A zero value means no role was named
	// and the child inherits the parent's surface, narrowed only by ReadOnly.
	//
	// Like ReadOnly, a runner that cannot honour it must fail rather than hand
	// back a child with a wider surface than the role asked for. This field is
	// what makes agents.Definition's tool permissions mean anything: before it
	// existed they were decoded into a struct nothing downstream ever read, so
	// a role declaring `"enable_mcp_tools": false` produced a child holding the
	// MCP tools, and the operator had a written permission that did nothing.
	Surface agents.ToolSurface
	// Leases is told about every lease this child takes for itself, so the
	// fan-out that dispatched it can give those back when the child cannot.
	//
	// It is the fix for the one hole in the cleanup this harness already had.
	// agents.Holder was only ever told about a lease the *dispatcher* named in
	// tasks[].task_id; a child that called devcouncil_checkout_task — the tool
	// it is given, and the only way it ever takes a lease — registered nothing.
	// Measured: an eight-way fan-out of children that each checked out a task
	// left seven leases held for the full TTL after the run exited cleanly,
	// and the fan-out reported "clean": true with no orphans.
	Leases LeaseSink
	// Verdict asks the child for a structured judgement in addition to its
	// prose, and is set only by a dispatcher that is going to act on one.
	//
	// It carries the marker the child is told to emit. The runner reads that
	// line out of the answer against a stated contract rather than guessing at
	// the prose, and a child that never emits it comes back having reached no
	// judgement — which is not a pass. Empty means the caller wants no
	// judgement, and none is parsed.
	Verdict string
}

// SubAgentResult is what one sub-agent actually produced.
type SubAgentResult struct {
	// Summary is the child's own account of its work, and it is what the
	// dispatching agent reads. An empty one is treated as a failure by the
	// dispatch: a child that finished with nothing to say did not do the work
	// its prompt described.
	Summary string
	// Steps is how many steps the child spent, for the run report.
	Steps int
	// Usage is what the child's own turn cost.
	//
	// It is carried because it was being discarded. A fan-out's children do
	// the bulk of the work and therefore the bulk of the spend, and the run's
	// reported usage counted only the dispatching agent: measured on an
	// eight-way fan-out, 2,200 of 38,200 input tokens — 5.8% of the real cost
	// — reported as the whole of it. A benchmark reading that number is being
	// told the harness is seventeen times cheaper than it is.
	Usage SubAgentUsage
	// Wrote names the repository-relative paths the child changed.
	//
	// A child runs on its own bus and its own log, so nothing it does reaches
	// the parent's terminal checkpoint by itself. Without this a fan-out of
	// builders leaves the parent with an empty written-path set and a
	// dispatch tool that merely counts as a mutation, so the parent's
	// end-of-turn check runs against nothing and reports a pass for work it
	// never looked at. The paths come back so the parent can verify what its
	// children did.
	Wrote []string
	// WroteTruncated is true when the child changed more paths than Wrote
	// lists, so the incompleteness survives the handoff rather than being
	// re-derived — or, worse, assumed away.
	WroteTruncated bool
	// Verdict is a structured judgement, set only by children dispatched to
	// judge something. It is empty for ordinary work.
	//
	// It exists because "completed" does not mean "passed". A child's status
	// is set from whether its summary was non-empty, so a critic that ran,
	// found three defects and said so in prose is reported exactly like one
	// that found none. Scraping the summary for a word like PASSED is not a
	// contract — it is a hope about prose — and an advance rule built on it
	// certifies whatever the model happened to write.
	Verdict SubAgentVerdict
}

// SubAgentVerdict is a judging child's structured answer.
//
// Passed is deliberately not the zero value. A verdict that was never set, a
// child that died, and a child that ran and found nothing wrong must not
// serialise to the same thing: the first two are the absence of a judgement and
// the third is a judgement, and an advance rule that cannot tell them apart
// advances on silence.
type SubAgentVerdict struct {
	// Judged is false unless a judgement was actually reached. Nothing may read
	// Passed without it.
	Judged bool `json:"judged"`
	// Passed is the judgement itself, meaningful only when Judged is true.
	Passed bool `json:"passed"`
	// Findings are what the judge objected to, already capped by its producer.
	// A verdict that failed with no findings is still a failure — the reason
	// may simply not have survived — but a verdict that passed while carrying
	// findings is a contradiction, and Reconcile refuses it.
	Findings []string `json:"findings,omitempty"`
}

// Reconcile folds a claimed verdict into a safe one, failing closed.
//
// Three ways a judgement can be untrustworthy, and all three resolve to the
// same answer: not passed. A judgement that was never reached is not a pass. A
// judgement that says it passed while listing objections is not internally
// consistent, and the direction that is safe to be wrong in is the one that
// asks a human. Being generous here is how "approved" comes to mean
// "unexamined".
func (v SubAgentVerdict) Reconcile() SubAgentVerdict {
	if !v.Judged {
		return SubAgentVerdict{Judged: false}
	}
	if v.Passed && len(v.Findings) > 0 {
		v.Passed = false
	}
	return v
}

// SubAgentUsage is one child's token cost.
//
// Declared here in plain integers rather than as llm.Usage so this package,
// which describes what a delegated turn owes its caller, does not take on the
// provider layer as a dependency.
type SubAgentUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	CacheReadTokens int `json:"cache_read_tokens,omitempty"`
}

// Add accumulates another child's cost.
func (u *SubAgentUsage) Add(o SubAgentUsage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.CacheReadTokens += o.CacheReadTokens
}

// Any reports whether anything was spent, so a fan-out that learned nothing
// about its children's cost says nothing rather than reporting zero.
func (u SubAgentUsage) Any() bool {
	return u.InputTokens > 0 || u.OutputTokens > 0 || u.ReasoningTokens > 0 || u.CacheReadTokens > 0
}

// SubAgentRunner runs one sub-agent turn to completion.
//
// It is an interface, and Deps.SubAgent is allowed to be nil, because running a
// turn needs a provider, a model, a system prompt and a session log — none of
// which this package has or should acquire for itself. The seam lets whoever
// already builds those attach the capability; until something does, the tool
// says so instead of pretending.
type SubAgentRunner interface {
	RunSubAgent(ctx context.Context, req SubAgentRequest) (SubAgentResult, error)
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
				"List contents of a directory in the repository.",
				`{"type":"object","properties":{"path":{"type":"string","description":"directory path (default: root)"},"recursive":{"type":"boolean","description":"recursively list subdirectories"}},"required":[]}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.listDir,
		},
		{
			Schema: schema("devcouncil_find_files",
				"Find files in the repository matching a glob pattern (e.g. `*.go`, `src/**/*.rs`, `*test*`).",
				`{"type":"object","properties":{"pattern":{"type":"string","description":"glob pattern to match"},"path":{"type":"string","description":"directory path to search within (default: root)"},"max_results":{"type":"integer","description":"maximum results to return (default: 100)"}},"required":["pattern"]}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.findFiles,
		},
		{
			Schema: schema("devcouncil_grep",
				"Search for pattern matches across files in the repository.",
				`{"type":"object","properties":{"pattern":{"type":"string","description":"pattern or string to search for"},"path":{"type":"string","description":"directory or file path (default: root)"},"max_results":{"type":"integer","description":"maximum matches to return (default: 50)"}},"required":["pattern"]}`),
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

// resolvePath turns a tool argument into a filesystem path, and decides whether
// an escaping one may be used.
//
// It owns that decision rather than reading the gate's verdict: a tool that
// touches the filesystem must not depend on another component having run. What
// it does share with the gate is the setting, so an operator has one switch to
// reason about instead of two that can disagree — and the containment it used
// to enforce unconditionally is now exactly as strong as the hard rules are.
//
// With the hard rules on, which is every posture but yolo, nothing resolves
// outside the repository root. With them off, an escaping path resolves to what
// it names and the write lands there. That is a deliberate retirement of an
// invariant, requested explicitly: yolo means the harness is not containing the
// agent, and a containment that survived it would have made the posture a
// half-truth. Two refusals survive regardless, because neither is containment:
// a path whose text and whose meaning to the kernel differ (NUL, control
// characters) and the repository root itself, both of which normalize to no
// usable target at all.
func (r *Registry) resolvePath(path string) (string, error) {
	if rel, ok := r.containedRel(path); ok {
		return filepath.Join(r.deps.Root, filepath.FromSlash(rel)), nil
	}

	normalized, _ := policy.NormalizeRepoPath(r.deps.Root, path)
	hardRules, _, err := flags.EffectiveHardRules(r.deps.Gate.Flags)
	if err != nil {
		// An unreadable setting is not permission. The containment stands.
		return "", fmt.Errorf("path %q is outside the repository, and the hard-rules setting could not be read: %w", path, err)
	}
	if hardRules {
		return "", fmt.Errorf("path %q is outside the repository", path)
	}
	// NormalizeRepoPath reports an escaping path as its resolved absolute form.
	// Anything else it calls uncontained — a malformed path, or the root itself
	// — carries no target to write to, and is refused whatever the setting says.
	target := filepath.FromSlash(normalized)
	if !filepath.IsAbs(target) {
		return "", fmt.Errorf("path %q does not name a usable location", path)
	}
	return target, nil
}

// containedRel answers the one question every path-taking tool has to ask
// before it touches the filesystem: which repository-relative path does this
// string name, and does it name one at all?
//
// It is the single owner of that answer. resolvePath is built on it, and so is
// readContained — which is the point: read_file refused a symlink pointing out
// of the repository while grep read the very same link through os.ReadFile and
// reported the contents under the in-repo name, because the two had separate
// opinions about containment and only one of them was enforcing.
//
// NormalizeRepoPath resolves symlinks before judging, so a link whose target
// escapes is reported uncontained here rather than followed later.
func (r *Registry) containedRel(p string) (string, bool) {
	normalized, outside := policy.NormalizeRepoPath(r.deps.Root, p)
	if outside {
		return "", false
	}
	// If the file exists directly under root, use it. If it does not exist
	// under root, but cwd is inside root and the file exists at cwd, prefer cwd.
	if !filepath.IsAbs(p) {
		if _, err := os.Stat(filepath.Join(r.deps.Root, normalized)); os.IsNotExist(err) {
			if cwd, err := os.Getwd(); err == nil {
				candidate := filepath.Join(cwd, p)
				if normCwd, outCwd := policy.NormalizeRepoPath(r.deps.Root, candidate); !outCwd {
					if _, statErr := os.Stat(candidate); statErr == nil {
						return filepath.ToSlash(normCwd), true
					}
				}
			}
		}
	}
	return filepath.ToSlash(normalized), true
}

// containedRelOf is containedRel's form for a path the tool already holds as an
// absolute location — a walk hit, say — where no normalization is wanted, only
// the containment verdict and the relative name.
func containedRelOf(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	// "." is the root itself: a legitimate search root, and never a file the
	// walk can hand to the reader — pinWriteTarget refuses it, so the read side
	// fails closed even if one ever arrived.
	return rel, true
}

// maxToolReadBytes bounds every file the ungated read tools will pull into
// memory. One number, because read_file and grep face the same repository and
// two different ceilings would only mean one of them was wrong.
const maxToolReadBytes = 2 * 1024 * 1024

// readContained is the canonical reader for the ungated read tools.
//
// It delegates to the pinned reader in safefs.go, which is the only code here
// that gets containment right: it opens with O_NOFOLLOW so a symlink cannot
// redirect the read, checks the opened descriptor is a regular file so a FIFO
// or a device node cannot block forever or stream without end, re-verifies
// every directory identity captured at pin time, and measures the size with
// fstat on the descriptor rather than an lstat on the name — an lstat measures
// the link, so a 182-byte symlink to a 5 MiB file passed a 2 MiB guard.
//
// The read runs on its own goroutine so ctx actually bounds it. It used to be
// accepted and ignored; a FIFO planted in the repository then held the tool
// open seconds past its own deadline, wedging the turn. The channel is
// buffered, so the goroutine finishes and exits even when nobody is left to
// receive.
func readContained(ctx context.Context, root, rel string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", rel, err)
	}
	type outcome struct {
		data []byte
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		data, err := ReadPinned(root, rel, limit)
		done <- outcome{data: data, err: err}
	}()
	select {
	case res := <-done:
		return res.data, res.err
	case <-ctx.Done():
		return nil, fmt.Errorf("reading %s: %w", rel, ctx.Err())
	}
}

func (r *Registry) resolveDirPath(path string) (string, error) {
	if path == "" || path == "." || path == "./" {
		return r.deps.Root, nil
	}
	return r.resolvePath(path)
}

// --- task lifecycle ---

func (r *Registry) nextTask(ctx context.Context, call tools.Call) tools.Result {
	ids, err := r.deps.Store.ReadyTasks(ctx)
	if err != nil {
		return unavailable("ready tasks", err)
	}
	if len(ids) == 0 {
		return ok(map[string]any{
			"task_id": nil,
			"message": "no unheld ready tasks",
		})
	}
	task, err := r.deps.Store.Task(ctx, ids[0])
	if err != nil {
		return unavailable("task scope", err)
	}
	return ok(map[string]any{"task_id": ids[0], "task": task, "ready_count": len(ids)})
}

func (r *Registry) getTask(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		TaskID string `json:"task_id"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	task, err := r.deps.Store.Task(ctx, args.TaskID)
	if err != nil {
		return unavailable("task", err)
	}
	if task == nil {
		return tools.Errorf("no task %q", args.TaskID)
	}
	return ok(task)
}

func (r *Registry) checkout(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		TaskID string `json:"task_id"`
		Owner  string `json:"owner"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	sess := r.sessionFor(ctx)
	owner := args.Owner
	if owner == "" {
		owner = sess.State().Owner
	}
	if owner == "" {
		owner = "manvi"
	}

	lease, err := r.deps.Store.Acquire(ctx, store.AcquireRequest{
		TaskID: args.TaskID, Owner: owner, TTL: r.deps.LeaseTTL,
	})
	var conflict *store.Conflict
	if errors.As(err, &conflict) {
		// Contention is routable, not fatal: the agent takes another task.
		return ok(map[string]any{
			"acquired":         false,
			"code":             string(conflict.Code()),
			"holder":           conflict.Holder,
			"suggested_action": "pick_other_task",
			"suggested_tool":   "devcouncil_next_task",
		})
	}
	if err != nil {
		return unavailable("lease", err)
	}

	task, err := r.deps.Store.Task(ctx, lease.TaskID)
	if err != nil {
		// The lease is already taken at this point, and a checkout that cannot
		// report its scope must not leave one behind holding the task against
		// every other builder until the TTL lapses.
		r.releaseQuietly(ctx, lease.TaskID, lease.Token)
		return unavailable("task scope after checkout", err)
	}
	if task == nil {
		// The store leases any id, including one no task was ever planned
		// under, because a lease is a mutex and a mutex does not ask what it is
		// protecting. That is right for `manvi lease acquire`, which a human
		// runs deliberately. It is wrong here, and the wrongness was not
		// theoretical: a model asked to edit one file invented a task id from
		// the prompt, checked it out successfully, was refused by every write
		// with "held but has no record", released, invented it again, and spent
		// its entire step budget in that loop while its context grew to 77k
		// tokens. The checkout said yes to something nothing else would honour.
		//
		// So it fails closed and gives back what it took, and the refusal names
		// the tool that lists real work.
		r.releaseQuietly(ctx, lease.TaskID, lease.Token)
		return ok(map[string]any{
			"acquired":         false,
			"task_id":          lease.TaskID,
			"reason":           fmt.Sprintf("no task %q exists; a task id cannot be invented", lease.TaskID),
			"suggested_action": "list_real_tasks",
			"suggested_tool":   "devcouncil_next_task",
			"note": "if this work belongs to no planned task, do not check one out — " +
				"work directly, and the policy gate will decide each write on its own",
		})
	}

	// Recorded through the session rather than on it, so the lease is handed
	// to whatever is watching this agent's holdings before the agent gets to
	// do anything under it. A fan-out's cleanup can only give back a lease it
	// was told about, and a child that took one and then died is exactly the
	// case that has nobody left to tell it.
	sess.adopt(lease.TaskID, lease.Token, owner)

	// The token is deliberately absent from the payload: it is a credential,
	// and the model does not need it to work. The harness holds it.
	return ok(map[string]any{
		"acquired":   true,
		"task_id":    lease.TaskID,
		"expires_at": lease.ExpiresAt,
		"task":       task,
	})
}

func (r *Registry) renew(ctx context.Context, call tools.Call) tools.Result {
	sess := r.sessionFor(ctx)
	state := sess.State()
	if err := held(state); err != nil {
		return tools.Errorf("%v", err)
	}
	if refusal := refuseInherited(state, "renew"); refusal != nil {
		return *refusal
	}
	lease, err := r.deps.Store.Renew(ctx, state.TaskID, state.Token, r.deps.LeaseTTL)
	if err != nil {
		return unavailable("lease renewal", err)
	}
	if lease == nil {
		return ok(map[string]any{
			"renewed":          false,
			"code":             string(dc.LeaseExpired),
			"suggested_action": "checkout_again",
			"suggested_tool":   "devcouncil_checkout_task",
		})
	}
	return ok(map[string]any{"renewed": true, "expires_at": lease.ExpiresAt})
}

func (r *Registry) release(ctx context.Context, call tools.Call) tools.Result {
	sess := r.sessionFor(ctx)
	state := sess.State()
	if err := held(state); err != nil {
		return tools.Errorf("%v", err)
	}
	if refusal := refuseInherited(state, "release"); refusal != nil {
		return *refusal
	}
	released, err := r.deps.Store.Release(ctx, state.TaskID, state.Token)
	if err != nil {
		return unavailable("lease release", err)
	}
	sess.clear()
	return ok(map[string]any{"released": released})
}

// refuseInherited stops a dispatched sub-agent giving away or extending the
// lease its parent took.
//
// A child inherits the parent's task so its writes are judged against the
// parent's scope, which means it also inherits a token the store will honour.
// Acting on that token is not a scope question the posture can soften: the
// parent's own record would go on saying it holds a task the store has already
// released, and the next sibling's write would be judged against a lease
// nobody holds.
func refuseInherited(state SessionState, verb string) *tools.Result {
	if !state.Inherited {
		return nil
	}
	res := tools.Errorf(
		"this sub-agent did not check %s out — it inherited that lease from the agent that dispatched it, "+
			"and cannot %s a lease it does not own. Report what you found and let the dispatching agent "+
			"decide; or check out a task of your own if this work belongs to a different one",
		state.TaskID, verb)
	res.Rule = "lease.not_owned"
	res.Severity = "hard"
	return &res
}

// --- guarded mutation ---

// currentTask reads the scope the gate evaluates against. A checked-out task
// whose scope cannot be read is an error rather than an empty scope: an empty
// scope would deny every write and read as a policy decision.
func (r *Registry) currentTask(ctx context.Context) (*dc.Task, error) {
	state := r.sessionFor(ctx).State()
	if err := held(state); err != nil {
		return nil, err
	}
	task, err := r.deps.Store.Task(ctx, state.TaskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %s is held but has no record", state.TaskID)
	}
	return task.Domain(), nil
}

func (r *Registry) policyCheck(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path string `json:"path"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	// The same resolution devcouncil_write_file performs, through the same
	// owner, and deliberately not a stricter one.
	//
	// This tool exists to preview that write, so any state in which the two
	// disagree is a state in which the cheap call lies about the expensive one.
	// It used to call currentTask, which refuses outright when no lease is
	// held, while the write called authorisingTask, which hands the gate a nil
	// task and lets the posture decide. Under the shipped dev posture and under
	// yolo the write therefore succeeded while the preview reported a hard stop
	// for the same path — and under core_tools_only the preview's refusal named
	// devcouncil_checkout_task, a tool that profile had removed. An agent that
	// checked before writing was told the work was impossible; one that skipped
	// the check wrote the file.
	task, refusal := r.authorisingTask(ctx, "checking a write")
	if refusal != nil {
		return *refusal
	}
	decision, err := r.deps.Gate.EvaluateWrite(args.Path, task, dc.OpWrite)
	if err != nil {
		return unavailable("policy decision", err)
	}
	return ok(r.decisionPayload(decision))
}

// decisionPayload renders a decision so an agent can branch without reading
// prose — including, when the block is overridable, the exact call that would
// clear it.
//
// It is a method rather than a free function because naming a recovery needs two
// things the decision does not carry: whether the rule's Target is a path or a
// command, and whether this gate's ledger would let an agent clear that rule.
// Built from the decision alone the advice was wrong in both directions at once
// — it handed a command string to an argument called "path", and it named an
// agent-issued override for rules no agent may issue one for.
func (r *Registry) decisionPayload(d policy.Decision) map[string]any {
	payload := map[string]any{
		"allowed":  !d.Blocked(),
		"action":   string(d.Action),
		"rule":     string(d.Rule),
		"severity": string(d.Severity),
		"reason":   d.Reason,
		"target":   d.Target,
		// What Target names. A consumer that has to infer this is the consumer
		// that handed a command line to a file-write evaluator.
		"subject": string(policy.SubjectOf(d.Rule)),
	}
	if len(d.Degraded) > 0 {
		payload["degraded"] = d.Degraded
	}
	if d.GrantID != "" {
		payload["granted_by"] = d.GrantedBy
		payload["grant_id"] = d.GrantID
		// A granted allow is not a clean pass, and the payload says so.
		payload["clean"] = false
	}
	if d.Demoted != "" {
		payload["demoted"] = d.Demoted
		payload["clean"] = false
	}
	if d.Widened != "" {
		// Named on every decision it authorises, not only on the one that
		// created it. An agent reading this should be able to tell a write the
		// plan authorised from one its own earlier argument did.
		payload["widened"] = d.Widened
		payload["clean"] = false
	}
	if d.Blocked() {
		if !d.Overridable() {
			payload["overridable"] = false
			payload["agent_grantable"] = false
			payload["note"] = "hard rule: no override clears this, by any authority"
			return payload
		}

		// Overridable says a grant of *some* authority could clear this.
		// Whether this agent is that authority is a separate question, and
		// conflating the two is what produced advice an agent could not act on.
		agentMay := r.deps.Gate.AgentCanGrant(d.Rule)
		payload["overridable"] = true
		payload["agent_grantable"] = agentMay

		// Which recovery to name depends on what is actually missing. An agent
		// that holds a task and strayed outside its planned scope should argue
		// for the exception; an agent holding no task at all should take one,
		// and telling it to request an override instead sends it to negotiate
		// about work it has not claimed. Every case here is a soft block, so
		// severity alone cannot distinguish them — the rule can.
		switch {
		case d.Rule == policy.RuleNoTask:
			payload["suggested_tool"] = "devcouncil_checkout_task"
			payload["suggested_arguments"] = map[string]string{
				"task_id": "the task this work belongs to; " +
					"devcouncil_next_task lists what is ready",
			}
			payload["note"] = "no task is checked out; checkout first, " +
				"or request an override if this write belongs to no task"

		case d.Rule == policy.RuleCommandNoLease:
			// The command-gate twin of task.absent, and it gets the same
			// answer. It used to reach the override branch, where the command
			// line was passed as a path to a tool that first demands the very
			// lease this rule says is missing.
			payload["suggested_tool"] = "devcouncil_checkout_task"
			payload["suggested_arguments"] = map[string]string{
				"task_id": "the task this work belongs to; " +
					"devcouncil_next_task lists what is ready",
			}
			payload["note"] = "shell commands need a lease; check out the task first. " +
				"Reading the repository does not: " + ungatedToolSentence

		case policy.IsCommandRule(d.Rule) && agentMay:
			// Reachable only when an operator set grants.agent.allow_commands.
			// The subject is named `command`, because that is what it is.
			payload["suggested_tool"] = "devcouncil_request_override"
			payload["suggested_arguments"] = map[string]string{
				"command": d.Target, "rule": string(d.Rule),
				"reason": "explain why this command is necessary",
			}

		case policy.IsCommandRule(d.Rule):
			// No agent-issued grant clears this, so no agent tool is named. The
			// previous code named one anyway: the agent called it, was told
			// "granted" for a file-scope rule it had not hit, had the command
			// line appended to its task's planned files, and found the command
			// still blocked. Naming nothing beats naming a dead end — and the
			// alternatives below are not a consolation prize, they are how
			// exploration is supposed to work here.
			payload["suggested_action"] = "no agent-issued grant clears a command block. " +
				"Use an ungated read tool, add the command to the task's allowed_commands, " +
				"or ask a human to grant it."
			payload["ungated_alternatives"] = ungatedReadTools
			payload["note"] = ungatedToolSentence +
				" An operator can enable agent-issued command grants with grants.agent.allow_commands."

		default:
			payload["suggested_tool"] = "devcouncil_request_override"
			payload["suggested_arguments"] = map[string]string{
				"path": d.Target, "rule": string(d.Rule),
				"reason": "explain why this write is necessary",
			}
		}
	}
	return payload
}

// ungatedReadTools are the tools that reach the repository without consulting
// task scope at all. They are listed on a command refusal because a blocked
// command is usually an attempt to look at something, and looking has never
// needed permission here — only writing and executing do.
var ungatedReadTools = []string{
	"devcouncil_read_file", "devcouncil_grep",
	"devcouncil_list_dir", "devcouncil_find_files",
}

const ungatedToolSentence = "devcouncil_read_file, devcouncil_grep, " +
	"devcouncil_list_dir and devcouncil_find_files are not gated by task scope."

func (r *Registry) readFile(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path string `json:"path"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	rel, contained := r.containedRel(args.Path)
	if !contained {
		return tools.Errorf("path %q is outside the repository", args.Path)
	}
	data, err := readContained(ctx, r.deps.Root, rel, maxToolReadBytes)
	if err != nil {
		return tools.Errorf("reading %s: %v", args.Path, err)
	}
	return tools.Result{Text: string(data)}
}

func (r *Registry) writeFile(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}

	// The lease is checked against the store on every write, not once at
	// checkout. A lease can expire mid-turn, and a write authorised by a lease
	// that has since lapsed is a write two agents could both believe they own.
	task, refusal := r.authorisingTask(ctx, "writing")
	if refusal != nil {
		return *refusal
	}
	// Asked before the write, because afterwards every path exists. See
	// reuse.go: this is what turns "extend existing seams rather than
	// duplicating" from a line in the system prompt into something that
	// actually looks.
	reusePath, creating := r.createdPath(args.Path)
	// The target is pinned BEFORE the ladder runs and before any approval
	// prompt: the identity snapshot has to predate human-scale delays, or a
	// concurrent actor could swap a directory for a symlink while the prompt
	// sat open and the kernel would follow it at open time. See safefs.go.
	normalized, outside := policy.NormalizeRepoPath(r.deps.Root, args.Path)
	var pinned *pinnedTarget
	if !outside {
		var err error
		pinned, err = pinWriteTarget(r.deps.Root, normalized)
		if err != nil {
			return tools.Errorf("%v", err)
		}
	}
	decision, err := r.deps.Gate.EvaluateWrite(args.Path, task, dc.OpWrite)
	if err != nil {
		return unavailable("policy decision", err)
	}
	if decision.Blocked() {
		escalated, ok := r.escalate(ctx, decision, decision.Target)
		if !ok {
			return r.refusal(decision)
		}
		decision = escalated
	}

	full, err := r.resolvePath(args.Path)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if pinned != nil {
		if err := pinned.Write([]byte(args.Content), 0o644); err != nil {
			return tools.Errorf("writing %s: %v", args.Path, err)
		}
	} else {
		// Outside-root targets are reachable only with hard rules off — an
		// explicit operator decision. The legacy path stands there.
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return tools.Errorf("creating directory for %s: %v", args.Path, err)
		}
		if err := os.WriteFile(full, []byte(args.Content), 0o644); err != nil {
			return tools.Errorf("writing %s: %v", args.Path, err)
		}
	}

	// The path is claimed only here, after the bytes have landed. A refusal, a
	// failed open or a pin that could not be taken all return above without it,
	// because none of them changed a file and a checker handed a path that was
	// never written would report on the wrong thing — or, worse, report a pass
	// for a write the gate refused.
	result := annotate(
		tools.Result{
			Text:  fmt.Sprintf("wrote %s (%d bytes)", decision.Target, len(args.Content)),
			Wrote: []string{decision.Target},
		},
		decision)
	if creating {
		result = annotateReuse(result, r.checkReuse(ctx, reusePath))
	}
	return result
}

func (r *Registry) deleteFile(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path string `json:"path"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return tools.Errorf("path is required")
	}

	task, refusal := r.authorisingTask(ctx, "deleting")
	if refusal != nil {
		return *refusal
	}
	decision, err := r.deps.Gate.EvaluateWrite(args.Path, task, dc.OpDelete)
	if err != nil {
		return unavailable("policy decision", err)
	}
	if decision.Blocked() {
		escalated, ok := r.escalate(ctx, decision, decision.Target)
		if !ok {
			return r.refusal(decision)
		}
		decision = escalated
	}

	// Delete is pinned like write: a directory swapped to an escaping symlink
	// between evaluation and unlink would otherwise delete wherever the link
	// points. The pin happens after escalation here — unlink has no
	// deliberation payload — so the residual race is only the microsecond
	// between pin and syscall, not the whole approval window.
	normalizedDelete, outside := policy.NormalizeRepoPath(r.deps.Root, args.Path)
	if !outside {
		if err := RemovePinned(r.deps.Root, normalizedDelete); err != nil {
			return tools.Errorf("%v", err)
		}
		return annotate(
			tools.Result{
				Text:  fmt.Sprintf("deleted %s", decision.Target),
				Wrote: []string{decision.Target},
			},
			decision)
	}

	full, err := r.resolvePath(args.Path)
	if err != nil {
		return tools.Errorf("%v", err)
	}

	if _, statErr := os.Stat(full); os.IsNotExist(statErr) {
		return tools.Errorf("file %s does not exist", args.Path)
	}

	if err := os.Remove(full); err != nil {
		return tools.Errorf("deleting %s: %v", args.Path, err)
	}

	return annotate(
		tools.Result{
			Text:  fmt.Sprintf("deleted %s", decision.Target),
			Wrote: []string{decision.Target},
		},
		decision)
}

func (r *Registry) execCommand(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Command string `json:"command"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Command) == "" {
		return tools.Errorf("command is required")
	}

	task, refusal := r.authorisingTask(ctx, "running commands")
	if refusal != nil {
		return *refusal
	}

	decision, err := r.deps.Gate.EvaluateCommand(args.Command, task)
	if err != nil {
		return unavailable("command policy decision", err)
	}

	if decision.Blocked() {
		escalated, ok := r.escalate(ctx, decision, args.Command)
		if !ok {
			return r.refusal(decision)
		}
		decision = escalated

		// Clearing the command is not clearing the write.
		//
		// The gate runs its redirect rung only on a command it is about to
		// permit, so a command denied when EvaluateCommand ran had its
		// redirect targets skipped -- and an escalation issues the grant
		// afterwards. That left the write half judged by nobody: a human
		// cleared `cat seed.txt > unplanned.txt` and the write landed on a
		// path a direct write_file would have refused as scope.unplanned. The
		// human was asked once, about the command.
		//
		// Only the redirect rung is re-run, not the whole ladder. Re-running
		// EvaluateCommand would re-litigate the command half against its
		// normalized target, which the just-issued grant deliberately does not
		// match -- the grant is keyed to the raw command the human was shown.
		taskID := ""
		if task != nil {
			taskID = task.ID
		}
		refused, err := gate.EvaluateRedirects(args.Command, taskID,
			func(target string) (policy.Decision, error) {
				return r.deps.Gate.EvaluateWrite(target, task, dc.OpWrite)
			})
		if err != nil {
			return unavailable("redirect target policy decision", err)
		}
		if refused.Refused {
			return r.refusal(refused.Decision)
		}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	start := time.Now()
	outText, exitCode, timedOut, runErr := runShell(cmdCtx, r.deps.Root, args.Command)
	elapsed := time.Since(start)

	if timedOut {
		return annotate(tools.Result{
			Text:    "command timed out after 5m:\n" + outText,
			IsError: true,
		}, decision)
	}
	if runErr != nil {
		return annotate(tools.Result{
			Text:    fmt.Sprintf("failed to execute command %q: %v", args.Command, runErr),
			IsError: true,
		}, decision)
	}

	resultText := fmt.Sprintf("exit code %d (took %s)\n%s", exitCode, elapsed.Round(time.Millisecond), outText)
	return annotate(tools.Result{
		Text:    strings.TrimRight(resultText, "\n"),
		IsError: exitCode != 0,
	}, decision)
}

// runShell executes command under `sh -c` in dir, bounded by the deadline on
// ctx, and returns the captured output, the exit code, and whether the
// deadline fired.
//
// Two properties make this safe for agent-authored commands. First, the child
// runs as its own process-group leader and the whole group is killed when the
// deadline fires — CommandContext reaches only sh itself, and a descendant it
// backgrounded would otherwise keep running with the inherited stdout pipe.
// Second, WaitDelay bounds how long those descendants may hold the stdio pipes
// after sh is gone; without it, os/exec's copy goroutine waits on EOF forever
// and this function never returns, wedging the tool call past its own timeout.
// A command whose foreground work completed reports success even when its
// leftover descendants were cut off that way.
//
// Third, the group is reaped on every exit path, not only on the deadline. The
// kill used to hang off ctx.Done() alone, so a command that exited zero closed
// the watcher and left its descendants running: `sleep 30 &` outlived the tool
// call that started it, unattached to anything that would ever clean it up. A
// process this function started must not survive the call, whichever way the
// call ended.
//
// Fourth, the child gets a built environment rather than the operator's. It
// used to inherit everything, so an agent-authored command could print the
// operator's API keys straight back into the transcript, and an interpreter
// preload variable in the operator's shell would apply to whatever the agent
// chose to run. See sanitizedEnv.
func runShell(ctx context.Context, dir, command string) (string, int, bool, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = sanitizedEnv(dir)
	setOwnProcessGroup(cmd)

	var buf bytes.Buffer
	capture := &limitWriter{w: &buf, limit: 1024 * 1024}
	cmd.Stdout = capture
	cmd.Stderr = capture

	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		return "", 0, false, err
	}
	pgid := cmd.Process.Pid
	// Reaped on every path out of this function: deadline, clean exit, and
	// non-zero exit alike.
	defer killProcessGroup(pgid)

	killed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// The context has already signalled the child; reach the rest of
			// its process group so a held pipe cannot outlive the deadline.
			killProcessGroup(pgid)
		case <-killed:
		}
	}()

	runErr := cmd.Wait()
	close(killed)

	output := func() string {
		if note := capture.truncationNote(); note != "" {
			return strings.TrimRight(buf.String(), "\n") + "\n" + note + "\n"
		}
		return buf.String()
	}

	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	if timedOut {
		return output(), 0, true, nil
	}
	exitCode := 0
	if runErr != nil {
		if errors.Is(runErr, exec.ErrWaitDelay) {
			// The command itself exited; only leftover descendants kept the
			// pipes open past the grace period. Report the command's own
			// outcome rather than dressing plumbing cleanup up as a failure.
			runErr = nil
			if cmd.ProcessState != nil {
				exitCode = cmd.ProcessState.ExitCode()
			}
		} else {
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) {
				return output(), 0, false, runErr
			}
			exitCode = exitErr.ExitCode()
		}
	}
	return output(), exitCode, false, nil
}

// sanitizedEnv builds the environment an agent-authored command runs under.
//
// An allowlist, never a denylist: the operator's environment is not an
// enumerable set, and every new secret exported into a shell would otherwise
// arrive here as a default-allow. The child used to inherit all of it, so
// `env` printed the operator's provider keys straight back into the model's
// transcript, and an interpreter preload variable — LD_PRELOAD on Linux,
// DYLD_INSERT_LIBRARIES where the platform does not strip it — applied to
// whatever the agent chose to run.
//
// What survives is what a build or a test genuinely needs: the interpreter
// search path, a home to find tool config under, locale, a temp directory, and
// the toolchain caches whose absence turns every command into a cold rebuild.
// Nothing matching a credential shape is on the list, and a name not on the
// list does not reach the child at all.
func sanitizedEnv(dir string) []string {
	env := make([]string, 0, len(childEnvAllowlist)+1)
	for _, name := range childEnvAllowlist {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	// PWD is the shell's own idea of where it is; leaving the operator's would
	// contradict cmd.Dir.
	env = append(env, "PWD="+dir)
	return env
}

// childEnvAllowlist is the complete set of variable names passed through to a
// child process. Adding one is a deliberate decision about what an agent may
// read; nothing is matched by prefix or pattern, because a pattern is how
// ANTHROPIC_API_KEY ends up covered by a rule written for ANTHROPIC_BASE_URL.
var childEnvAllowlist = []string{
	// Where to find programs, and who is running them.
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM",
	// Scratch space.
	"TMPDIR", "TMP", "TEMP",
	// Text handling and clock, so output is not silently mangled or misdated.
	"LANG", "LC_ALL", "LC_CTYPE", "TZ",
	// Toolchain caches and roots. Dropping these does not fail closed, it
	// fails slow: every command becomes a cold rebuild.
	"GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOTMPDIR",
	"CARGO_HOME", "RUSTUP_HOME", "CARGO_TARGET_DIR",
	"npm_config_cache", "NODE_PATH",
	"PYTHONHASHSEED", "VIRTUAL_ENV",
}

func (r *Registry) listDir(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	targetPath := args.Path
	if targetPath == "" {
		targetPath = "."
	}
	full, err := r.resolveDirPath(targetPath)
	if err != nil {
		return tools.Errorf("%v", err)
	}

	type entry struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size_bytes,omitempty"`
	}
	var entries []entry

	// One ceiling for both branches. The recursive walk capped at 500 and said
	// so; the flat listing had no cap at all, so a directory with a hundred
	// thousand entries was rendered whole into one tool result.
	const maxEntries = 500

	if !args.Recursive {
		dirEntries, err := os.ReadDir(full)
		if err != nil {
			return tools.Errorf("reading directory %s: %v", targetPath, err)
		}
		truncated := len(dirEntries) > maxEntries
		if truncated {
			dirEntries = dirEntries[:maxEntries]
		}
		for _, de := range dirEntries {
			var size int64
			if info, err := de.Info(); err == nil && !de.IsDir() {
				size = info.Size()
			}
			entries = append(entries, entry{
				Name:  de.Name(),
				IsDir: de.IsDir(),
				Size:  size,
			})
		}
		payload := map[string]any{
			"path":    targetPath,
			"count":   len(entries),
			"entries": entries,
		}
		// A capped sample is never handed back looking like the whole
		// directory: a caller that concluded "the file is not here" from a
		// silently trimmed listing would be concluding it from nothing.
		if truncated {
			payload["truncated"] = true
			payload["limit"] = maxEntries
		}
		return ok(payload)
	}

	count := 0
	truncated := false
	if err := filepath.WalkDir(full, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if count >= maxEntries {
			truncated = true
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(r.deps.Root, p)
		if err != nil || rel == "." {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".devcouncil" || d.Name() == "node_modules" || d.Name() == "target") {
			return filepath.SkipDir
		}
		count++
		var size int64
		if info, err := d.Info(); err == nil && !d.IsDir() {
			size = info.Size()
		}
		entries = append(entries, entry{
			Name:  filepath.ToSlash(rel),
			IsDir: d.IsDir(),
			Size:  size,
		})
		return nil
	}); err != nil {
		return tools.Errorf("walking directory %s: %v", targetPath, err)
	}
	payload := map[string]any{
		"path":    targetPath,
		"count":   len(entries),
		"entries": entries,
	}
	if truncated {
		payload["truncated"] = true
		payload["limit"] = maxEntries
	}
	return ok(payload)
}

func (r *Registry) grepSearch(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		MaxResults int    `json:"max_results"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return tools.Errorf("pattern is required")
	}

	// A regular expression, because the argument is called `pattern` and every
	// model that reaches for this tool writes one.
	//
	// It used to be strings.Contains. That is not a smaller feature, it is a
	// wrong answer: an alternation like `sys|time` matched nothing and came
	// back as {"count":0}, which is exactly the shape of "this file does not
	// use sys or time". A model asked to remove unused imports read that as
	// proof, then read `subprocess|re|os` as proof of the same thing about
	// imports the file plainly used, and spent its entire step budget trying to
	// reconcile two contradictory facts it had been handed. Nothing in the
	// result said the pattern had not been understood.
	//
	// So an unparseable pattern is now an error naming the fault, never an
	// empty match set. A search that could not run must not look like a search
	// that ran and found nothing.
	expr, err := regexp.Compile(args.Pattern)
	if err != nil {
		return tools.Errorf("pattern %q is not a valid regular expression: %v — "+
			"this is not a negative result, no search ran", args.Pattern, err)
	}

	if args.MaxResults <= 0 {
		args.MaxResults = 50
	}
	targetPath := args.Path
	if targetPath == "" {
		targetPath = "."
	}
	full, resolveErr := r.resolveDirPath(targetPath)
	if resolveErr != nil {
		return tools.Errorf("%v", resolveErr)
	}
	// An uncontained search root is refused outright rather than walked to an
	// empty result, for the same reason an unparseable pattern is: a search
	// that could not run must never be reported as a search that found nothing.
	if _, contained := containedRelOf(r.deps.Root, full); !contained {
		return tools.Errorf("path %q is outside the repository and grep reads only inside it — "+
			"this is not a negative result, no search ran", targetPath)
	}

	type match struct {
		Path       string `json:"path"`
		LineNumber int    `json:"line_number"`
		Line       string `json:"line"`
	}
	var matches []match
	truncated := false

	err = filepath.WalkDir(full, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(matches) >= args.MaxResults {
			truncated = true
			return filepath.SkipAll
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".devcouncil" || d.Name() == "target" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		// WalkDir does not follow symlinks, so a link arrives here as a link —
		// but os.ReadFile did follow it, and d.Info() is an lstat that measured
		// the link rather than its target. Anything that is not a plain file is
		// skipped before it can be opened, and the read itself goes through the
		// same contained reader read_file uses, so the size guard is an fstat on
		// what was actually opened.
		if !d.Type().IsRegular() {
			return nil
		}
		rel, contained := containedRelOf(r.deps.Root, p)
		if !contained {
			return nil
		}

		data, err := readContained(ctx, r.deps.Root, rel, maxToolReadBytes)
		if err != nil {
			return nil
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil
		}

		lines := strings.Split(string(data), "\n")
		for idx, line := range lines {
			if expr.MatchString(line) {
				matches = append(matches, match{
					Path:       filepath.ToSlash(rel),
					LineNumber: idx + 1,
					Line:       firstLines(strings.TrimSpace(line), 1),
				})
				if len(matches) >= args.MaxResults {
					truncated = true
					return filepath.SkipAll
				}
			}
		}
		return nil
	})

	if err != nil {
		return tools.Errorf("grep error: %v", err)
	}

	payload := map[string]any{
		"pattern": args.Pattern,
		"count":   len(matches),
		"matches": matches,
	}
	if truncated {
		payload["truncated"] = true
		payload["limit"] = args.MaxResults
	}
	return ok(payload)
}

// limitWriter forwards at most limit bytes to w and silently discards the rest,
// while remaining a well-behaved io.Writer.
//
// That second half is the whole point. io.Writer requires a short return to
// carry a non-nil error, and os/exec copies a child's stdout with io.Copy,
// which enforces it: the write that straddled the cap used to return
// (truncated, nil), io.Copy turned that into io.ErrShortWrite, os/exec closed
// the pipe, and the child took SIGPIPE. A command that had already run to
// completion was then reported as a failure — "failed to execute command" from
// exec_command, and "git … failed with exit code 141" from the git tools — for
// work whose side effects were already on disk. Capping output is not a reason
// to kill the process producing it, and it is never a reason to call a
// successful command failed.
//
// dropped counts what the cap ate, so a truncated capture can say so rather
// than passing a partial sample off as the whole output.
type limitWriter struct {
	w       io.Writer
	limit   int
	wrote   int
	dropped int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.wrote >= l.limit {
		l.dropped += len(p)
		return len(p), nil
	}
	remaining := l.limit - l.wrote
	if len(p) <= remaining {
		n, err := l.w.Write(p)
		l.wrote += n
		return n, err
	}
	n, err := l.w.Write(p[:remaining])
	l.wrote += n
	if err != nil {
		// A real downstream failure is still short, and still an error.
		return n, err
	}
	l.dropped += len(p) - remaining
	return len(p), nil
}

// truncated reports whether the cap discarded anything.
func (l *limitWriter) truncated() bool { return l.dropped > 0 }

// truncationNote renders the discarded byte count for a result, so a capped
// capture is never presented as complete coverage.
func (l *limitWriter) truncationNote() string {
	if !l.truncated() {
		return ""
	}
	return fmt.Sprintf("[output truncated: %d bytes captured, %d further bytes discarded]",
		l.wrote, l.dropped)
}

// escalate puts a blocked decision to an attached human and, if cleared,
// returns the decision as it stands after the grant.
//
// Three refusals are unconditional and none of them consult the approver: no
// approver attached, a rule no authority can clear, and a decision the ledger
// itself declines to grant. A hard rule is never even shown as a question —
// offering an allow that is going to be refused downstream teaches an operator
// that the control is advisory.
//
// subject is the operation that will actually be performed if this is cleared:
// the repository path for a write, the exact command line for an exec. Every
// caller passes it, because decision.Target is not reliably that thing. The
// command gate matches against a normalised form with trailing redirections
// stripped, and the denial carries the normalised string — so the approval card
// asked a human to clear `cat src/calc.go` while `cat src/calc.go > .env.local`
// was what stood ready to run, and the grant issued from it took its scope from
// the same stripped string, re-clearing every redirect variant of that command
// for the grant's whole life. What a human authorises is now exactly what
// executes, and the grant covers exactly what was shown.
func (r *Registry) escalate(ctx context.Context, decision policy.Decision, subject string) (policy.Decision, bool) {
	if r.deps.Approver == nil || !decision.Overridable() {
		return decision, false
	}
	if strings.TrimSpace(subject) == "" {
		// An escalation with nothing concrete to show is not a question a
		// human can answer, and consent to an unnamed operation is not consent.
		return decision, false
	}

	// The decision the human is shown and the decision the grant is issued from
	// are one and the same, and both name the real operation. The original is
	// left untouched so a refusal still reports what the ladder actually said.
	authorised := decision
	authorised.Target = subject

	state := r.sessionFor(ctx).State()
	answer, err := r.deps.Approver.Approve(ctx, ui.Request{
		Rule:      string(authorised.Rule),
		Severity:  string(authorised.Severity),
		Path:      authorised.Target,
		Subject:   string(policy.SubjectOf(authorised.Rule)),
		Reason:    authorised.Reason,
		TaskID:    state.TaskID,
		Grantable: authorised.Overridable(),
	})
	// An error, a refusal, and an allow with no reason are all refusals. A
	// question that could not be answered must never be treated as consent.
	if err != nil || !answer.Allow || strings.TrimSpace(answer.Reason) == "" {
		return decision, false
	}

	grant, err := r.deps.Gate.RequestOverride(authorised,
		grants.Grantor{Authority: grants.Human, ID: grantorID(answer.By)},
		answer.Reason, state.TaskID)
	if err != nil {
		return decision, false
	}

	// Re-evaluated rather than patched. The decision returned here is the one
	// the ledger actually produces for this write, so the qualification that
	// travels onto the result is the real grant rather than one assembled at
	// the call site.
	authorised.Action = policy.Allow
	authorised.GrantID = grant.ID
	authorised.GrantedBy = string(grant.Grantor.Authority)
	return authorised, true
}

func grantorID(by string) string {
	if by == "" {
		return "operator"
	}
	return by
}

// annotate carries a decision's qualifications onto a successful result.
//
// Every way an allow can be reached other than "the rules passed" travels with
// it: the grant that cleared it, the posture or mode that demoted it, the
// checks that could not run. Without this a write permitted by the dev posture
// is indistinguishable in the log from one the plan authorised, and the run
// report would be assembled from results that had already lost the distinction.
// refusal is the one place a blocked decision becomes a model-facing result.
//
// It was four identical literals, one per gated tool, and each of them had to
// remember to carry the decision's rule and severity onto the result. Nothing
// made them agree, and there was no single place to add the verdict itself —
// so the loop that reports the run was left inferring "the gate refused this"
// from Rule and IsError, two fields that allowed results also set. See
// tools.Result.Blocked.
func (r *Registry) refusal(d policy.Decision) tools.Result {
	payload, _ := json.Marshal(r.decisionPayload(d))
	return tools.Result{
		Text: string(payload), IsError: true, Blocked: true,
		Rule: string(d.Rule), Severity: string(d.Severity),
	}
}

func annotate(result tools.Result, d policy.Decision) tools.Result {
	if d.Clean() {
		return result
	}
	result.Blocked = d.Blocked()
	result.Rule = string(d.Rule)
	result.Severity = string(d.Severity)
	result.GrantID = d.GrantID
	result.GrantedBy = d.GrantedBy
	result.Demoted = d.Demoted
	result.Degraded = d.Degraded
	result.Widened = d.Widened
	if d.Demoted != "" {
		result.Text += fmt.Sprintf("\n[%s would have blocked this: %s — allowed by %s]",
			d.Rule, d.Reason, d.Demoted)
	}
	return result
}

// --- the override seam ---

func (r *Registry) requestOverride(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path    string `json:"path"`
		Command string `json:"command"`
		Rule    string `json:"rule"`
		Reason  string `json:"reason"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Reason) == "" {
		return tools.Errorf("a reason is required: an override nobody can review later is not worth recording")
	}
	state := r.sessionFor(ctx).State()
	if err := held(state); err != nil {
		return tools.Errorf("%v", err)
	}

	task, err := r.currentTask(ctx)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	// An unrecognised rule name is refused rather than interpreted. It cannot
	// be classified, so it would fall to the file gate by default — and the
	// file gate's soft rules are the agent-grantable ones, which means a typo
	// would be answered with a grant for a rule the caller never hit. The
	// blocked payload always carries the exact name; there is nothing to guess.
	if named := strings.TrimSpace(args.Rule); named != "" && !policy.RuleKnown(policy.RuleID(named)) {
		return tools.Errorf("unknown rule %q: name the rule from the block you hit, "+
			"exactly as its payload's \"rule\" field spelled it", named)
	}

	subject, target := overrideSubject(args.Rule, args.Command, args.Path)
	if target == "" {
		return tools.Errorf("nothing to override: give the %s the block named in its \"target\" field", subject)
	}

	// Re-evaluate rather than trusting the rule the model named. An agent that
	// could nominate the rule it is being granted could name a softer one than
	// the block it actually hit.
	var decision policy.Decision
	op := dc.OpWrite
	if subject == policy.SubjectCommand {
		cmdDecision, cmdErr := r.deps.Gate.EvaluateCommand(target, task)
		if cmdErr != nil {
			return unavailable("command policy decision", cmdErr)
		}
		decision = cmdDecision
	} else {
		if args.Rule == string(policy.RuleOperation) || strings.Contains(args.Reason, "delete") || strings.Contains(args.Rule, "delete") {
			op = dc.OpDelete
		}
		writeDecision, writeErr := r.deps.Gate.EvaluateWrite(target, task, op)
		if writeErr != nil {
			return unavailable("policy decision", writeErr)
		}
		if !writeDecision.Blocked() && op != dc.OpDelete {
			if delDec, delErr := r.deps.Gate.EvaluateWrite(target, task, dc.OpDelete); delErr == nil && delDec.Blocked() {
				writeDecision = delDec
			}
		}
		decision = writeDecision
	}
	if !decision.Blocked() {
		return ok(map[string]any{"granted": false, "reason": "not blocked; no override needed"})
	}

	grant, err := r.deps.Gate.RequestOverride(decision,
		grants.Grantor{Authority: grants.Agent, ID: state.Owner},
		args.Reason, state.TaskID)
	if err != nil {
		// A refusal is the seam working. Say which authority could clear it,
		// so the agent asks a human instead of retrying.
		return ok(map[string]any{
			"granted":          false,
			"rule":             string(decision.Rule),
			"severity":         string(decision.Severity),
			"error":            err.Error(),
			"suggested_action": humanPath(decision),
		})
	}
	payload := map[string]any{
		"granted":    true,
		"grant_id":   grant.ID,
		"rule":       string(decision.Rule),
		"target":     decision.Target,
		"expires_at": grant.ExpiresAt.Format(time.RFC3339),
		"note":       "recorded; the write will report as allowed-by-grant, never as a clean pass",
	}
	r.persistWidenedScope(ctx, decision, op, payload)
	return ok(payload)
}

// overrideSubject decides which gate an override request is about, and what the
// target text is.
//
// The rule leads, because the rule is the thing re-evaluated below and the thing
// the blocked payload handed the caller. Classification comes from the policy
// package, which owns the rule taxonomy: a prefix test spelled here would be a
// second, private answer to a question that already has one, and it would go
// stale the first time a command rule is named without the prefix it assumed.
//
// The populated argument only breaks a tie the rule cannot: a caller that named
// no rule at all. Naming neither argument is an error at the call site, not a
// silent evaluation of the empty path.
func overrideSubject(rule, command, path string) (policy.Subject, string) {
	id := policy.RuleID(strings.TrimSpace(rule))
	command = strings.TrimSpace(command)
	path = strings.TrimSpace(path)

	if id != policy.RuleNone && policy.IsCommandRule(id) {
		if command != "" {
			return policy.SubjectCommand, command
		}
		// Tolerated on purpose: the old advice put the command line in `path`,
		// and transcripts replayed from before that was fixed still do. Reading
		// it as a command is the correct evaluation of what the caller meant,
		// and it is what stops the file gate from ever seeing a command again.
		return policy.SubjectCommand, path
	}
	if id != policy.RuleNone {
		if path != "" {
			return policy.SubjectPath, path
		}
		return policy.SubjectPath, command
	}
	if command != "" {
		return policy.SubjectCommand, command
	}
	return policy.SubjectPath, path
}

// persistWidenedScope writes a just-granted path into the task's own scope, so
// the argument the agent made survives the grant that recorded it.
//
// Without this the seam is honest but exhausting. A grant lives for minutes by
// design — it is an exception, and an exception with no expiry is a permission —
// while the task it was argued inside lives for as long as the work does. On
// anything longer than the ceiling the agent meets the identical block, writes
// the identical reason, and a second grant appears in the ledger saying what the
// first one already said. Recording the conclusion where the gate reads scope
// from ends that loop without loosening anything: the widened path is still
// bounded by every rung above it, and every write it authorises is marked
// widened rather than clean.
//
// Only scope.unplanned is persisted. It is the one rule that means "the plan did
// not mention this file" — a gap in the plan, which is the thing worth closing.
// The others mean the plan mentioned it and said no, and closing that gap is
// editing someone else's decision rather than completing it.
//
// The human approval path deliberately does not come through here. An operator
// answering "allow this write?" consented to a write, not to an amendment of the
// plan, and taking the second from the first would be answering a question they
// were not asked.
func (r *Registry) persistWidenedScope(ctx context.Context, decision policy.Decision, op dc.Operation, payload map[string]any) {
	if decision.Rule != policy.RuleUnplannedScope {
		return
	}

	// The target is a concrete path and planned files are matched as fnmatch
	// patterns, so it is quoted to cover itself and nothing else. Unquoted, a
	// real file named "a[bc].go" would widen scope to "ab.go" and "ac.go" as
	// well — the same defect the grant ledger quotes its own paths against.
	entry := dc.PlannedFile{
		Path:          fnmatch.QuoteMeta(decision.Target),
		AllowedChange: appendedChangeFor(op),
	}
	holder := r.sessionFor(ctx).State()
	added, err := r.deps.Store.AppendPlannedFiles(ctx, holder.TaskID, holder.Token, []dc.PlannedFile{entry})
	if err != nil {
		// Reported, not fatal. The grant already authorises this write, so
		// failing the call would refuse work that policy has allowed; what is
		// lost is only the durability, and the agent has to know that so it can
		// expect the block again rather than being surprised by it.
		payload["scope_persisted"] = false
		payload["scope_error"] = err.Error()
		payload["note"] = "granted, but this widening could not be written into the task's scope: " +
			"the grant stands and expires, and the same block will return"
		return
	}
	payload["scope_persisted"] = true
	if len(added) == 0 {
		// Nothing to add means the path was already in scope — the grant was
		// for a rung this path no longer reaches.
		payload["scope_note"] = "already in the task's scope; nothing appended"
		return
	}
	payload["scope_appended"] = entry.Path
	payload["scope_note"] = "written into this task's scope for the rest of its life; " +
		"writes it authorises report as widened, never as clean"
}

// appendedChangeFor is the permission an appended entry carries.
//
// It matches the operation that was blocked rather than defaulting to modify.
// An entry appended as modify would refuse a later delete of the same path under
// scope.operation — a rule no agent may grant itself — which would leave the
// agent worse off for having widened its scope than it was before.
func appendedChangeFor(op dc.Operation) dc.AllowedChange {
	if op == dc.OpDelete {
		return dc.ChangeDelete
	}
	return dc.ChangeModify
}

// humanPath names the recovery that remains when an agent may not grant a rule
// itself. It is subject-aware for the same reason the payload is: a command
// block's remedy is the task's allowed_commands, and telling an agent to update
// its planned files sends it to widen the wrong list.
func humanPath(d policy.Decision) string {
	if d.Severity == policy.Hard {
		return "this rule is never grantable; change the approach"
	}
	if policy.IsCommandRule(d.Rule) {
		return "ask a human to grant this (manvi allow --cmd), or add the command to the " +
			"task's allowed_commands. " + ungatedToolSentence
	}
	return "ask a human to grant this, or update the task's planned files"
}

// --- evidence ---

func (r *Registry) getDiff(ctx context.Context, call tools.Call) tools.Result {
	diff, notes, err := gitDiff(ctx, r.deps.Root)
	if err != nil {
		return unavailable("working-tree diff", err)
	}
	files, err := changedFiles(diff)
	if err != nil {
		return unavailable("diff parse", err)
	}
	out := map[string]any{"files": files, "diff": diff}
	if len(notes) > 0 {
		// The model has to be able to tell a complete diff from a capped one;
		// acting on a partial diff as though it were whole is how a change is
		// reported finished with files nobody looked at.
		out["degraded"] = notes
	}
	return ok(out)
}

func (r *Registry) verify(ctx context.Context, call tools.Call) tools.Result {
	report, err := r.report(ctx)
	if err != nil {
		return unavailable("verification", err)
	}
	return reported(report, report)
}

func (r *Registry) getGaps(ctx context.Context, call tools.Call) tools.Result {
	report, err := r.report(ctx)
	if err != nil {
		return unavailable("gaps", err)
	}
	return reported(map[string]any{"gaps": report.Gaps, "blocking": report.BlockingCount()}, report)
}

func (r *Registry) getNextActions(ctx context.Context, call tools.Call) tools.Result {
	report, err := r.report(ctx)
	if err != nil {
		return unavailable("next actions", err)
	}
	return reported(map[string]any{"next_actions": report.NextActions}, report)
}

// reported carries a verification's degradations onto the tool result.
//
// The JSON body already names them, and that is what the model reads. This is
// the other reader: the session log and the run report are assembled from
// tools.Result, and a verification whose degradations lived only in the payload
// would arrive there looking exactly like one where every gate ran —
// Result.Qualified() would answer false for a run that skipped a check. The
// three tools below all project the same report, so all three carry it; a gap
// list assembled with a gate switched off is as incomplete as the verdict is.
func reported(payload any, report *Report) tools.Result {
	result := ok(payload)
	result.Degraded = report.Degraded
	return result
}

func (r *Registry) patchFile(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path               string `json:"path"`
		TargetContent      string `json:"target_content"`
		ReplacementContent string `json:"replacement_content"`
		StartLine          int    `json:"start_line,omitempty"`
		EndLine            int    `json:"end_line,omitempty"`
		AllowMultiple      bool   `json:"allow_multiple,omitempty"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return tools.Errorf("path is required")
	}
	if args.TargetContent == "" {
		return tools.Errorf("target_content is required")
	}

	task, refusal := r.authorisingTask(ctx, "patching")
	if refusal != nil {
		return *refusal
	}
	// Pinned before evaluation and before any approval prompt: patch is
	// read-modify-write, so both halves must address the same verified file,
	// and a directory swapped mid-deliberation must void the whole operation.
	normalizedPatch, outside := policy.NormalizeRepoPath(r.deps.Root, args.Path)
	var pinned *pinnedTarget
	if !outside {
		var err error
		pinned, err = pinWriteTarget(r.deps.Root, normalizedPatch)
		if err != nil {
			return tools.Errorf("%v", err)
		}
	}
	decision, err := r.deps.Gate.EvaluateWrite(args.Path, task, dc.OpModify)
	if err != nil {
		return unavailable("policy decision", err)
	}
	if decision.Blocked() {
		escalated, ok := r.escalate(ctx, decision, decision.Target)
		if !ok {
			return r.refusal(decision)
		}
		decision = escalated
	}

	full, err := r.resolvePath(args.Path)
	if err != nil {
		return tools.Errorf("%v", err)
	}

	var content string
	if pinned != nil {
		data, err := pinned.Read(maxPatchReadBytes)
		if err != nil {
			return tools.Errorf("%v", err)
		}
		content = string(data)
	} else {
		data, err := os.ReadFile(full)
		if err != nil {
			if os.IsNotExist(err) {
				return tools.Errorf("file %s does not exist; use devcouncil_write_file to create new files", args.Path)
			}
			return tools.Errorf("reading %s: %v", args.Path, err)
		}
		content = string(data)
	}

	if args.StartLine > 0 || args.EndLine > 0 {
		lines := strings.Split(content, "\n")
		totalLines := len(lines)

		start := args.StartLine
		if start < 1 {
			start = 1
		}
		end := args.EndLine
		if end < 1 || end > totalLines {
			end = totalLines
		}
		if start > end || start > totalLines {
			return tools.Errorf("invalid line range [%d, %d] for file %s with %d lines",
				args.StartLine, args.EndLine, args.Path, totalLines)
		}

		chunkLines := lines[start-1 : end]
		chunk := strings.Join(chunkLines, "\n")

		occurrences := strings.Count(chunk, args.TargetContent)
		if occurrences == 0 {
			return tools.Errorf("target_content not found in lines %d-%d of %s", start, end, args.Path)
		}
		if occurrences > 1 && !args.AllowMultiple {
			return tools.Errorf("target_content found %d times in lines %d-%d of %s; narrow the line range or set allow_multiple: true",
				occurrences, start, end, args.Path)
		}

		var replacedChunk string
		if args.AllowMultiple {
			replacedChunk = strings.ReplaceAll(chunk, args.TargetContent, args.ReplacementContent)
		} else {
			replacedChunk = strings.Replace(chunk, args.TargetContent, args.ReplacementContent, 1)
		}

		var newLines []string
		if start > 1 {
			newLines = append(newLines, lines[:start-1]...)
		}
		newLines = append(newLines, replacedChunk)
		if end < totalLines {
			newLines = append(newLines, lines[end:]...)
		}
		content = strings.Join(newLines, "\n")
	} else {
		occurrences := strings.Count(content, args.TargetContent)
		if occurrences == 0 {
			return tools.Errorf("target_content not found in %s", args.Path)
		}
		if occurrences > 1 && !args.AllowMultiple {
			return tools.Errorf("target_content found %d times in %s; specify start_line and end_line or set allow_multiple: true",
				occurrences, args.Path)
		}

		if args.AllowMultiple {
			content = strings.ReplaceAll(content, args.TargetContent, args.ReplacementContent)
		} else {
			content = strings.Replace(content, args.TargetContent, args.ReplacementContent, 1)
		}
	}

	if pinned != nil {
		if err := pinned.Write([]byte(content), 0o644); err != nil {
			return tools.Errorf("writing patched %s: %v", args.Path, err)
		}
	} else {
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return tools.Errorf("writing patched %s: %v", args.Path, err)
		}
	}

	return annotate(
		tools.Result{
			Text:  fmt.Sprintf("patched %s (%d bytes)", decision.Target, len(content)),
			Wrote: []string{decision.Target},
		},
		decision)
}

func (r *Registry) findFiles(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path,omitempty"`
		MaxResults int    `json:"max_results,omitempty"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return tools.Errorf("pattern is required")
	}
	if args.MaxResults <= 0 {
		args.MaxResults = 100
	}
	targetPath := args.Path
	if targetPath == "" {
		targetPath = "."
	}
	full, err := r.resolveDirPath(targetPath)
	if err != nil {
		return tools.Errorf("%v", err)
	}

	var matches []string
	truncated := false
	err = filepath.WalkDir(full, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(matches) >= args.MaxResults {
			truncated = true
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(r.deps.Root, p)
		if err != nil || rel == "." {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == ".devcouncil" || name == "node_modules" || name == "target" || name == ".venv" {
				return filepath.SkipDir
			}
			return nil
		}
		slashRel := filepath.ToSlash(rel)
		fileName := d.Name()

		if fnmatch.Match(args.Pattern, slashRel) || fnmatch.Match(args.Pattern, fileName) {
			matches = append(matches, slashRel)
			if len(matches) >= args.MaxResults {
				truncated = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return tools.Errorf("searching files in %s: %v", targetPath, err)
	}

	payload := map[string]any{
		"pattern": args.Pattern,
		"path":    targetPath,
		"count":   len(matches),
		"files":   matches,
	}
	if truncated {
		payload["truncated"] = true
		payload["limit"] = args.MaxResults
	}
	return ok(payload)
}

// spawnSubagents fans delegated work out across a bounded worker pool.
//
// What this must never do is the thing it used to do. The handler decoded
// `prompt` — required by its own schema — and then never read it; each worker
// took a lease and returned {"status":"completed"} for work no model had been
// asked to perform. Because agents.Result drops its Value from the JSON, even
// that fiction never reached the caller: a fan-out of four analyses came back
// as {"children":4,"failed":0}, which reads as four successes and is the
// strongest possible claim the payload can make. An agent fanning out four
// investigations was told all four had landed and reported the work done.
//
// So the capability is now either present or refused. With no runner attached
// the dispatch fails before it acquires anything — refusing after taking leases
// would leave every named task locked against other builders until its TTL
// lapsed, on behalf of work that never started.
func (r *Registry) spawnSubagents(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Tasks []struct {
			Label    string `json:"label"`
			Prompt   string `json:"prompt"`
			TaskID   string `json:"task_id,omitempty"`
			ReadOnly bool   `json:"read_only,omitempty"`
			Type     string `json:"type,omitempty"`
		} `json:"tasks"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if len(args.Tasks) == 0 {
		return tools.Errorf("tasks array is required and must not be empty")
	}

	// Validated before anything is acquired. The schema marks prompt required,
	// but the schema is a request to the model, not a guarantee about what
	// arrives, and a child dispatched with an empty prompt is a child that
	// cannot do anything except report that it finished.
	for i, t := range args.Tasks {
		if strings.TrimSpace(t.Label) == "" {
			return tools.Errorf("tasks[%d] has no label; a fan-out result nobody can attribute is not a result", i)
		}
		if strings.TrimSpace(t.Prompt) == "" {
			return tools.Errorf("tasks[%d] (%s) has an empty prompt; there is nothing to delegate", i, t.Label)
		}
	}

	// Resolved by agents.ResolveBounds rather than re-derived here. This block
	// was a second copy of that logic, and it had already drifted: its
	// fallbacks were the literals 2 and 4 while the registry's defaults moved
	// on, so a run with no configuration used one pair of bounds and a run with
	// configuration used another. Two readers of one setting is one reader too
	// many when the setting is a bound on how much work a turn may spawn.
	var reg *flags.Registry
	if r.deps.Gate != nil {
		reg = r.deps.Gate.Flags
	}
	bounds := agents.ResolveBounds(reg)
	maxDepth, maxFanout := bounds.MaxDepth, bounds.MaxFanout

	// The depth bound, applied. It was read into a variable, handed to a pool
	// whose Child() nothing ever calls, and enforced nowhere: setting
	// agents.max_spawn_depth to 0 dispatched a full fan-out exactly as 2 did.
	// A setting that reports itself through `manvi flags` and binds at no value
	// is worse than one that is not offered, because an operator reads it as a
	// control that is in force.
	//
	// Depth below this level stays structural — a child's registry simply has
	// no dispatching tools, so there is no path to a grandchild to count. What
	// this adds is the level the structure cannot express: zero, meaning this
	// harness delegates nothing at all. agents.max_fanout cannot say that; its
	// floor is one child.
	if maxDepth < 1 {
		// The reason, not the flag, when something other than the flag decided.
		// A refusal citing agents.max_spawn_depth=0 to an operator who set it
		// to 2 sends them to look at a setting that is not the cause.
		cause := fmt.Sprintf("%s=%d", flags.AgentsMaxSpawnDepth, maxDepth)
		if bounds.DepthReason != "" {
			cause = bounds.DepthReason
		}
		res := tools.Errorf(
			"delegation is off: %s, so no sub-agent may be dispatched at any width. "+
				"Nothing was dispatched and no child ran — this is not a report of zero results. "+
				"Carry out the work in this turn.",
			cause)
		res.Rule = flags.AgentsMaxSpawnDepth
		return res
	}

	// Checked after the depth bound above. Both refuse the same call, and when
	// both apply the setting is the more useful answer: "no runner is attached"
	// sends an operator to look at how the harness was built, when what
	// actually decided it was a value they can change.
	runner := r.deps.SubAgent
	if runner == nil {
		// Named as a missing capability rather than as a failed fan-out, so the
		// dispatching agent stops delegating and does the work itself instead
		// of retrying a tool that cannot ever answer.
		return tools.Errorf(
			"sub-agent dispatch is not available: this harness has no sub-agent runner attached, " +
				"so no child could be run — this is not a report of zero results, nothing was delegated; " +
				"carry out the work in this turn instead of fanning it out")
	}

	roles := r.getSubagentRegistry()

	// Where the children will actually run decides how many may run at once.
	// The bound above came from the session's default provider, which is the
	// wrong answer for a mixed team: a frontier parent placing builders on a
	// local model resolved to the frontier width and dispatched all of them
	// onto the one device the local cap exists to protect.
	placements := make([]string, 0, len(args.Tasks))
	for _, t := range args.Tasks {
		if t.Type == "" {
			continue
		}
		if def, ok := roles.Get(t.Type); ok {
			placements = append(placements, def.Model)
		}
	}
	maxFanout, narrowed := agents.FanoutFor(bounds, placements)

	pool, err := agents.New(maxDepth, maxFanout, r.deps.Store)
	if err != nil {
		return tools.Errorf("creating subagent pool: %v", err)
	}

	subTasks := make([]agents.Task, 0, len(args.Tasks))
	for _, t := range args.Tasks {
		t := t
		subTasks = append(subTasks, agents.Task{
			Label: t.Label,
			Run: func(childCtx context.Context, holder *agents.Holder) (any, error) {
				if t.TaskID != "" {
					req := store.AcquireRequest{
						TaskID: t.TaskID,
						Owner:  t.Label,
						TTL:    r.deps.LeaseTTL,
					}
					lease, err := r.deps.Store.Acquire(childCtx, req)
					if err != nil {
						return nil, fmt.Errorf("acquiring task %s: %w", t.TaskID, err)
					}
					if lease == nil {
						return nil, fmt.Errorf("task %s is already held by another agent", t.TaskID)
					}
					// Registered before the child does any work with it, so a
					// cancellation always finds it. See agents.Holder.
					holder.Add(agents.Lease{TaskID: lease.TaskID, Token: lease.Token})
				}

				// A task may name a role, which is what lets one fan-out
				// put a planner on one model and its workers on another. A
				// task that names none keeps the placement this tool has
				// always had: the parent's.
				req := SubAgentRequest{
					Label: t.Label, Prompt: t.Prompt, TaskID: t.TaskID, ReadOnly: t.ReadOnly,
					// So a lease this child takes for itself is released by the
					// same cleanup that releases one the dispatch named.
					Leases: leaseSinkFor(holder),
				}
				if t.Type != "" {
					if def, ok := roles.Get(t.Type); ok {
						req.ModelSpec = def.Model
						req.SystemPrompt = def.SystemPrompt
						req.Surface = def.Surface()
						// ReadOnly is a floor, never a ceiling: a caller that
						// asked for a non-mutating child must not be handed a
						// writing one because the role permits writes.
						req.ReadOnly = t.ReadOnly || !def.EnableWriteTools
						if t.ReadOnly {
							// The caller demanded a non-mutating child, so the
							// role's named write exceptions do not apply. Both
							// facts arrive as one bool at the runner, so the
							// distinction has to be made here, where the
							// caller's own request is still separable from the
							// role's default.
							req.Surface.Writes = nil
						}
					}
				}

				out, err := runner.RunSubAgent(childCtx, req)
				if err != nil {
					return nil, fmt.Errorf("sub-agent %s: %w", t.Label, err)
				}
				if strings.TrimSpace(out.Summary) == "" {
					// A child that ran and produced nothing is a failure, not a
					// quiet success. Counting it among the completions is how a
					// fan-out comes to report work that did not happen.
					return nil, fmt.Errorf("sub-agent %s returned no summary; "+
						"it produced nothing that can be reported as its work", t.Label)
				}
				child := map[string]any{
					"status":  "completed",
					"task_id": t.TaskID,
					"summary": out.Summary,
					"steps":   out.Steps,
					"usage":   out.Usage,
				}
				if len(out.Wrote) > 0 {
					child["wrote"] = out.Wrote
				}
				if out.WroteTruncated {
					child["wrote_truncated"] = true
				}
				if out.Verdict.Judged {
					child["verdict"] = out.Verdict.Reconcile()
				}
				return child, nil
			},
		})
	}

	results, err := pool.Run(ctx, subTasks)
	if err != nil {
		return tools.Errorf("subagent fan-out failed: %v", err)
	}

	// agents.Result carries its Value with a `json:"-"` tag, so marshalling the
	// results directly would hand the caller labels and lease bookkeeping and
	// no sub-agent output at all — a fan-out reported entirely as a count of
	// children that did not fail. The outcomes are projected here so what each
	// child actually produced is what the dispatching agent reads.
	outcomes := make([]map[string]any, 0, len(results))
	for _, res := range results {
		outcome := map[string]any{"label": res.Label}
		if res.Err != nil {
			outcome["status"] = "failed"
			outcome["error"] = res.Error
		} else if value, isMap := res.Value.(map[string]any); isMap {
			for k, v := range value {
				outcome[k] = v
			}
		} else {
			// Unreachable with the closure above, and still not reported as a
			// success: an outcome nobody can read is not evidence of work.
			outcome["status"] = "failed"
			outcome["error"] = "the sub-agent returned an unreadable outcome"
		}
		if len(res.ReleasedLeases) > 0 {
			outcome["released_leases"] = res.ReleasedLeases
		}
		if len(res.OrphanedLeases) > 0 {
			outcome["orphaned_leases"] = res.OrphanedLeases
		}
		outcomes = append(outcomes, outcome)
	}

	report := agents.Summarise(results)
	// Summed here as well as reported per child, because the dispatching model
	// is deciding whether to delegate again and the per-child figures make it
	// do the arithmetic. Absent rather than zero when nothing was learned: a
	// fan-out whose children all failed spent nothing this can see, and
	// printing 0 would read as "delegation is free".
	var spent SubAgentUsage
	for _, res := range results {
		if value, isMap := res.Value.(map[string]any); isMap {
			if u, ok := value["usage"].(SubAgentUsage); ok {
				spent.Add(u)
			}
		}
	}
	payload := map[string]any{
		"report":  report,
		"clean":   report.Clean(),
		"results": outcomes,
	}
	if narrowed != "" {
		// Said out loud. An operator who set max_fanout to eight and watched
		// two children run has to be able to find out which of their settings
		// did not decide it.
		payload["fanout_narrowed"] = narrowed
	}
	if spent.Any() {
		payload["usage"] = spent
	}

	// The children's changed paths are carried onto this call's own result so
	// the parent turn's terminal checkpoint can see them. Without this hop the
	// paths stop at the child's log: the parent's checkpoint would be told the
	// turn mutated — a dispatch is a mutating tool — and handed no file to
	// look at, which is a check that runs against nothing and passes.
	var wrote []string
	truncated := false
	for _, res := range results {
		value, isMap := res.Value.(map[string]any)
		if !isMap {
			continue
		}
		if paths, ok := value["wrote"].([]string); ok {
			wrote, truncated = mergeWrites(wrote, truncated, paths)
		}
		if flag, ok := value["wrote_truncated"].(bool); ok && flag {
			truncated = true
		}
	}
	// A fan-out in which every child failed is a failed check, and this package
	// has one rule for those: "a failed check is an error result, never an empty
	// success" (see unavailable). It was returning ok() regardless, so the model
	// read a total failure as a completed delegation — and, because a non-error
	// result from a mutating tool is credited as progress, the no-progress
	// detector reset its streak too. A model that keeps delegating into failures
	// was never interrupted.
	//
	// Partial failure stays a success result: some work did land, and the
	// per-child status and clean=false in the payload say what did not.
	if report.Children > 0 && report.Failed == report.Children {
		res := failure(payload, fmt.Sprintf(
			"all %d sub-agent(s) failed; nothing was delegated successfully", report.Children))
		// Reported even on a total failure. A child that wrote a file and then
		// failed still changed the tree, and a checkpoint told otherwise would
		// leave those bytes unexamined.
		res.Wrote = wrote
		return res
	}
	res := ok(payload)
	res.Wrote = wrote
	if truncated {
		res.Degraded = append(res.Degraded,
			"a sub-agent changed more paths than it reported; the list is incomplete")
	}
	return res
}

// mergeWrites folds one child's changed paths into a fan-out's set, ordered,
// de-duplicated and capped. The cap reports itself for the same reason the
// loop's does: a truncated list passed on as a complete one is how files come
// to be recorded as checked without anything having looked at them.
func mergeWrites(have []string, truncated bool, add []string) ([]string, bool) {
	for _, p := range add {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if slices.Contains(have, p) {
			continue
		}
		if len(have) >= maxReportedChildWrites {
			return have, true
		}
		have = append(have, p)
	}
	return have, truncated
}

// maxReportedChildWrites bounds how many child-changed paths one fan-out
// reports upward. Sized to the loop's own per-turn cap: carrying more than the
// parent can track would be counted and then discarded a layer later, which is
// a truncation nobody records.
const maxReportedChildWrites = 256

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
