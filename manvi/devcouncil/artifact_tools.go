package devcouncil

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"manvi/artifacts"
	"manvi/flags"
	"manvi/policy"
	"manvi/tools"
)

func (r *Registry) artifactTools() []tools.Tool {
	return []tools.Tool{
		{
			Schema: schema("devcouncil_create_artifact",
				"Create a persistent structured artifact (e.g. implementation plan, walkthrough, research notes, "+
					"architecture design document) under .devcouncil/artifacts/.",
				`{"type":"object","properties":{"name":{"type":"string","description":"artifact filename (e.g. implementation_plan.md)"},"content":{"type":"string","description":"artifact document contents"},"metadata":{"type":"object","properties":{"summary":{"type":"string"},"user_facing":{"type":"boolean"},"request_feedback":{"type":"boolean"}},"required":["summary"]}},"required":["name","content","metadata"]}`),
			Group:    tools.GroupArtifact,
			Handler:  r.createArtifact,
			Extended: true,
		},
		{
			Schema: schema("devcouncil_update_artifact",
				"Update an existing structured artifact's content and metadata, incrementing its revision.",
				`{"type":"object","properties":{"name":{"type":"string","description":"artifact filename (e.g. implementation_plan.md)"},"content":{"type":"string","description":"new artifact contents"},"metadata":{"type":"object","properties":{"summary":{"type":"string"},"user_facing":{"type":"boolean"},"request_feedback":{"type":"boolean"}}}},"required":["name","content"]}`),
			Group:    tools.GroupArtifact,
			Handler:  r.updateArtifact,
			Extended: true,
		},
		{
			Schema: schema("devcouncil_list_artifacts",
				"List all persistent artifacts currently recorded in .devcouncil/artifacts/.",
				`{"type":"object","properties":{}}`),
			Group:    tools.GroupArtifact,
			ReadOnly: true,
			Handler:  r.listArtifacts,
			Extended: true,
		},
	}
}

// artifactDir is the subtree every artifact lands in, relative to the
// repository root. Named here so the refusals below can point at it.
const artifactDir = ".devcouncil/artifacts"

// artifactStoreScope is what an artifact write admits about how it was judged:
// the scope rungs never ran on it. No task plans a file under
// `.devcouncil/artifacts/`, and none should have to, so the write is authorised
// by the store's confinement rather than by the plan — and an allow reached
// that way must not be indistinguishable from one the plan authorised.
const artifactStoreScope = "scope.artifact_store"

// authoriseArtifactWrite settles who may write an artifact and what the result
// has to admit about it.
//
// Neither create nor update consulted the gate or required a lease, and this
// harness has no PreExecute listeners — gating is per handler, so a handler
// that does not ask is simply ungated. Two tools ended up disagreeing about one
// directory: devcouncil_write_file refuses `.devcouncil/artifacts/note.md` as a
// hard rule that "no override clears, by any authority", while
// devcouncil_create_artifact wrote the file beside it in the same run, with no
// task checked out and nothing in the record to say so.
//
// The hard rule is not the thing to relax. `.devcouncil/` holds the task store,
// the config and the grant ledger, and a general-purpose write tool pointed
// into it reaches all of that — which is the whole of what that rung protects.
// What the artifact tools have and write_file does not is confinement: a name
// the store sanitises, under one subtree that holds nothing security-bearing.
// So neither tool changes what it may reach; the artifact tools start asking
// the two questions they were skipping.
//
// The first is the lease. An artifact is a record of work on a task, and one
// written with no task checked out is a record nothing can attribute — the same
// thing devcouncil_write_file refuses through policy.RuleNoTask. That rule is
// soft, so a gate mode demotes it, and it is demoted here for the same reason
// gate.settle demotes it there: an operator who set the file gate to advisory
// or off has said they do not want scope enforcement, and this is that same
// statement. Applied locally because an artifact is not a repository write and
// does not pass through EvaluateWrite; the mode still has to mean one thing.
//
// The second is the record, which is artifactStoreScope above.
func (r *Registry) authoriseArtifactWrite(ctx context.Context, verb string) (policy.Decision, *tools.Result) {
	task, refusal := r.authorisingTask(ctx, verb)
	if refusal != nil {
		return policy.Decision{}, refusal
	}
	if task != nil {
		return policy.Decision{
			Action: policy.Allow, Rule: policy.RuleNone, Severity: policy.None,
			Reason:   "The artifact store confines this write to " + artifactDir + ".",
			Target:   artifactDir,
			TaskID:   task.ID,
			Degraded: []string{artifactStoreScope},
		}, nil
	}

	d := policy.Decision{
		Action: policy.Deny, Rule: policy.RuleNoTask, Severity: policy.SeverityOf(policy.RuleNoTask),
		Reason: "No task is checked out, so an artifact written now records work nothing can attribute. " +
			"Call devcouncil_checkout_task first.",
		Target:   artifactDir,
		Degraded: []string{artifactStoreScope},
	}
	if r.deps.Gate == nil || r.deps.Gate.Flags == nil {
		// The mode could not be resolved, so it cannot have said "off". A
		// demotion that could not be checked must never resolve the same way as
		// one that was checked and applied.
		res := r.refusal(d)
		return d, &res
	}
	mode, origin, err := flags.EffectiveGateMode(r.deps.Gate.Flags, flags.PolicyFileMode)
	if err != nil {
		res := unavailable("the file gate mode", err)
		return d, &res
	}
	switch mode {
	case flags.ModeAdvisory, flags.ModeOff:
		d.Action = policy.Allow
		d.Demoted = fmt.Sprintf("%s=%s (%s)", flags.PolicyFileMode, mode, origin)
		return d, nil
	}
	res := r.refusal(d)
	return d, &res
}

func (r *Registry) getArtifactStore() (*artifacts.Store, error) {
	if r.deps.Artifacts != nil {
		return r.deps.Artifacts, nil
	}
	dir := filepath.Join(r.deps.Root, ".devcouncil", "artifacts")
	return artifacts.NewStore(dir)
}

func (r *Registry) createArtifact(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Name     string             `json:"name"`
		Content  string             `json:"content"`
		Metadata artifacts.Metadata `json:"metadata"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Name) == "" {
		return tools.Errorf("artifact name is required")
	}

	decision, refusal := r.authoriseArtifactWrite(ctx, "creating an artifact")
	if refusal != nil {
		return *refusal
	}

	store, err := r.getArtifactStore()
	if err != nil {
		return tools.Errorf("accessing artifact store: %v", err)
	}

	art, err := store.Create(args.Name, args.Content, args.Metadata)
	if err != nil {
		return tools.Errorf("creating artifact: %v", err)
	}

	// The artifact's path is reported like any other write. Whether an
	// end-of-turn check has anything useful to say about a plan document is
	// the checker's judgement, not this handler's: a handler that quietly
	// withheld paths it thought uninteresting would be deciding coverage by
	// omission, and the one thing downstream must never have to guess is
	// whether a short list is short because little was written or because
	// something filtered it.
	result := ok(map[string]any{
		"status":   "created",
		"artifact": art.Name,
		"revision": art.Revision,
		"path":     art.Path,
		"summary":  art.Metadata.Summary,
	})
	result.Wrote = []string{art.Path}
	return annotate(result, decision)
}

func (r *Registry) updateArtifact(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Name     string              `json:"name"`
		Content  string              `json:"content"`
		Metadata *artifacts.Metadata `json:"metadata,omitempty"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Name) == "" {
		return tools.Errorf("artifact name is required")
	}

	decision, refusal := r.authoriseArtifactWrite(ctx, "updating an artifact")
	if refusal != nil {
		return *refusal
	}

	store, err := r.getArtifactStore()
	if err != nil {
		return tools.Errorf("accessing artifact store: %v", err)
	}

	art, err := store.Update(args.Name, args.Content, args.Metadata)
	if err != nil {
		return tools.Errorf("updating artifact: %v", err)
	}

	// The artifact's path is reported like any other write. Whether an
	// end-of-turn check has anything useful to say about a plan document is
	// the checker's judgement, not this handler's: a handler that quietly
	// withheld paths it thought uninteresting would be deciding coverage by
	// omission, and the one thing downstream must never have to guess is
	// whether a short list is short because little was written or because
	// something filtered it.
	result := ok(map[string]any{
		"status":   "updated",
		"artifact": art.Name,
		"revision": art.Revision,
		"path":     art.Path,
		"summary":  art.Metadata.Summary,
	})
	result.Wrote = []string{art.Path}
	return annotate(result, decision)
}

func (r *Registry) listArtifacts(ctx context.Context, call tools.Call) tools.Result {
	store, err := r.getArtifactStore()
	if err != nil {
		return tools.Errorf("accessing artifact store: %v", err)
	}

	arts, err := store.List()
	if err != nil {
		return tools.Errorf("listing artifacts: %v", err)
	}

	var list []map[string]any
	for _, a := range arts {
		list = append(list, map[string]any{
			"name":        a.Name,
			"path":        a.Path,
			"revision":    a.Revision,
			"summary":     a.Metadata.Summary,
			"user_facing": a.Metadata.UserFacing,
			"updated_at":  a.UpdatedAt,
		})
	}

	return ok(map[string]any{
		"count":     len(list),
		"artifacts": list,
	})
}
