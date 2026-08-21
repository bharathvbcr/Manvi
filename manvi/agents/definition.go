package agents

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Subagent Lifecycle States.
const (
	StateRunning           = "running"
	StateIdle              = "idle"
	StateWaitingForInput   = "waiting_for_input"
	StateWaitingForMessage = "waiting_for_message"
	StateCanceling         = "canceling"
	StateCompleted         = "completed"
	StateErrored           = "errored"
)

// Definition defines a dynamic subagent type.
//
// Every field here is honoured somewhere. That is a property worth stating,
// because it did not hold: this struct once carried a Workspace mode, a
// sub-agent-tools permission, an MCP toggle and an allowlist, all decoded from
// the schema the model was shown and none of them read by anything downstream.
// A role could be written with a tool policy and a workspace and get neither,
// with no error and nothing in the transcript to say so.
//
// Two of those fields are gone rather than wired, because neither could be made
// true:
//
//   - Workspace ("inherit" | "branch" | "scratch") promised git-worktree or
//     scratch-directory isolation. No such machinery exists in this harness, so
//     "branch" meant the parent's working tree with a different word for it —
//     and an isolation mode that silently does nothing is the worst kind of
//     default, because the caller believes the child's writes are contained.
//     Implementing it is a design decision, not a wiring fix; advertising it in
//     the meantime is not.
//   - EnableSubagentTools promised a child that could dispatch children. The
//     depth bound in this harness is structural: the dispatch tools are absent
//     from a child's registry, so there is nothing for the flag to switch on.
//     A permission the design refuses to grant under any value is not a
//     permission, and keeping it would only invite someone to honour it.
//
// devcouncil_define_subagent refuses either key rather than dropping it, so a
// caller that still sends one is told, instead of being ignored the way this
// struct used to ignore them.
type Definition struct {
	Name         string `json:"name"`
	Role         string `json:"role"`
	Description  string `json:"description"`
	SystemPrompt string `json:"system_prompt"`
	// Model is where the child runs: "inherit", "provider/model", a bare
	// provider, or a bare model. See ParsePlacement.
	Model string `json:"model,omitempty"`
	// EnableMCPTools admits the MCP tool group, which reaches servers outside
	// this process. False takes the group away from the child's registry.
	EnableMCPTools bool `json:"enable_mcp_tools"`
	// EnableWriteTools, when false, makes the child read-only. It is a floor a
	// caller can lower and never raise: a dispatch that asked for a non-mutating
	// child gets one whatever the role permits.
	EnableWriteTools bool `json:"enable_write_tools"`
	// AllowedTools, when non-empty, is an allowlist of tool names. It only ever
	// removes — see ToolSurface.
	AllowedTools []string `json:"allowed_tools,omitempty"`
}

// Registry stores dynamic subagent definitions.
type Registry struct {
	mu   sync.RWMutex
	defs map[string]Definition
}

// NewRegistry creates a registry populated with default built-in subagent types.
func NewRegistry() *Registry {
	r := &Registry{
		defs: make(map[string]Definition),
	}

	// Built-in defaults:
	_ = r.Register(Definition{
		Name:             "research",
		Role:             "Codebase & Documentation Researcher",
		Description:      "Read-only research subagent with exploration, dev map, and search tools for codebase surveys and docs verification.",
		SystemPrompt:     "You are a specialized research subagent. Systematically explore and comprehend the codebase, utilize the dev map for symbol navigation, verify online/official documentation, and identify structural gaps without making mutations.",
		Model:            "inherit",
		EnableMCPTools:   true,
		EnableWriteTools: false,
	})

	_ = r.Register(Definition{
		Name:             "builder",
		Role:             "Full-Stack Feature Builder",
		Description:      "Full-stack implementation subagent with core reuse, minimal focused changes, and test-driven verification.",
		SystemPrompt:     "You are a specialized feature builder subagent. Construct upon existing core functions without duplication or bloat. Characterize baseline behavior with tests first, apply maximal hardening, and verify complete gap resolution.",
		Model:            "inherit",
		EnableMCPTools:   true,
		EnableWriteTools: true,
	})

	_ = r.Register(Definition{
		Name:             "critic",
		Role:             "Adversarial Code & Security Reviewer",
		Description:      "Adversarial code reviewer that verifies edge cases, invariants, security posture, and test coverage.",
		SystemPrompt:     "You are a specialized critic subagent. Adversarially audit proposed changes against invariants, edge cases (empty, nil, concurrent, timeout), credential safety, and regression risks. Disrupt existing logic with stress tests before certifying done.",
		Model:            "inherit",
		EnableMCPTools:   false,
		EnableWriteTools: false,
	})

	_ = r.Register(Definition{
		Name:             "planner",
		Role:             "Problem Deconstructor & Hypothesis Architect",
		Description:      "Deconstructs complex requirements, formulates hypotheses, surveys existing features, and drafts implementation artifacts.",
		SystemPrompt:     "You are a specialized planning subagent. Deconstruct complex problems, formulate verifiable hypotheses, identify architectural gaps, clarify decision impacts, and draft structured plans under .devcouncil/artifacts/ without making code mutations.",
		Model:            "inherit",
		EnableMCPTools:   true,
		EnableWriteTools: false,
	})

	_ = r.Register(Definition{
		Name:             "stress_tester",
		Role:             "Adversarial Stress Tester & Hardener",
		Description:      "Rigorously stress-tests logic, probes edge cases (empty, nil, concurrent, timeout), and disrupts assumptions prior to deployment.",
		SystemPrompt:     "You are a specialized adversarial stress tester. Attack solutions with boundary conditions, concurrent races, malformed inputs, and timeouts. Disrupt existing logic with tests to prove resilience and maximal hardening.",
		Model:            "inherit",
		EnableMCPTools:   true,
		EnableWriteTools: true,
	})

	_ = r.Register(Definition{
		Name:             "self",
		Role:             "Autonomous Pair Subagent",
		Description:      "Subagent that inherits parent configuration, tools, and system prompt for delegated concurrent work.",
		SystemPrompt:     "",
		Model:            "inherit",
		EnableMCPTools:   true,
		EnableWriteTools: true,
	})

	return r
}

// Register registers a new or updated dynamic subagent definition.
func (r *Registry) Register(def Definition) error {
	if def.Name == "" {
		return errors.New("agents: subagent definition requires a name")
	}
	if def.Role == "" {
		def.Role = def.Name
	}
	if def.Model == "" {
		def.Model = "inherit"
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs[def.Name] = def
	return nil
}

// Get retrieves a subagent definition by name.
func (r *Registry) Get(name string) (Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.defs[name]
	return d, ok
}

// List returns all registered definitions in sorted order.
func (r *Registry) List() []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var list []Definition
	for _, d := range r.defs {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Name < list[j].Name
	})
	return list
}

// Instance represents one live running or completed subagent.
type Instance struct {
	ConversationID string `json:"conversationId"`
	Type           string `json:"type"`
	Role           string `json:"role"`
	State          string `json:"state"`
	StateDetail    string `json:"stateDetail,omitempty"`
	TranscriptURI  string `json:"transcript,omitempty"`

	mu     sync.Mutex
	cancel func()
	inbox  chan string
}

// SetState updates the live state and detail.
func (inst *Instance) SetState(state, detail string) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.State = state
	inst.StateDetail = detail
}

// InstanceManager tracks active subagents and message routing.
type InstanceManager struct {
	mu        sync.RWMutex
	instances map[string]*Instance
}

// NewInstanceManager creates a new instance manager.
func NewInstanceManager() *InstanceManager {
	return &InstanceManager{
		instances: make(map[string]*Instance),
	}
}

// Register adds an instance.
func (m *InstanceManager) Register(inst *Instance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instances[inst.ConversationID] = inst
}

// Get retrieves an instance by conversation ID.
func (m *InstanceManager) Get(id string) (*Instance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst, ok := m.instances[id]
	return inst, ok
}

// List returns all registered subagent instances.
func (m *InstanceManager) List() []*Instance {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var list []*Instance
	for _, inst := range m.instances {
		list = append(list, inst)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].ConversationID < list[j].ConversationID
	})
	return list
}

// SendMessage delivers a message to a subagent's inbox.
func (m *InstanceManager) SendMessage(id, message string) error {
	m.mu.RLock()
	inst, ok := m.instances[id]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("agents: subagent with conversation ID %q not found", id)
	}

	inst.mu.Lock()
	defer inst.mu.Unlock()

	if inst.inbox == nil {
		inst.inbox = make(chan string, 10)
	}

	select {
	case inst.inbox <- message:
		return nil
	default:
		return fmt.Errorf("agents: subagent %s inbox is full", id)
	}
}

// Kill terminates a subagent instance.
func (m *InstanceManager) Kill(id string) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("agents: subagent %q not found", id)
	}

	inst.mu.Lock()
	cancel := inst.cancel
	inst.State = StateCanceling
	inst.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return nil
}

// KillAll terminates all running subagent instances.
func (m *InstanceManager) KillAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, inst := range m.instances {
		inst.mu.Lock()
		cancel := inst.cancel
		inst.State = StateCanceling
		inst.mu.Unlock()

		if cancel != nil {
			cancel()
		}
	}
	return nil
}
