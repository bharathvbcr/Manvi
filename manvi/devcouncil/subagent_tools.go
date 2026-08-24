package devcouncil

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"manvi/agents"
	"manvi/flags"
	"manvi/tools"
)

var conversationCounter atomic.Int64

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
	return agents.NewRegistry()
}

func (r *Registry) getSubagentManager() *agents.InstanceManager {
	if r.deps.SubagentMgr != nil {
		return r.deps.SubagentMgr
	}
	return agents.NewInstanceManager()
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
		res := tools.Errorf(
			"delegation is off: %s=%d, so no sub-agent may be dispatched at any width. "+
				"Nothing was dispatched and no child ran — this is not a report of zero results. "+
				"Carry out the work in this turn, or ask the operator to change the setting",
			flags.AgentsMaxSpawnDepth, maxDepth)
		res.Rule = flags.AgentsMaxSpawnDepth
		return res
	}

	pool, err := agents.New(maxDepth, maxFanout, r.deps.Store)
	if err != nil {
		return tools.Errorf("creating subagent pool: %v", err)
	}

	var subTasks []agents.Task
	var dispatchedIDs []string

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
			// Auto-fallback to self
			def = agents.Definition{
				Name:             s.TypeName,
				Role:             s.Role,
				EnableWriteTools: true,
			}
		}

		convID := fmt.Sprintf("subagent-%d-%d", time.Now().UnixMilli(), conversationCounter.Add(1))
		dispatchedIDs = append(dispatchedIDs, convID)

		subTasks = append(subTasks, agents.Task{
			Label: s.Role,
			Run: func(childCtx context.Context, holder *agents.Holder) (any, error) {
				childCtx, cancel := context.WithCancel(childCtx)
				defer cancel()

				inst := &agents.Instance{
					ConversationID: convID,
					Type:           s.TypeName,
					Role:           s.Role,
					State:          agents.StateRunning,
				}
				mgr.Register(inst)

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
					inst.SetState(agents.StateErrored, "no sub-agent runner is attached")
					return nil, fmt.Errorf(
						"sub-agent %q cannot run: no model is attached to this invocation", s.Role)
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
					inst.SetState(agents.StateErrored, err.Error())
					return nil, err
				}

				inst.SetState(agents.StateCompleted, "done")
				return map[string]any{
					"conversation_id": convID,
					"status":          "completed",
					"role":            s.Role,
					"type":            s.TypeName,
					"summary":         out.Summary,
					"steps":           out.Steps,
					"usage":           out.Usage,
				}, nil
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
	if report.Children > 0 && report.Failed == report.Children {
		return failure(payload, fmt.Sprintf(
			"all %d sub-agent(s) failed; nothing was dispatched successfully", report.Children))
	}
	return ok(payload)
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

	mgr := r.getSubagentManager()
	if err := mgr.SendMessage(args.Recipient, args.Message); err != nil {
		return tools.Errorf("delivering message: %v", err)
	}

	return ok(map[string]any{
		"delivered": true,
		"recipient": args.Recipient,
	})
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
		instances := mgr.List()
		return ok(map[string]any{
			"count":     len(instances),
			"subagents": instances,
		})
	case "kill":
		if len(args.ConversationIDs) == 0 {
			return tools.Errorf("conversation_ids are required for 'kill' action")
		}
		var killed []string
		for _, id := range args.ConversationIDs {
			if err := mgr.Kill(id); err == nil {
				killed = append(killed, id)
			}
		}
		return ok(map[string]any{
			"killed": killed,
		})
	case "kill_all":
		_ = mgr.KillAll()
		return ok(map[string]any{
			"status": "all subagents terminating",
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
