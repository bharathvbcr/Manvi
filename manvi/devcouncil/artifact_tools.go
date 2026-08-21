package devcouncil

import (
	"context"
	"path/filepath"
	"strings"

	"manvi/artifacts"
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

	store, err := r.getArtifactStore()
	if err != nil {
		return tools.Errorf("accessing artifact store: %v", err)
	}

	art, err := store.Create(args.Name, args.Content, args.Metadata)
	if err != nil {
		return tools.Errorf("creating artifact: %v", err)
	}

	return ok(map[string]any{
		"status":   "created",
		"artifact": art.Name,
		"revision": art.Revision,
		"path":     art.Path,
		"summary":  art.Metadata.Summary,
	})
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

	store, err := r.getArtifactStore()
	if err != nil {
		return tools.Errorf("accessing artifact store: %v", err)
	}

	art, err := store.Update(args.Name, args.Content, args.Metadata)
	if err != nil {
		return tools.Errorf("updating artifact: %v", err)
	}

	return ok(map[string]any{
		"status":   "updated",
		"artifact": art.Name,
		"revision": art.Revision,
		"path":     art.Path,
		"summary":  art.Metadata.Summary,
	})
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
