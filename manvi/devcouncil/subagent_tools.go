package devcouncil

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The governing invariant of this file:
//
//	A control-plane tool must never report an outcome it did not achieve.
//
// These four tools are how a model observes and steers its own children, and
// each of them once answered success for something that did not happen: a kill
// that cancelled nothing and replied {"killed":[id]} while the child ran on and
// wrote to the tree; a kill of an ID nobody had registered, and a kill_all over
// an empty manager, both reported as done; a message queued into a channel with
// no reader and answered {"delivered":true}; a child that produced nothing
// reported "completed"; a role name with a typo in it silently producing a
// child with the parent's whole tool surface.
//
// The rule that follows from it, in both directions: an action that could not
// be delivered is an error result naming what was not done, and an outcome is
// only reported once the thing it describes has actually been established. A
// control that reports an action it did not perform is worse than no control,
// because the operator and the model both stop looking.

var conversationCounter atomic.Int64

// fallbackPlanes holds the role catalogue and instance table a Registry uses
// when Deps supplied neither.
//
// It is keyed per Registry and built once, because the getters below used to
// answer agents.NewRegistry() / agents.NewInstanceManager() on every single
// call. Two consequences, both of them this file's invariant broken: a role
// defined in one call was written into a throwaway and looked up in a different
// one on the next, so a dispatch under that name found nothing and silently
// fell back; and every instance a dispatch registered went into a manager the
// management tool would never see, so "list" answered count 0 with children
// live and "kill" could never find anything to kill.
//
// Production wires both (cmd/manvi/harness.go), so this map stays empty there. It
// holds a reference to any Registry that does reach it, which is a bounded cost
// only an embedder that leaves the fields nil pays.
var fallbackPlanes sync.Map // *Registry -> *subagentPlane

type subagentPlane struct {
	roles     *agents.Registry
	instances *agents.InstanceManager
}

func (r *Registry) fallbackPlane() *subagentPlane {
	if plane, ok := fallbackPlanes.Load(r); ok {
		return plane.(*subagentPlane)
	}
	plane, _ := fallbackPlanes.LoadOrStore(r, &subagentPlane{
		roles:     agents.NewRegistry(),
		instances: agents.NewInstanceManager(),
	})
	return plane.(*subagentPlane)
}

func (r *Registry) subagentTools() []tools.Tool {
	return []tools.Tool{
		{
			Schema: schema("devcouncil_define_subagent",
				"Dynamically define or update a specialized subagent type with a custom role, system prompt, and tool configuration.",
				`{"type":"object","properties":{"name":{"type":"string","description":"unique name for subagent type"},"role":{"type":"string","description":"role description"},"description":{"type":"string","description":"human-readable description"},"system_prompt":{"type":"string","description":"detailed system prompt"},"model":{"type":"string","description":"model name or inherit/flash/pro"},"enable_mcp_tools":{"type":"boolean","description":"admit the mcp tool group; false takes it away from the child"},"enable_write_tools":{"type":"boolean","description":"false makes the child read-only"},"allowed_tools":{"type":"array","items":{"type":"string"},"description":"allowlist of tool names; it only ever removes, never widens what enable_write_tools and enable_mcp_tools permit"}},"required":["name","description","system_prompt"]}`),
			Group:    tools.GroupSubagent,
			Handler:  r.defineSubagent,
			Extended: true,
		},
		{
			Schema: schema("devcouncil_invoke_subagent",
				"Invoke one or more subagents concurrently by name with custom task prompts, returning their unique conversation IDs and execution outcomes.",
				`{"type":"object","properties":{"subagents":{"type":"array","items":{"type":"object","properties":{"type_name":{"type":"string","description":"subagent type name"},"role":{"type":"string","description":"specific role description"},"prompt":{"type":"string","description":"actionable task description"},"model":{"type":"string"}},"required":["type_name","role","prompt"]}}},"required":["subagents"]}`),
			Group:    tools.GroupSubagent,
			Handler:  r.invokeSubagents,
			Extended: true,
		},
		{
			Schema: schema("devcouncil_send_message",
				"Send a message or instruction to an active subagent by its conversation ID.",
				`{"type":"object","properties":{"recipient":{"type":"string","description":"recipient conversation ID"},"message":{"type":"string","description":"message content"}},"required":["recipient","message"]}`),
			Group:    tools.GroupSubagent,
			Handler:  r.sendMessage,
			Extended: true,
		},
		{
			Schema: schema("devcouncil_manage_subagents",
				"Manage subagents: list active direct subagents with live state, or terminate specific/all subagents.",
				`{"type":"object","properties":{"action":{"type":"string","enum":["list","kill","kill_all"],"description":"management action"},"conversation_ids":{"type":"array","items":{"type":"string"},"description":"IDs to kill when action is 'kill'"}},"required":["action"]}`),
			Group:    tools.GroupSubagent,
			Handler:  r.manageSubagents,
			Extended: true,
		},
	}
}

func (r *Registry) getSubagentRegistry() *agents.Registry {
	if r.deps.SubagentRegistry != nil {
		return r.deps.SubagentRegistry
	}
	return r.fallbackPlane().roles
}

func (r *Registry) getSubagentManager() *agents.InstanceManager {
	if r.deps.SubagentMgr != nil {
		return r.deps.SubagentMgr
	}
	return r.fallbackPlane().instances
}

// requireSummary is the one place this package decides whether a child that
// returned without an error actually did any work.
//
// A child that ran and produced nothing is a failure, not a quiet success.
// Counting it among the completions is how a fan-out comes to report work that
// did not happen.
//
// It is a function rather than a line inside each dispatcher because there are
// two dispatchers and the judgement drifted between them: invoke_subagent lost
// the check entirely and reported "completed" for an empty result. Both call
// it now.
func requireSummary(label, summary string) error {
	if strings.TrimSpace(summary) != "" {
		return nil
	}
	return fmt.Errorf("sub-agent %s returned no summary; "+
		"it produced nothing that can be reported as its work", label)
}

func (r *Registry) defineSubagent(ctx context.Context, call tools.Call) tools.Result {
	if refusal := r.refuseDynamicRoles(); refusal != nil {
		return *refusal
	}
	var def agents.Definition
	if err := decode(call, &def); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if refusal := refuseRetiredRoleKeys(call); refusal != nil {
		return *refusal
	}
	if strings.TrimSpace(def.Name) == "" {
		return tools.Errorf("subagent name is required")
	}
	if strings.TrimSpace(def.SystemPrompt) == "" {
		return tools.Errorf("system_prompt is required")
	}

	reg := r.getSubagentRegistry()
	// A shipped role is not redefinable, whatever subagents.dynamic.enabled
	// says. The two are different questions: the setting decides whether a
	// model may author roles at all, while this decides whether it may rewrite
	// one the operator already reviewed. Registry.Register overwrites by name,
	// so without this check "define a role" and "rewrite the shipped read-only,
	// MCP-denied critic to say you may write, with MCP on" are the same call —
	// a reviewed permission widened inside a single turn, under a name every
	// later dispatch still reads as the reviewed one.
	if reg.IsBuiltIn(def.Name) {
		return tools.Errorf(
			"%q is a role this harness ships, and a shipped role cannot be redefined at runtime. "+
				"Nothing was registered and %q is unchanged. Define a role under a different name instead",
			def.Name, def.Name)
	}
	if err := reg.Register(def); err != nil {
		return tools.Errorf("registering subagent: %v", err)
	}

	return ok(map[string]any{
		"status":      "defined",
		"name":        def.Name,
		"role":        def.Role,
		"description": def.Description,
	})
}

// invokeRequest is one entry of devcouncil_invoke_subagent's subagents array.
type invokeRequest struct {
	TypeName string `json:"type_name"`
	Role     string `json:"role"`
	Prompt   string `json:"prompt"`
	Model    string `json:"model,omitempty"`
	// Workspace is decoded only so it can be refused. The schema no
	// longer offers it and Definition no longer carries it, but
	// json.Unmarshal drops an unknown key without a word, and a
	// silently dropped isolation request is the exact defect the
	// removal was for.
	Workspace string `json:"workspace,omitempty"`
}

func (r *Registry) invokeSubagents(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Subagents []invokeRequest `json:"subagents"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if len(args.Subagents) == 0 {
		return tools.Errorf("subagents array is required and must not be empty")
	}
	for i, s := range args.Subagents {
		if mode := strings.TrimSpace(s.Workspace); mode != "" {
			return tools.Errorf("subagents[%d] (%s) asks for workspace %q: %s",
				i, s.Role, mode, unimplementedWorkspace)
		}
	}

	reg := r.getSubagentRegistry()
	mgr := r.getSubagentManager()

	runner := r.deps.SubAgent

	bounds, refusal := r.delegationBounds()
	if refusal != nil {
		return *refusal
	}

	// The same narrowing the other dispatcher applies, from the same function:
	// how many children may run at once depends on where they will run, and the
	// bound above was resolved from the session's default provider.
	placements := make([]string, 0, len(args.Subagents))
	for _, sub := range args.Subagents {
		if override := strings.TrimSpace(sub.Model); override != "" {
			placements = append(placements, override)
			continue
		}
		if def, ok := reg.Get(sub.TypeName); ok {
			placements = append(placements, def.Model)
		}
	}

	// unknownTypes names every type_name that is not a registered role. A typo
	// in a role name used to be invisible: the dispatch fell back to a
	// synthetic definition with EnableWriteTools true, so "critc" produced a
	// child with the parent's whole tool surface and write access, and the
	// result said nothing about it. The fallback is now read-only, and the
	// names are reported, so a caller can tell "the role I named" from "a role
	// this harness has never heard of".
	var unknownTypes []string

	return r.runFanout(ctx, bounds, fanout{
		placements: placements,
		tasks: func() []agents.Task {
			var subTasks []agents.Task
			for _, s := range args.Subagents {
				def, ok := reg.Get(s.TypeName)
				if !ok {
					unknownTypes = append(unknownTypes, s.TypeName)
				}
				subTasks = append(subTasks, invokeTask(s, def, ok, mgr, runner))
			}
			return subTasks
		},
		runFailure: "invoking subagents failed",
		verb:       "dispatched",
		payload: func(payload map[string]any) {
			// "dispatched" counts what was asked for, not what ran.
			payload["dispatched"] = len(args.Subagents)
			if len(unknownTypes) > 0 {
				payload["unknown_types"] = unknownTypes
				payload["unknown_types_note"] = fmt.Sprintf(
					"these type_name values are not registered roles (%s). Each ran read-only on the "+
						"parent's tool surface, under no role's system prompt, model placement or tool "+
						"policy. Check the spelling against the registered roles, or define the role first",
					strings.Join(unknownTypes, ", "))
			}
		},
	})
}

// invokeTask is one devcouncil_invoke_subagent child: a tracked instance the
// control plane can list and kill, running under the role s names. known is
// false when no role by that name is registered, and def is then the zero
// value.
func invokeTask(s invokeRequest, def agents.Definition, known bool,
	mgr *agents.InstanceManager, runner SubAgentRunner) agents.Task {
	// The surface is taken only from a role that actually exists. The
	// fallback below is synthetic — nobody wrote it down — so it declares
	// nothing and the child inherits the parent's tool set, which is the
	// same reading an unknown type already gets for its placement. Building
	// a surface out of the fallback's zero fields would instead read as a
	// role that deliberately denied the MCP group.
	var surface agents.ToolSurface
	if known {
		surface = def.Surface()
	} else {
		// Auto-fallback to self, with writes withheld. A permission nobody
		// wrote down is a permission nobody reviewed, and the direction an
		// unverifiable permission has to fail is closed: the caller named a
		// role that does not exist, and the one thing that must not follow
		// from a misspelling is a child that can mutate the working tree.
		def = agents.Definition{
			Name:             s.TypeName,
			Role:             s.Role,
			EnableWriteTools: false,
		}
	}

	convID := fmt.Sprintf("subagent-%d-%d", time.Now().UnixMilli(), conversationCounter.Add(1))

	return agents.Task{
		Label: s.Role,
		Run: func(childCtx context.Context, holder *agents.Holder) (any, error) {
			childCtx, cancel := context.WithCancel(childCtx)
			defer cancel()

			// This is the cancellation the control plane delivers, bound to
			// the instance before it is registered. Instance.cancel was
			// never assigned anywhere outside a test, so every kill found
			// nil, cancelled nothing, moved the state to canceling and
			// answered {"killed":[id]} — while the child ran to completion
			// and went on writing to the tree.
			inst, err := agents.NewInstance(convID, s.TypeName, s.Role, cancel)
			if err != nil {
				return nil, fmt.Errorf("sub-agent %q could not be tracked: %w", s.Role, err)
			}
			// Refused rather than dispatched untracked: a child nothing can
			// list or terminate is one the control plane would answer
			// questions about that it cannot answer.
			if err := mgr.Register(inst); err != nil {
				return nil, fmt.Errorf("sub-agent %q could not be registered: %w", s.Role, err)
			}

			if runner == nil {
				// No runner means no model, which means no work was done.
				//
				// This branch used to return status "completed" with a
				// summary it composed from the prompt — "subagent %s
				// finished executing task: %s" — for a child that had never
				// existed. A dispatching agent reading four of those reports
				// four analyses it never received, and nothing anywhere
				// says otherwise. "Standalone mode" is not a reason to
				// report success; it is a reason to report that the
				// capability is absent.
				return nil, recordFailure(inst, fmt.Errorf(
					"sub-agent %q cannot run: no model is attached to this invocation", s.Role))
			}

			// The role decides where the child runs, and a per-call model
			// overrides it. The override is honoured rather than ignored
			// because the schema has advertised `model` since this tool
			// shipped: the handler decoded it into a field it never read,
			// which tells the model it has control it does not have.
			modelSpec := def.Model
			if override := strings.TrimSpace(s.Model); override != "" {
				modelSpec = override
			}

			out, err := runner.RunSubAgent(childCtx, SubAgentRequest{
				Label:        s.Role,
				Prompt:       s.Prompt,
				ReadOnly:     !def.EnableWriteTools,
				ModelSpec:    modelSpec,
				SystemPrompt: def.SystemPrompt,
				Surface:      surface,
				// So a lease this child takes for itself is released by the
				// same cleanup that releases one the dispatch named.
				Leases: leaseSinkFor(holder),
			})
			if err != nil {
				return leftBehind(out), recordFailure(inst, err)
			}
			// The same judgement the other dispatcher makes, from the same
			// function rather than a second copy of it: a child that came
			// back with nothing to say is not a completion.
			if err := requireSummary(s.Role, out.Summary); err != nil {
				return leftBehind(out), recordFailure(inst, err)
			}

			// Only now, with a result in hand, is "completed" claimed — and
			// only if the lifecycle still allows it. The one move this
			// refuses is completed-after-canceling, which is a child the
			// control plane terminated mid-flight; reporting that as
			// completed work is exactly the fabrication this file exists to
			// prevent.
			if err := inst.SetState(agents.StateCompleted, "done"); err != nil {
				return leftBehind(out), fmt.Errorf(
					"sub-agent %q was terminated before it could report its work: %w", s.Role, err)
			}
			outcome := completedOutcome(out)
			outcome["conversation_id"] = convID
			outcome["role"] = s.Role
			outcome["type"] = s.TypeName
			if !known {
				// Carried on the child's own outcome as well as on the
				// payload, because this is the line a dispatching model
				// reads when it is deciding whether the answer it got came
				// from the role it asked for.
				outcome["unknown_type"] = true
				outcome["note"] = fmt.Sprintf(
					"no role named %q is registered; this child ran read-only on the parent's "+
						"tool surface rather than under that role", s.TypeName)
			}
			return outcome, nil
		},
	}
}

// recordFailure moves an instance to errored and returns the failure the child
// is to report.
//
// The state write is checked rather than dropped: the only way it is refused is
// that the instance already reached a terminal state, and a lifecycle the
// control plane cannot record is not something to swallow on the way out of an
// error path.
func recordFailure(inst *agents.Instance, cause error) error {
	if stateErr := inst.SetState(agents.StateErrored, cause.Error()); stateErr != nil {
		return fmt.Errorf("%w (and its state could not be recorded: %v)", cause, stateErr)
	}
	return cause
}

func (r *Registry) sendMessage(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Recipient string `json:"recipient"`
		Message   string `json:"message"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Recipient) == "" {
		return tools.Errorf("recipient conversation ID is required")
	}
	if strings.TrimSpace(args.Message) == "" {
		return tools.Errorf("message content is required")
	}

	// Refused, because it cannot be delivered.
	//
	// This answered {"delivered": true} into a channel nothing in this harness
	// ever reads. The buffer is what proves it: ten sends succeeded and the
	// eleventh failed with "inbox is full", so every one of the ten was still
	// sitting there and the child had been told nothing. A sub-agent runs
	// through SubAgentRunner — one prompt in, one result out — and there is no
	// seam anywhere for a message that arrives after it started.
	//
	// Delivering it for real is a design change (the runner would have to drain
	// the inbox between steps), not a wiring fix. Until something does, saying
	// so is the only honest answer: a caller told "delivered" stops repeating
	// the instruction and starts believing the child has it.
	mgr := r.getSubagentManager()
	known := "it is not a registered sub-agent"
	if _, ok := mgr.Get(args.Recipient); ok {
		known = "it is a registered sub-agent, but nothing delivers to a child mid-run"
	}
	return tools.Errorf(
		"nothing was delivered to %s: %s. This harness runs a sub-agent as one prompt in and one "+
			"result out, with no seam for an instruction that arrives after it started — a message "+
			"queued here would sit unread until the child finished. Put the instruction in the "+
			"child's prompt when you dispatch it, or wait for its result and dispatch a follow-up",
		args.Recipient, known)
}

func (r *Registry) manageSubagents(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Action          string   `json:"action"`
		ConversationIDs []string `json:"conversation_ids,omitempty"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}

	mgr := r.getSubagentManager()
	switch args.Action {
	case "list":
		// Snapshots, not the live instances. Marshalling *agents.Instance read
		// State and StateDetail with no lock while pool goroutines wrote them
		// through SetState — a data race the detector fires on for every list
		// issued during a fan-out, on a path a model reaches directly.
		instances := mgr.Snapshot()
		return ok(map[string]any{
			"count":     len(instances),
			"subagents": instances,
		})
	case "kill":
		if len(args.ConversationIDs) == 0 {
			return tools.Errorf("conversation_ids are required for 'kill' action")
		}
		// Every ID is accounted for. The error from Kill used to be dropped —
		// `if err := mgr.Kill(id); err == nil` — so an ID nobody had registered
		// simply vanished from the answer, and a caller reading {"killed":[]}
		// against a list it had just been given had no way to tell a child that
		// was stopped from one that was never there.
		killed := make([]string, 0, len(args.ConversationIDs))
		refused := make([]map[string]any, 0, len(args.ConversationIDs))
		for _, id := range args.ConversationIDs {
			if err := mgr.Kill(id); err != nil {
				refused = append(refused, map[string]any{
					"conversation_id": id,
					"error":           err.Error(),
				})
				continue
			}
			killed = append(killed, id)
		}
		payload := map[string]any{
			"requested": len(args.ConversationIDs),
			"killed":    killed,
		}
		if len(refused) > 0 {
			payload["not_terminated"] = refused
			return failure(payload, fmt.Sprintf(
				"%d of %d sub-agent(s) were not terminated; the ones named under not_terminated "+
					"are still whatever state they were in",
				len(refused), len(args.ConversationIDs)))
		}
		return ok(payload)
	case "kill_all":
		// The IDs are named, and an empty manager is an error. "all subagents
		// terminating" over a manager holding nothing is a claim about children
		// that do not exist, and it was returned unconditionally: the error
		// from KillAll was assigned to the blank identifier.
		killed, err := mgr.KillAll()
		if err != nil {
			return failure(map[string]any{
				"killed": killed,
				"count":  len(killed),
			}, err.Error())
		}
		return ok(map[string]any{
			"status": "terminating",
			"killed": killed,
			"count":  len(killed),
		})
	default:
		return tools.Errorf("unknown action %q (must be 'list', 'kill', or 'kill_all')", args.Action)
	}
}

// refuseDynamicRoles is subagents.dynamic.enabled, applied.
//
// This tool is the one place a model authors a role rather than choosing one:
// a name, a system prompt, a model placement and a tool surface, all decided
// inside the turn and none of them written down anywhere an operator reviewed.
// An operator who does not want that has to be able to say so, and until this
// check existed the setting was in the catalogue, reported its default through
// `manvi flags`, and changed nothing — which is worse than not offering it,
// because it reads as a control that is in force.
//
// Three things the refusal has to get right:
//
//   - It names the setting. A model told only "not allowed" rewords the request
//     and tries again; a model told which setting is off can report that to the
//     operator, who is the only one able to change it.
//   - It refuses before the definition is registered, not after. A message that
//     says no while the registry takes the role anyway leaves it invocable
//     under a name the transcript says was rejected.
//   - It covers a name that already exists. Registry.Register overwrites by
//     name, so "define a role" and "silently replace the shipped critic's
//     system prompt" are the same call, and gating only unfamiliar names would
//     leave the more damaging half open.
//
// What it does not touch is invocation. The built-in roles are the catalogue
// this harness ships with, an operator already has them, and taking them away
// too would make this setting mean "no delegation at all" — which is what
// agents.max_fanout is for.
func (r *Registry) refuseDynamicRoles() *tools.Result {
	if r.settingOn(flags.SubagentsDynamicEnabled, true) {
		return nil
	}
	available := make([]string, 0, 8)
	for _, def := range r.getSubagentRegistry().List() {
		available = append(available, def.Name)
	}
	// The roles are listed rather than the tool that dispatches them, and that
	// is not a stylistic choice: this package may not name a tool the core
	// profile does not offer, because a model narrowed to that profile would be
	// sent to call something it has not been given. Sub-agent dispatch is
	// Extended. See TestNoToolNamesAToolTheCoreProfileDoesNotOffer.
	res := tools.Errorf(
		"defining subagent types at runtime is off: %s=false. Nothing was registered. "+
			"The roles this harness already holds can still be dispatched by type_name (%s). "+
			"Use one of those, or ask the operator to change the setting",
		flags.SubagentsDynamicEnabled, strings.Join(available, ", "))
	// The session log records why a call was refused, and "a setting said no"
	// is a why. Without this the log holds an error string and nothing an
	// operator could group or count.
	res.Rule = flags.SubagentsDynamicEnabled
	return &res
}

// unimplementedWorkspace is the one explanation both refusals give, so a caller
// that hits it from either tool reads the same sentence.
const unimplementedWorkspace = "this harness has no workspace isolation — " +
	"there is no git-worktree or scratch-directory machinery anywhere in it, so every sub-agent " +
	"runs in the parent's working tree. Nothing was dispatched. Drop the field, or bound the " +
	"child's writes with a task scope instead"

// refuseRetiredRoleKeys refuses the two Definition keys that were removed
// rather than wired.
//
// Both were advertised by this tool's schema and decoded into fields nothing
// downstream ever read. Deleting the fields fixes the advertisement but not the
// silence: encoding/json discards an unknown key, so a model still sending
// `workspace` or `enable_subagent_tools` — from an older transcript, or from
// its own priors about what a sub-agent definition looks like — would be
// ignored exactly as before. A retired permission has to say it is retired
// once, out loud, or its removal is indistinguishable from the bug.
func refuseRetiredRoleKeys(call tools.Call) *tools.Result {
	var retired struct {
		Workspace           string `json:"workspace"`
		EnableSubagentTools *bool  `json:"enable_subagent_tools"`
	}
	if err := decode(call, &retired); err != nil {
		// Unreadable arguments are the caller's own decode error to report.
		return nil
	}
	if mode := strings.TrimSpace(retired.Workspace); mode != "" {
		res := tools.Errorf("workspace %q is not supported: %s", mode, unimplementedWorkspace)
		return &res
	}
	if retired.EnableSubagentTools != nil {
		res := tools.Errorf(
			"enable_subagent_tools is not supported: no sub-agent may dispatch sub-agents of its own. " +
				"The depth bound is structural rather than a counter — the dispatch tools are absent " +
				"from a child's tool registry — so there is nothing this flag could switch on at either " +
				"value. Define the role without it and fan out from here instead")
		return &res
	}
	return nil
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

// spawnRequest is one entry of devcouncil_spawn_subagents' tasks array.
type spawnRequest struct {
	Label    string `json:"label"`
	Prompt   string `json:"prompt"`
	TaskID   string `json:"task_id,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
	Type     string `json:"type,omitempty"`
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
		Tasks []spawnRequest `json:"tasks"`
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

	bounds, refusal := r.delegationBounds()
	if refusal != nil {
		return *refusal
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

	// The children's changed paths are carried onto this call's own result so
	// the parent turn's terminal checkpoint can see them. Without this hop the
	// paths stop at the child's log: the parent's checkpoint would be told the
	// turn mutated — a dispatch is a mutating tool — and handed no file to
	// look at, which is a check that runs against nothing and passes.
	return r.runFanout(ctx, bounds, fanout{
		placements: placements,
		tasks: func() []agents.Task {
			subTasks := make([]agents.Task, 0, len(args.Tasks))
			for _, t := range args.Tasks {
				subTasks = append(subTasks, r.spawnTask(t, roles, runner))
			}
			return subTasks
		},
		runFailure: "subagent fan-out failed",
		verb:       "delegated",
		outcome: func(outcome map[string]any, res agents.Result) {
			if len(res.ReleasedLeases) > 0 {
				outcome["released_leases"] = res.ReleasedLeases
			}
			if len(res.OrphanedLeases) > 0 {
				outcome["orphaned_leases"] = res.OrphanedLeases
			}
		},
	})
}

// spawnTask is one devcouncil_spawn_subagents child: it holds a lease on the
// task it names, if it names one, and runs under the role it names, if any.
func (r *Registry) spawnTask(t spawnRequest, roles *agents.Registry, runner SubAgentRunner) agents.Task {
	return agents.Task{
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
				return leftBehind(out), fmt.Errorf("sub-agent %s: %w", t.Label, err)
			}
			if err := requireSummary(t.Label, out.Summary); err != nil {
				return leftBehind(out), err
			}
			child := completedOutcome(out)
			child["task_id"] = t.TaskID
			return child, nil
		},
	}
}

// delegationBounds resolves the bounds a fan-out runs under, and refuses the
// dispatch outright when the depth bound allows no delegation at all.
//
// One reader for the delegation bounds, in the package that enforces them.
// Each dispatch tool used to carry its own copy of this block, and the copies
// had already drifted: one fell back to the literals 2 and 4, the other to
// four against a catalogue default of eight, so a run with no configuration
// used one pair of bounds and a run with configuration used another. Two
// readers of one setting is one reader too many when the setting is a bound on
// how much work a turn may spawn.
func (r *Registry) delegationBounds() (agents.Bounds, *tools.Result) {
	var reg *flags.Registry
	if r.deps.Gate != nil {
		reg = r.deps.Gate.Flags
	}
	bounds := agents.ResolveBounds(reg)

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
	if bounds.MaxDepth < 1 {
		// The reason, not the flag, when something other than the flag decided.
		// A refusal citing agents.max_spawn_depth=0 to an operator who set it
		// to 2 sends them to look at a setting that is not the cause.
		cause := fmt.Sprintf("%s=%d", flags.AgentsMaxSpawnDepth, bounds.MaxDepth)
		if bounds.DepthReason != "" {
			cause = bounds.DepthReason
		}
		res := tools.Errorf(
			"delegation is off: %s, so no sub-agent may be dispatched at any width. "+
				"Nothing was dispatched and no child ran — this is not a report of zero results. "+
				"Carry out the work in this turn.",
			cause)
		res.Rule = flags.AgentsMaxSpawnDepth
		return bounds, &res
	}
	return bounds, nil
}

// fanout is what one dispatch tool contributes to a fan-out: the children it
// built and where its result differs from the other tool's. Everything else —
// the width, the pool, the projection of each child's outcome, the summed
// spend and the merge of the children's writes — is runFanout's, once, so the
// two tools cannot drift apart again.
type fanout struct {
	// placements are where the children will run; see agents.FanoutFor.
	placements []string
	// tasks builds the children. It is called only once the pool exists, so
	// a fan-out the pool refuses builds nothing.
	tasks func() []agents.Task
	// runFailure introduces the error when the pool could not run at all.
	runFailure string
	// verb completes "nothing was %s successfully" when every child failed.
	verb string
	// outcome, when set, adds this tool's own fields to one child's outcome.
	outcome func(outcome map[string]any, res agents.Result)
	// payload, when set, adds this tool's own fields to the whole result.
	payload func(payload map[string]any)
}

// runFanout narrows the width to where the children will run, runs them on a
// bounded pool, and reports what each one produced.
func (r *Registry) runFanout(ctx context.Context, bounds agents.Bounds, f fanout) tools.Result {
	maxFanout, narrowed := agents.FanoutFor(bounds, f.placements)

	pool, err := agents.New(bounds.MaxDepth, maxFanout, r.deps.Store)
	if err != nil {
		return tools.Errorf("creating subagent pool: %v", err)
	}

	results, err := pool.Run(ctx, f.tasks())
	if err != nil {
		return tools.Errorf("%s: %v", f.runFailure, err)
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
			carryLeftBehind(outcome, res.Value)
		} else if value, isMap := res.Value.(map[string]any); isMap {
			for k, v := range value {
				outcome[k] = v
			}
		} else {
			// Unreachable with either tool's children, and still not reported
			// as a success: an outcome nobody can read is not evidence of work.
			// This branch was once absent from one of the two copies, so such a
			// child appeared in the results with a label and no status at all
			// -- neither a completion nor a failure.
			outcome["status"] = "failed"
			outcome["error"] = "the sub-agent returned an unreadable outcome"
		}
		if f.outcome != nil {
			f.outcome(outcome, res)
		}
		outcomes = append(outcomes, outcome)
	}

	// Both tools report the same shape. One of them used to carry neither the
	// report nor clean, so a caller could not tell a wholly failed dispatch
	// from a successful one at all.
	report := agents.Summarise(results)
	// Summed here as well as reported per child, because the dispatching model
	// is deciding whether to delegate again and the per-child figures make it
	// do the arithmetic. A failed child's spend is counted too (see
	// leftBehind): it spent what it spent. Absent rather than zero when
	// nothing was learned, because printing 0 would read as "delegation is
	// free".
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
	if f.payload != nil {
		f.payload(payload)
	}

	// Children changed paths on their own logs, and this is the only hop that
	// carries them to the parent's terminal checkpoint. See SubAgentResult.Wrote.
	return fanoutResult(payload, report, results, f.verb)
}

// completedOutcome is what a child that finished with a summary reports, in
// the fields both dispatch tools share. Each tool adds its own identifiers.
func completedOutcome(out SubAgentResult) map[string]any {
	outcome := map[string]any{
		"status":  "completed",
		"summary": out.Summary,
		"steps":   out.Steps,
		"usage":   out.Usage,
	}
	if len(out.Wrote) > 0 {
		outcome["wrote"] = out.Wrote
	}
	if out.WroteTruncated {
		outcome["wrote_truncated"] = true
	}
	if out.Verdict.Judged {
		// Carried structured rather than left in the prose. A judgement a
		// caller has to read out of a paragraph is a judgement a caller will
		// read wrong.
		outcome["verdict"] = out.Verdict.Reconcile()
	}
	return outcome
}

// fanoutResult is how both dispatch tools close.
//
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
//
// Either way the children's changed paths are carried onto the result, and a
// cut list says so. A child that wrote a file and then failed still changed
// the tree, and a checkpoint told otherwise would leave those bytes
// unexamined. The total-failure branch used to return before the truncation
// note, so a cut list arrived looking complete exactly when every child had
// failed; one exit for both outcomes is what keeps that from recurring.
func fanoutResult(payload map[string]any, report agents.Report, results []agents.Result, verb string) tools.Result {
	var res tools.Result
	if report.Children > 0 && report.Failed == report.Children {
		res = failure(payload, fmt.Sprintf(
			"all %d sub-agent(s) failed; nothing was %s successfully", report.Children, verb))
	} else {
		res = ok(payload)
	}
	wrote, truncated := childWrites(results)
	res.Wrote = wrote
	if truncated {
		res.Degraded = append(res.Degraded,
			"a sub-agent changed more paths than it reported; the list is incomplete")
	}
	return res
}

// leftBehind is what a child that did not complete still reports: the paths
// it changed and what it spent. The runner returns both on its failure paths,
// and agents.Pool keeps a task's value beside its error, so returning this with
// the failure is what carries a failed child's writes to the parent's
// checkpoint through childWrites. Returning nil instead dropped them: a child
// that wrote three files and then failed was reported upward as having changed
// nothing, and the parent verified a tree that did not include them.
func leftBehind(out SubAgentResult) any {
	v := map[string]any{}
	if len(out.Wrote) > 0 {
		v["wrote"] = out.Wrote
	}
	if out.WroteTruncated {
		v["wrote_truncated"] = true
	}
	if out.Usage.Any() {
		v["usage"] = out.Usage
	}
	if len(v) == 0 {
		return nil
	}
	return v
}

// carryLeftBehind puts a failed child's changed paths on its outcome, so the
// dispatching model reads that the failure still changed files.
func carryLeftBehind(outcome map[string]any, value any) {
	left, isMap := value.(map[string]any)
	if !isMap {
		return
	}
	for _, k := range []string{"wrote", "wrote_truncated"} {
		if v, ok := left[k]; ok {
			outcome[k] = v
		}
	}
}

// childWrites folds every child's changed paths into one fan-out set with
// tools.MergeWrites, so the set a dispatch reports upward is bounded by the
// same cap as the parent turn's set it lands in. A child that reported its own
// list as truncated keeps the fan-out's list marked truncated too.
func childWrites(results []agents.Result) ([]string, bool) {
	var wrote []string
	truncated := false
	for _, res := range results {
		value, isMap := res.Value.(map[string]any)
		if !isMap {
			continue
		}
		if paths, ok := value["wrote"].([]string); ok {
			wrote, truncated = tools.MergeWrites(wrote, truncated, paths)
		}
		if flag, ok := value["wrote_truncated"].(bool); ok && flag {
			truncated = true
		}
	}
	return wrote, truncated
}
