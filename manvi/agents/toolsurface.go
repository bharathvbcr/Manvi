package agents

import "manvi/tools"

// ToolSurface is the tool set a role declares for the child it describes.
//
// It exists because a Definition's tool fields used to be decoded, stored, and
// dropped. `enable_mcp_tools` and `allowed_tools` were part of the schema the
// model was shown when it defined a role, so a role written as "a critic that
// may not reach external MCP servers" produced a child that reached them
// anyway, and nothing anywhere said so. A permission that is advertised and not
// enforced is worse than no permission at all: it is a claim the operator
// believes.
//
// The type is carried rather than the Definition itself because the dispatch
// side (devcouncil) and the enforcement side (the runner, which is the only
// thing holding a tool registry) are deliberately separate packages. This is
// the narrow thing that crosses: what the role permits, with no opinion about
// how a registry is narrowed to match.
type ToolSurface struct {
	// Declared separates "a role said what this child may use" from "no role
	// was named at all".
	//
	// The distinction is load-bearing and cannot be recovered from the other
	// fields: a zero ToolSurface and a role with EnableMCPTools=false have the
	// same MCP value and must not have the same effect. An untyped fan-out, and
	// the synthetic definition invoke_subagent falls back to when a named type
	// is not registered, both inherit the parent's surface — the same rule
	// ModelSpec already follows, where empty means inherit rather than deny.
	Declared bool
	// MCP reports whether the role admits the MCP tool group. It is only
	// consulted when Declared.
	MCP bool
	// Allowed, when non-empty, is an allowlist of tool names.
	//
	// It only ever removes. It is intersected with every other rule the runner
	// applies — the structural absence of sub-agent dispatch, and the caller's
	// read-only floor — and is never unioned with them, so naming a tool here
	// cannot hand a child something those rules took away. An empty list is
	// "the role did not narrow by name", not "no tools".
	Allowed []string
	// Writes names mutating tools a read-only role may nonetheless use.
	//
	// SECURITY IMPACT, stated because this is the one field here that widens
	// rather than narrows. Everything else on this type can only take capability
	// away; this hands a child a tool the read-only floor would otherwise have
	// removed, so every entry is a permission that has to be justified on its
	// own and reviewed as one.
	//
	// It exists for a contradiction that shipped: the planner role's own system
	// prompt tells it to "draft structured plans under .devcouncil/artifacts/"
	// and the role sets EnableWriteTools false, so the artifact tools were
	// removed by the floor and the instruction could not be followed. That is
	// the same defect as a prompt naming a tool the model was not offered, in a
	// role definition instead of a section — and the honest options were to
	// stop claiming it or to make it true.
	//
	// Three properties keep the widening small. It is per-tool and by name, not
	// per-group: "the planner may write artifacts" is expressible and "the
	// planner may write" is not. It is intersected with the registry, so a name
	// that is not a registered tool grants nothing. And it does not touch the
	// policy gate: an artifact write still passes authoriseArtifactWrite like
	// any other, so this decides which door the child may knock on, never
	// whether the door opens.
	Writes []string
}

// PermitsWrite reports whether a role explicitly allows this mutating tool
// despite the read-only floor.
//
// Names are matched exactly. A prefix or glob here would be a permission whose
// scope changes whenever a tool is added, which is the opposite of reviewable.
func (s ToolSurface) PermitsWrite(name string) bool {
	if !s.Declared {
		return false
	}
	for _, allowed := range s.Writes {
		if allowed == name {
			return true
		}
	}
	return false
}

// Surface returns the tool surface this definition declares.
func (d Definition) Surface() ToolSurface {
	return ToolSurface{
		Declared: true,
		MCP:      d.EnableMCPTools,
		Allowed:  d.AllowedTools,
		Writes:   d.WriteExceptions,
	}
}

// Constrains reports whether this surface would narrow anything. A surface that
// constrains nothing lets a caller skip the narrowing work entirely, and — more
// importantly — skip reporting a narrowing that did not happen.
func (s ToolSurface) Constrains() bool {
	return s.Declared && (!s.MCP || len(s.Allowed) > 0 || len(s.Writes) > 0)
}

// Permits reports whether this surface admits one tool, named and grouped as
// the registry holds it.
//
// An undeclared surface permits everything: it is not a role saying "all
// tools", it is the absence of a role, and the caller's own rules still apply
// on top. Denial is by name and by group, never by the tool's own opinion of
// itself, so a tool that is added to the MCP group later is covered without
// this function changing.
func (s ToolSurface) Permits(name, group string) bool {
	if !s.Declared {
		return true
	}
	// The MCP group reaches servers outside this process. A role that did not
	// ask for it does not get it, which is the direction a permission that
	// cannot be verified has to fail.
	if group == tools.GroupMCP && !s.MCP {
		return false
	}
	if len(s.Allowed) == 0 {
		return true
	}
	for _, allowed := range s.Allowed {
		if allowed == name {
			return true
		}
	}
	return false
}
