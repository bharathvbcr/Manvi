package devcouncil

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"manvi/agents"
	"manvi/flags"
	"manvi/tools"
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
// Production wires both (cmd/manvi/main.go), so this map stays empty there. It
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
// two dispatchers and the judgement drifted between them: this one lost the
// check entirely and reported "completed" for an empty result. The second copy
// still stands inline in the spawn_subagents fan-out in tools.go, which is
// owned elsewhere; it should call this instead of restating it.
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

func (r *Registry) invokeSubagents(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Subagents []struct {
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
		} `json:"subagents"`
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

	// One reader for the delegation bounds, in the package that enforces them.
	// This block used to be a verbatim copy of the one in spawn_subagents, with
	// its own hardcoded fallback of four against a catalogue default of eight.
	var flagReg *flags.Registry
	if r.deps.Gate != nil {
		flagReg = r.deps.Gate.Flags
	}
	bounds := agents.ResolveBounds(flagReg)
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
	maxFanout, fanoutNarrowed := agents.FanoutFor(bounds, placements)

	pool, err := agents.New(maxDepth, maxFanout, r.deps.Store)
	if err != nil {
		return tools.Errorf("creating subagent pool: %v", err)
	}

	var subTasks []agents.Task
	// unknownTypes names every type_name that is not a registered role. A typo
	// in a role name used to be invisible: the dispatch fell back to a
	// synthetic definition with EnableWriteTools true, so "critc" produced a
	// child with the parent's whole tool surface and write access, and the
	// result said nothing about it. The fallback is now read-only, and the
	// names are reported, so a caller can tell "the role I named" from "a role
	// this harness has never heard of".
	var unknownTypes []string

	for _, s := range args.Subagents {
		s := s
		def, ok := reg.Get(s.TypeName)
		// The surface is taken only from a role that actually exists. The
		// fallback below is synthetic — nobody wrote it down — so it declares
		// nothing and the child inherits the parent's tool set, which is the
		// same reading an unknown type already gets for its placement. Building
		// a surface out of the fallback's zero fields would instead read as a
		// role that deliberately denied the MCP group.
		var surface agents.ToolSurface
		if ok {
			surface = def.Surface()
		} else {
			unknownTypes = append(unknownTypes, s.TypeName)
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

		subTasks = append(subTasks, agents.Task{
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
					return nil, recordFailure(inst, err)
				}
				// The same judgement the other dispatcher makes, from the same
				// function rather than a second copy of it: a child that came
				// back with nothing to say is not a completion.
				if err := requireSummary(s.Role, out.Summary); err != nil {
					return nil, recordFailure(inst, err)
				}

				// Only now, with a result in hand, is "completed" claimed — and
				// only if the lifecycle still allows it. The one move this
				// refuses is completed-after-canceling, which is a child the
				// control plane terminated mid-flight; reporting that as
				// completed work is exactly the fabrication this file exists to
				// prevent.
				if err := inst.SetState(agents.StateCompleted, "done"); err != nil {
					return nil, fmt.Errorf(
						"sub-agent %q was terminated before it could report its work: %w", s.Role, err)
				}
				outcome := map[string]any{
					"conversation_id": convID,
					"status":          "completed",
					"role":            s.Role,
					"type":            s.TypeName,
					"summary":         out.Summary,
					"steps":           out.Steps,
					"usage":           out.Usage,
				}
				if len(out.Wrote) > 0 {
					outcome["wrote"] = out.Wrote
				}
				if out.WroteTruncated {
					outcome["wrote_truncated"] = true
				}
				if out.Verdict.Judged {
					// Carried structured rather than left in the prose. A
					// judgement a caller has to read out of a paragraph is a
					// judgement a caller will read wrong.
					outcome["verdict"] = out.Verdict.Reconcile()
				}
				if !ok {
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
		})
	}

	results, err := pool.Run(ctx, subTasks)
	if err != nil {
		return tools.Errorf("invoking subagents failed: %v", err)
	}

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
			// success: an outcome nobody can read is not evidence of work. This
			// branch was absent, so such a child appeared in the results with a
			// label and no status at all -- neither a completion nor a failure.
			outcome["status"] = "failed"
			outcome["error"] = "the sub-agent returned an unreadable outcome"
		}
		outcomes = append(outcomes, outcome)
	}

	// Same shape as the fan-out in tools.go, deliberately: this was the second
	// copy, and it carried neither the report nor clean, so a caller could not
	// tell a wholly failed dispatch from a successful one at all. "dispatched"
	// counts what was asked for, not what ran.
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
		"dispatched": len(args.Subagents),
		"report":     report,
		"clean":      report.Clean(),
		"results":    outcomes,
	}
	if spent.Any() {
		payload["usage"] = spent
	}
	if fanoutNarrowed != "" {
		payload["fanout_narrowed"] = fanoutNarrowed
	}
	if len(unknownTypes) > 0 {
		payload["unknown_types"] = unknownTypes
		payload["unknown_types_note"] = fmt.Sprintf(
			"these type_name values are not registered roles (%s). Each ran read-only on the "+
				"parent's tool surface, under no role's system prompt, model placement or tool "+
				"policy. Check the spelling against the registered roles, or define the role first",
			strings.Join(unknownTypes, ", "))
	}

	// Children changed paths on their own logs, and this is the only hop that
	// carries them to the parent's terminal checkpoint. See SubAgentResult.Wrote.
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

	if report.Children > 0 && report.Failed == report.Children {
		res := failure(payload, fmt.Sprintf(
			"all %d sub-agent(s) failed; nothing was dispatched successfully", report.Children))
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
