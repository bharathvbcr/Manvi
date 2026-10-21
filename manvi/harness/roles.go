package harness

import (
	"fmt"
	"slices"
	"sync"

	"github.com/bharathvbcr/Manvi/manvi/agents"
)

// Surface is what the session itself holds: the tools its registry offers and
// whether any of them mutates. A spec's roles are judged against it and can
// only be narrower.
type Surface struct {
	// Tools are the session's tool names.
	Tools []string
	// Writes reports whether the session has any tool that mutates.
	Writes bool
}

// Definition converts a role into the registry's own type, no wider than the
// session.
//
//   - EnableWriteTools is the session's own write capability, lowered by
//     ReadOnly. It is never raised: a role on a read-only session is read-only.
//   - AllowedTools is intersected with the session's tools; the names dropped
//     for not being session tools are returned so the caller can say so. When a
//     role listed tools and none survive, the role is refused rather than
//     registered, because an empty allowlist means "no narrowing by name" and
//     that would turn a restriction into the absence of one.
//   - MCP access, write exceptions, tier and model are never set: the zero
//     values are the narrowest, and the registry derives the tier from them.
func (r RoleSpec) Definition(sf Surface) (def agents.Definition, dropped []string, err error) {
	def = agents.Definition{
		Name:             r.Name,
		Role:             r.Name,
		Description:      r.Description,
		SystemPrompt:     r.SystemPrompt,
		EnableWriteTools: sf.Writes && !r.ReadOnly,
	}
	for _, t := range r.AllowedTools {
		if slices.Contains(sf.Tools, t) {
			def.AllowedTools = append(def.AllowedTools, t)
		} else {
			dropped = append(dropped, t)
		}
	}
	if len(r.AllowedTools) > 0 && len(def.AllowedTools) == 0 {
		return agents.Definition{}, dropped, fmt.Errorf("none of its allowed_tools (%v) is a tool this session has, and an empty list would leave it unrestricted", dropped)
	}
	return def, dropped, nil
}

// Roles tracks the roles a spec registered, so the next spec can replace them
// and nothing else is ever touched. The zero value is ready.
type Roles struct {
	mu   sync.Mutex
	mine map[string]bool
}

// Apply registers the spec's roles on reg and removes the ones a previous Apply
// registered that the new list does not carry. It returns a notice for each
// role it did not register and each tool it narrowed away; it never returns an
// error, because a role that cannot be registered must not stop the turn, and
// it must not be quiet either.
//
// A role whose name is already held by something Apply did not register — a
// shipped role, or one a model defined — is refused: a spec does not overwrite.
func (t *Roles) Apply(reg *agents.Registry, roles []RoleSpec, sf Surface) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var notices []string
	next := map[string]bool{}
	for _, r := range roles {
		def, dropped, err := r.Definition(sf)
		if err != nil {
			notices = append(notices, fmt.Sprintf("harness role %q was not registered: %v", r.Name, err))
			continue
		}
		if _, held := reg.Get(r.Name); held && !t.mine[r.Name] {
			notices = append(notices, fmt.Sprintf("harness role %q was not registered: the name is already held by another role, and a spec does not overwrite", r.Name))
			continue
		}
		if err := reg.Register(def); err != nil {
			notices = append(notices, fmt.Sprintf("harness role %q was not registered: %v", r.Name, err))
			continue
		}
		next[r.Name] = true
		if len(dropped) > 0 {
			notices = append(notices, fmt.Sprintf("harness role %q: tools %v are not tools this session has and were dropped from its allowlist", r.Name, dropped))
		}
	}
	for name := range t.mine {
		if next[name] {
			continue
		}
		if err := reg.Unregister(name); err != nil {
			notices = append(notices, fmt.Sprintf("harness role %q could not be removed: %v", name, err))
		}
	}
	t.mine = next
	return notices
}
