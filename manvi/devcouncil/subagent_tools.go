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
				`{"type":"object","properties":{"name":{"type":"string","description":"unique name for subagent type"},"role":{"type":"string","description":"role description"},"description":{"type":"string","description":"human-readable description"},"system_prompt":{"type":"string","description":"detailed system prompt"},"model":{"type":"string","description":"a tier, not a model id: basic, lite or heavy, at or below the role's ceiling (heavy when enable_write_tools is true, lite otherwise); model ids and providers are refused"},"tier":{"type":"string","description":"the dearest tier this role may run on: basic, lite or heavy, at or below its ceiling (heavy when enable_write_tools is true, lite otherwise); omit to take the ceiling"},"enable_mcp_tools":{"type":"boolean","description":"admit the mcp tool group; false takes it away from the child"},"enable_write_tools":{"type":"boolean","description":"false makes the child read-only"},"allowed_tools":{"type":"array","items":{"type":"string"},"description":"allowlist of tool names; it only ever removes, never widens what enable_write_tools and enable_mcp_tools permit"}},"required":["name","description","system_prompt"]}`),
			Group:    tools.GroupSubagent,
			Handler:  r.defineSubagent,
			Extended: true,
		},
		{
			Schema: schema("devcouncil_invoke_subagent",
				"Invoke one or more subagents concurrently by name with custom task prompts, returning their unique conversation IDs and execution outcomes.",
				`{"type":"object","properties":{"subagents":{"type":"array","items":{"type":"object","properties":{"type_name":{"type":"string","description":"subagent type name"},"role":{"type":"string","description":"specific role description"},"prompt":{"type":"string","description":"actionable task description"},"model":{"type":"string","description":"optional tier for this child (basic, lite, heavy, advisor), at or below its role's tier; model ids and providers are refused"}},"required":["type_name","role","prompt"]}}},"required":["subagents"]}`),
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

// Roles is the role catalogue this surface dispatches from, so a dispatcher
// outside this package — the end-of-turn critic — runs a role exactly as these
// tools would rather than describing one of its own.
func (r *Registry) Roles() *agents.Registry { return r.getSubagentRegistry() }

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

	// Where a model-authored role runs is a tier, never a model id. See
	// authoredTier.
	tier, err := authoredTier(def)
	if err != nil {
		return tools.Errorf("%v. Nothing was registered", err)
	}
	def.Model = agents.InheritSpec
	def.Tier = tier

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
		"tier":        def.Ceiling(),
	})
}

// authoredTier reads the placement of a role a model is defining, from either
// of the two keys that can carry it, and refuses anything that is not a tier
// within the role's permissions.
//
// A model used to be able to write any string into a role's model — including
// an id billed at several times the rate of anything the operator chose — and
// every later dispatch of that role went there. The operator's own roles may
// still name raw models: they are registered in-process, through
// agents.Registry.Register, and never pass through this tool.
//
// The ceiling is the one the role's permissions earn, not one the model
// declares, so a model cannot raise its own limit by asking for it.
func authoredTier(def agents.Definition) (agents.Tier, error) {
	ceiling := def.PermissionCeiling()
	fromModel, err := agents.ModelPlacement(def.Model, ceiling)
	if err != nil {
		return "", fmt.Errorf("model: %w", err)
	}
	fromTier, err := agents.ModelPlacement(string(def.Tier), ceiling)
	if err != nil {
		return "", fmt.Errorf("tier: %w", err)
	}
	if fromModel != "" && fromTier != "" && fromModel != fromTier {
		return "", fmt.Errorf("model %q and tier %q disagree; name the tier once", fromModel, fromTier)
	}
	if fromTier != "" {
		return fromTier, nil
	}
	return fromModel, nil
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

	// A per-call model is the dispatching model choosing a placement, so it
	// may name only a tier, and only one at or below the role's ceiling.
	// Checked for every child before any runs: a fan-out half of which was
	// refused would report the other half as the dispatch the caller asked
	// for.
	tiers := make([]agents.Tier, len(args.Subagents))
	roles := make([]resolvedRole, len(args.Subagents))
	for i, s := range args.Subagents {
		roles[i] = resolveRole(reg, s.TypeName, s.Role, false, true)
		def := roles[i].Def
		tier, err := agents.ModelPlacement(s.Model, def.Ceiling())
		if err != nil {
			return tools.Errorf("subagents[%d] (%s, type %q): model: %v. Nothing was dispatched",
				i, s.Role, s.TypeName, err)
		}
		// An operator who pinned a role to a model decided where it runs; a
		// tier named in the call cannot move it. Refused rather than ignored,
		// because a field that is accepted and has no effect tells the model
		// it has control it does not have.
		if tier != "" && !agents.ParsePlacement(def.Model, nil).Inherits() {
			return tools.Errorf("subagents[%d] (%s, type %q): model: the operator placed this role on %q, "+
				"and a per-call tier cannot move it; omit model. Nothing was dispatched",
				i, s.Role, s.TypeName, def.Model)
		}
		tiers[i] = tier
	}

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
		if role := resolveRole(reg, sub.TypeName, sub.Role, false, true); role.Known {
			placements = append(placements, role.Def.Model)
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
	requests := make([]SubAgentRequest, 0, len(args.Subagents))
	for i, s := range args.Subagents {
		if roles[i].Unknown {
			unknownTypes = append(unknownTypes, s.TypeName)
		}
		requests = append(requests, invokeChildRequest(s, roles[i], tiers[i], len(args.Subagents)))
	}

	return r.runFanout(ctx, bounds, fanout{
		placements: placements,
		runner:     runner,
		requests:   requests,
		task: func(i int, req SubAgentRequest) agents.Task {
			return invokeTask(args.Subagents[i], roles[i], req, mgr, runner)
		},
		runFailure: "invoking subagents failed",
		verb:       "dispatched",
		payload: func(payload map[string]any) {
			// "dispatched" counts what was asked for, not what ran.
			payload["dispatched"] = len(args.Subagents)
			reportUnknownRoles(payload, unknownTypes)
		},
	})
}

// ApplyRole puts what a registered role decides onto a request, for a dispatcher
// that holds the role's definition rather than a name to resolve: the end-of-turn
// critic. It is resolveRole's policy for a role that exists, so every dispatcher
// of a role reaches the same answer.
func ApplyRole(req *SubAgentRequest, def agents.Definition, callerReadOnly bool) {
	knownRole(def.Name, def, callerReadOnly).apply(req)
}

// invokeChildRequest is what one devcouncil_invoke_subagent child is asked to
// do, under the role resolveRole made of s's type. tier is the caller's own
// placement, already checked against the role's ceiling, or empty; width is how
// many children this dispatch holds. Built before the fan-out runs so the
// runner can admit every child against a spend cap before any starts.
func invokeChildRequest(s invokeRequest, role resolvedRole, tier agents.Tier, width int) SubAgentRequest {
	req := SubAgentRequest{Label: s.Role, Prompt: s.Prompt}
	role.apply(&req)
	// The router decides the tier from the role and the dispatch; a caller's
	// own placement, already held to the role's ceiling, replaces it.
	req.Tier = agents.Route(role.routeDef(), req.ReadOnly, false, width)
	if tier != "" {
		req.Tier = tier
	}

	return req
}

// invokeTask is one devcouncil_invoke_subagent child: a tracked instance the
// control plane can list and kill, running req under the role s names, as
// resolved by resolveRole.
func invokeTask(s invokeRequest, role resolvedRole, req SubAgentRequest,
	mgr *agents.InstanceManager, runner SubAgentRunner) agents.Task {
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

			// So a lease this child takes for itself is released by the same
			// cleanup that releases one the dispatch named.
			req := req
			req.Leases = leaseSinkFor(holder)
			out, err := runner.RunSubAgent(childCtx, req)
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
				// Errored, not left in canceling: nothing else moves a
				// canceling child on, so it would stay listed as terminating
				// forever and a later kill would "terminate" it again.
				return leftBehind(out), recordFailure(inst, fmt.Errorf(
					"sub-agent %q was terminated before it could report its work: %w", s.Role, err))
			}
			outcome := completedOutcome(out)
			outcome["conversation_id"] = convID
			outcome["role"] = s.Role
			outcome["type"] = s.TypeName
			role.annotate(outcome)
			return outcome, nil
		},
	}
}

// resolvedRole is what a dispatch tool's role name came to. It is the one
// reader of the role registry for both dispatchers, so what a name means — and
// what an unregistered one is refused — cannot differ between them.
type resolvedRole struct {
	// Name is the type the caller wrote; empty when it named none.
	Name string
	// Named reports that the caller named a role at all.
	Named bool
	// Known reports that the named role is registered; Def is meaningful only
	// then, or for the synthetic fallback below.
	Known bool
	// Unknown reports that a role was named and is not registered.
	Unknown bool
	Def     agents.Definition
	// ReadOnly is what the child runs under. Surface is the role's tool
	// surface, declared only by a role somebody actually wrote.
	ReadOnly bool
	Surface  agents.ToolSurface
}

// resolveRole resolves a dispatch's role name against the registry.
//
// A name that is not registered fails closed: a permission nobody wrote down is
// a permission nobody reviewed, so the child is read-only, declares no surface
// (it inherits the parent's, the same reading an unknown type already gets for
// its placement) and the miss is reported through reportUnknownRoles and
// annotate rather than left for the dispatching model to infer. A caller that
// names no role at all keeps the placement the tool has always had, with
// callerReadOnly as its only constraint, unless roleRequired says the tool has
// no such reading (invoke_subagent always runs under a role, so an empty name
// there is a miss like any other). Where a role is found, callerReadOnly
// is a floor and never a ceiling: a caller that asked for a non-mutating child
// is not handed a writing one because the role permits writes, and the role's
// named write exceptions do not apply to it.
func resolveRole(roles *agents.Registry, typeName, label string, callerReadOnly, roleRequired bool) resolvedRole {
	role := resolvedRole{Name: typeName, Named: typeName != "" || roleRequired, ReadOnly: callerReadOnly}
	if !role.Named {
		return role
	}
	def, ok := roles.Get(typeName)
	if !ok {
		role.Unknown = true
		role.ReadOnly = true
		role.Def = agents.Definition{Name: typeName, Role: label, EnableWriteTools: false}
		return role
	}
	return knownRole(typeName, def, callerReadOnly)
}

// knownRole is the policy for a role that exists, in one place because every
// dispatcher of a role reaches it: resolveRole for both dispatch tools and
// ApplyRole for the end-of-turn critic. callerReadOnly is a floor, never a
// ceiling; the role's own surface is taken only from a role somebody wrote.
func knownRole(name string, def agents.Definition, callerReadOnly bool) resolvedRole {
	role := resolvedRole{
		Name: name, Named: true, Known: true, Def: def, Surface: def.Surface(),
		ReadOnly: callerReadOnly || !def.EnableWriteTools,
	}
	if callerReadOnly {
		role.Surface.Writes = nil
	}
	return role
}

// apply puts the role's placement, prompt, surface and write permission onto a
// request. A caller that named no role leaves the request as it was.
func (r resolvedRole) apply(req *SubAgentRequest) {
	if !r.Named {
		return
	}
	req.ModelSpec = r.Def.Model
	req.SystemPrompt = r.Def.SystemPrompt
	req.Surface = r.Surface
	req.ReadOnly = r.ReadOnly
}

// routeDef is the definition the router reads: the role's own, or for a caller
// that named none a generalist that may write unless the caller said otherwise.
// An unregistered role's synthetic definition states no tier and no writes, so
// it is capped at lite.
func (r resolvedRole) routeDef() agents.Definition {
	if !r.Named {
		return agents.Definition{EnableWriteTools: true}
	}
	return r.Def
}

// annotate marks a child's own outcome when its role was not registered. It is
// carried here as well as on the payload because this is the line a dispatching
// model reads when it is deciding whether the answer it got came from the role
// it asked for.
func (r resolvedRole) annotate(outcome map[string]any) {
	if !r.Unknown {
		return
	}
	outcome["unknown_type"] = true
	outcome["note"] = fmt.Sprintf(
		"no role named %q is registered; this child ran read-only on the parent's "+
			"tool surface rather than under that role", r.Name)
}

// reportUnknownRoles adds the unregistered role names of a dispatch to its
// payload, so a typo is visible on the result as well as on each child.
func reportUnknownRoles(payload map[string]any, unknownTypes []string) {
	if len(unknownTypes) == 0 {
		return
	}
	payload["unknown_types"] = unknownTypes
	payload["unknown_types_note"] = fmt.Sprintf(
		"these role type values are not registered roles (%s). Each ran read-only on the "+
			"parent's tool surface, under no role's system prompt, model placement or tool "+
			"policy. Check the spelling against the registered roles, or define the role first",
		strings.Join(unknownTypes, ", "))
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
	// Tier is the tier the child is to run on, decided by agents.Route from
	// the role and the dispatch, or named by the dispatching model within the
	// role's ceiling. The runner resolves it against the session's tier
	// profile; an empty tier, or one the profile does not hold, inherits the
	// parent's model. A ModelSpec that names a placement wins over it, because
	// only an operator can write one.
	Tier agents.Tier
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
	// Escalation is the fan-out's shared cap on re-runs onto a higher tier,
	// set by the dispatcher. Nil means the child may not escalate.
	Escalation *EscalationBudget
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
	// Escalations lists each move up a tier this child made, so the run report
	// names it. Empty for a child that finished where it started.
	Escalations []Escalation
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

// SubAgentUsage is one child's token cost, and where it was spent.
//
// Declared here in plain integers rather than as llm.Usage so this package,
// which describes what a delegated turn owes its caller, does not take on the
// provider layer as a dependency.
type SubAgentUsage struct {
	// Model is where the child ran, as "provider/model".
	//
	// Carried because a sum of tokens cannot be turned into money once the
	// children of one fan-out run on different models: a thousand tokens on a
	// frontier model and a thousand on a small one are not the same bill, and
	// a total that has forgotten which was which cannot be priced afterwards.
	Model           string `json:"model,omitempty"`
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	ReasoningTokens int    `json:"reasoning_tokens,omitempty"`
	CacheReadTokens int    `json:"cache_read_tokens,omitempty"`
	// CacheWriteTokens is the part of the prompt written to the provider's
	// cache, which is billed above a fresh input token. It was dropped, so a
	// child that primed a cache reported the dearest part of its prompt as
	// though it had cost the same as the rest.
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	// CostNanoUSD is what the child's priced steps cost, each step priced from
	// its model's own table (budget.Prices) as its response arrived.
	CostNanoUSD int64 `json:"cost_nano_usd,omitempty"`
	// UnpricedSteps counts the child's steps that could not be priced — a
	// model with no price table, or usage the table refused. When it is not
	// zero, CostNanoUSD is a lower bound and must not be read as the bill.
	UnpricedSteps int `json:"unpriced_steps,omitempty"`
}

// Add accumulates another child's cost. Model is left as it was: a sum over
// children on different models belongs to none of them, and the run's meter,
// which does need the split, keeps one total per model instead.
func (u *SubAgentUsage) Add(o SubAgentUsage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.CostNanoUSD += o.CostNanoUSD
	u.UnpricedSteps += o.UnpricedSteps
}

// Any reports whether anything was spent, so a fan-out that learned nothing
// about its children's cost says nothing rather than reporting zero.
func (u SubAgentUsage) Any() bool {
	return u.InputTokens > 0 || u.OutputTokens > 0 || u.ReasoningTokens > 0 || u.CacheReadTokens > 0 ||
		u.CacheWriteTokens > 0 || u.CostNanoUSD > 0 || u.UnpricedSteps > 0
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

// SubAgentAdmitter is implemented by a runner whose children draw on a spend
// cap, so a fan-out can be admitted before any child starts.
//
// It is optional because only the runner knows where each child will run and
// what its first request will look like, and only a runner whose provider is
// metered has a cap to admit against. A runner without it dispatches as every
// runner did before: all at once, with no cap consulted.
type SubAgentAdmitter interface {
	// AdmitSubAgents names the cap the children draw on and what each one's
	// first request is expected to reserve against it, in the order given. A
	// child the cap does not meter reserves zero; a nil Budget means none of
	// them is metered. An error refuses the whole fan-out.
	AdmitSubAgents(reqs []SubAgentRequest) (agents.Budget, []int64, error)
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
	rolesOf := make([]resolvedRole, len(args.Tasks))
	placements := make([]string, 0, len(args.Tasks))
	for i, t := range args.Tasks {
		rolesOf[i] = resolveRole(roles, t.Type, t.Label, t.ReadOnly, false)
		if rolesOf[i].Known {
			placements = append(placements, rolesOf[i].Def.Model)
		}
	}

	// The children's changed paths are carried onto this call's own result so
	// the parent turn's terminal checkpoint can see them. Without this hop the
	// paths stop at the child's log: the parent's checkpoint would be told the
	// turn mutated — a dispatch is a mutating tool — and handed no file to
	// look at, which is a check that runs against nothing and passes.
	var unknownTypes []string
	requests := make([]SubAgentRequest, 0, len(args.Tasks))
	for _, role := range rolesOf {
		if role.Unknown {
			unknownTypes = append(unknownTypes, role.Name)
		}
	}
	for i, t := range args.Tasks {
		requests = append(requests, spawnChildRequest(t, rolesOf[i], len(args.Tasks)))
	}
	return r.runFanout(ctx, bounds, fanout{
		placements: placements,
		runner:     runner,
		requests:   requests,
		task: func(i int, req SubAgentRequest) agents.Task {
			return r.spawnTask(args.Tasks[i], req, rolesOf[i], runner)
		},
		payload:    func(payload map[string]any) { reportUnknownRoles(payload, unknownTypes) },
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

// spawnChildRequest is what one devcouncil_spawn_subagents child is asked to
// do, under the role resolveRole made of its type, if it named one. width is how
// many children this dispatch holds, which the router reads. Built before the
// fan-out runs so the runner can admit every child against a spend cap before
// any of them starts.
func spawnChildRequest(t spawnRequest, role resolvedRole, width int) SubAgentRequest {
	// A task may name a role, which is what lets one fan-out put a planner on
	// one model and its workers on another. A task that names none keeps the
	// placement this tool has always had: the parent's.
	req := SubAgentRequest{Label: t.Label, Prompt: t.Prompt, TaskID: t.TaskID, ReadOnly: t.ReadOnly}
	role.apply(&req)
	req.Tier = agents.Route(role.routeDef(), req.ReadOnly, false, width)
	return req
}

// spawnTask is one devcouncil_spawn_subagents child: it holds a lease on the
// task it names, if it names one, and runs req under the role it was resolved to.
func (r *Registry) spawnTask(t spawnRequest, req SubAgentRequest, role resolvedRole, runner SubAgentRunner) agents.Task {
	return agents.Task{
		Label: t.Label,
		Run: func(childCtx context.Context, holder *agents.Holder) (any, error) {
			if t.TaskID != "" {
				acquire := store.AcquireRequest{
					TaskID: t.TaskID,
					Owner:  t.Label,
					TTL:    r.deps.LeaseTTL,
				}
				lease, err := r.deps.Store.Acquire(childCtx, acquire)
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

			// So a lease this child takes for itself is released by the same
			// cleanup that releases one the dispatch named.
			req := req
			req.Leases = leaseSinkFor(holder)

			out, err := runner.RunSubAgent(childCtx, req)
			if err != nil {
				return leftBehind(out), fmt.Errorf("sub-agent %s: %w", t.Label, err)
			}
			if err := requireSummary(t.Label, out.Summary); err != nil {
				return leftBehind(out), err
			}
			child := completedOutcome(out)
			child["task_id"] = t.TaskID
			role.annotate(child)
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
	// requests are what each child is asked to do, in dispatch order. They
	// exist before any child does, so the runner can admit the whole fan-out
	// against a spend cap first.
	requests []SubAgentRequest
	// runner runs the children; when it is a SubAgentAdmitter, the fan-out is
	// admitted against its cap. Nil is allowed: each child then reports that
	// no runner is attached.
	runner SubAgentRunner
	// task builds child i from its request. It is called only once the pool
	// exists, so a fan-out the pool refuses builds nothing.
	task func(i int, req SubAgentRequest) agents.Task
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

	// One cap for the whole dispatch, on every child's request before the
	// requests are copied into tasks or sized for admission, so all of them
	// draw on the same counter.
	escalation := NewEscalationBudget(len(f.requests))
	for i := range f.requests {
		f.requests[i].Escalation = escalation
	}
	tasks := make([]agents.Task, 0, len(f.requests))
	for i, req := range f.requests {
		tasks = append(tasks, f.task(i, req))
	}
	if admitter, ok := f.runner.(SubAgentAdmitter); ok {
		budget, reserves, err := admitter.AdmitSubAgents(f.requests)
		if err != nil {
			return tools.Errorf("%s: the children could not be admitted against the spend cap, so none was started: %v",
				f.runFailure, err)
		}
		if budget != nil {
			if len(reserves) != len(tasks) {
				return tools.Errorf("%s: the runner sized %d of %d children's first requests, so none was started",
					f.runFailure, len(reserves), len(tasks))
			}
			pool.Budget = budget
			for i := range tasks {
				tasks[i].Reserve = reserves[i]
			}
		}
	}

	results, err := pool.Run(ctx, tasks)
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
		if res.NotStarted != "" {
			// Never run, so there is nothing left behind to carry: no paths,
			// no spend, no leases. Reported apart from "failed" because the
			// difference decides what the dispatching model should do next —
			// a failed child's work may be half done, a child that never
			// started has done none.
			outcome["status"] = "not_started"
			outcome["reason"] = res.NotStarted
			outcome["error"] = res.Error
			outcomes = append(outcomes, outcome)
			continue
		}
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
	var escalations []Escalation
	for _, res := range results {
		if value, isMap := res.Value.(map[string]any); isMap {
			if u, ok := value["usage"].(SubAgentUsage); ok {
				spent.Add(u)
			}
			if e, ok := value["escalations"].([]Escalation); ok {
				escalations = append(escalations, e...)
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
	if len(escalations) > 0 {
		// Named in the report, with the cap it drew on: an operator reading a
		// bill that includes a dearer model needs to find which child moved
		// there and on what evidence.
		payload["escalations"] = escalations
		payload["escalation_cap"] = escalation.Cap()
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
	if len(out.Escalations) > 0 {
		outcome["escalations"] = out.Escalations
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
	switch {
	case report.Children > 0 && report.NotStarted > 0 && report.Failed+report.NotStarted == report.Children:
		started := "none was started"
		if report.Failed > 0 {
			started = fmt.Sprintf("the %d that started failed", report.Failed)
		}
		res = failure(payload, fmt.Sprintf(
			"%d sub-agent(s) were not started because the spend cap could not cover them, and %s; "+
				"nothing was %s successfully", report.NotStarted, started, verb))
	case report.Children > 0 && report.Failed == report.Children:
		res = failure(payload, fmt.Sprintf(
			"all %d sub-agent(s) failed; nothing was %s successfully", report.Children, verb))
	default:
		res = ok(payload)
	}
	if report.NotStarted > 0 {
		// Said on the result as well as per child, so a partly admitted
		// fan-out is never read as a whole one.
		res.Degraded = append(res.Degraded, fmt.Sprintf(
			"%d of %d sub-agent(s) were not started: the spend cap could not cover them",
			report.NotStarted, report.Children))
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
	if len(out.Escalations) > 0 {
		v["escalations"] = out.Escalations
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
	for _, k := range []string{"wrote", "wrote_truncated", "escalations"} {
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
