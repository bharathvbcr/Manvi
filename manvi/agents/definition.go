package agents

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Subagent lifecycle states.
//
// Every state named here is one this harness can actually enter, and every move
// between them is checked by SetState. Neither property held before: the block
// declared seven states of which three — idle, waiting_for_input,
// waiting_for_message — were assigned by nothing anywhere in the tree, and
// there were no transition rules at all. So a child went running -> canceling
// -> completed, which is a cancelled agent reporting success, and a second kill
// moved a finished child back out of a terminal state. A state nobody can enter
// is vocabulary the control plane advertises and never speaks; an unchecked
// transition is how "terminated" comes to be reported as "done".
const (
	StateRunning   = "running"
	StateCanceling = "canceling"
	StateCompleted = "completed"
	StateErrored   = "errored"
)

// transitions is the state machine, written down rather than implied.
//
// canceling leads only to errored: a child that was cut short did not finish
// the work its prompt described, and letting it land on completed is precisely
// how a kill comes to be reported as a completion. completed and errored lead
// nowhere.
//
// A state that is not a key here — including the zero value of an Instance that
// was assembled as a bare struct literal rather than by NewInstance — permits
// no move at all. That direction is deliberate: an instance nobody constructed
// properly is one this package cannot reason about, and guessing on its behalf
// is the failure mode this map exists to remove.
var transitions = map[string][]string{
	StateRunning:   {StateCanceling, StateCompleted, StateErrored},
	StateCanceling: {StateErrored},
	StateCompleted: {},
	StateErrored:   {},
}

// terminal reports whether a state is one nothing leaves.
func terminal(state string) bool {
	return state == StateCompleted || state == StateErrored
}

// canTransition reports whether from -> to is a move the machine allows.
// Restating the state an instance is already in is allowed, so a repeated
// report is not an error; every actual move must be named in transitions.
func canTransition(from, to string) bool {
	if from == to {
		return true
	}
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

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
	// WriteExceptions names mutating tools a read-only role may still use.
	//
	// SECURITY IMPACT: this is the only field on a role that widens what a
	// child can do. See ToolSurface.Writes for the reasoning and the bounds.
	// A role that sets EnableWriteTools true has no use for it — the floor is
	// already down.
	WriteExceptions []string `json:"write_exceptions,omitempty"`
}

// Registry stores dynamic subagent definitions.
type Registry struct {
	mu sync.RWMutex
	// defs is every role this registry can dispatch, shipped or authored.
	defs map[string]Definition
	// builtIn names the subset that shipped with the harness. It is tracked
	// separately from defs because the distinction is not recoverable from a
	// Definition — a role authored at runtime under a shipped name looks
	// exactly like the role it replaced. See IsBuiltIn.
	builtIn map[string]bool
}

// NewRegistry creates a registry populated with default built-in subagent types.
func NewRegistry() *Registry {
	r := &Registry{
		defs:    make(map[string]Definition),
		builtIn: make(map[string]bool),
	}

	// Built-in defaults:
	_ = r.register(true, Definition{
		Name:             "research",
		Role:             "Codebase & Documentation Researcher",
		Description:      "Read-only research subagent with exploration, dev map, and search tools for codebase surveys and docs verification.",
		SystemPrompt:     "You are a specialized research subagent. Systematically explore and comprehend the codebase, utilize the dev map for symbol navigation, verify online/official documentation, and identify structural gaps without making mutations.",
		Model:            "inherit",
		EnableMCPTools:   true,
		EnableWriteTools: false,
	})

	_ = r.register(true, Definition{
		Name:             "builder",
		Role:             "Full-Stack Feature Builder",
		Description:      "Full-stack implementation subagent with core reuse, minimal focused changes, and test-driven verification.",
		SystemPrompt:     "You are a specialized feature builder subagent. Construct upon existing core functions without duplication or bloat. Characterize baseline behavior with tests first, apply maximal hardening, and verify complete gap resolution.",
		Model:            "inherit",
		EnableMCPTools:   true,
		EnableWriteTools: true,
	})

	_ = r.register(true, Definition{
		Name:             "critic",
		Role:             "Adversarial Code & Security Reviewer",
		Description:      "Adversarial code reviewer that verifies edge cases, invariants, security posture, and test coverage.",
		SystemPrompt:     "You are a specialized critic subagent. Adversarially audit proposed changes against invariants, edge cases (empty, nil, concurrent, timeout), credential safety, and regression risks. Disrupt existing logic with stress tests before certifying done.",
		Model:            "inherit",
		EnableMCPTools:   false,
		EnableWriteTools: false,
	})

	_ = r.register(true, Definition{
		Name:             "planner",
		Role:             "Problem Deconstructor & Hypothesis Architect",
		Description:      "Deconstructs complex requirements, formulates hypotheses, surveys existing features, and drafts implementation artifacts.",
		SystemPrompt:     "You are a specialized planning subagent. Deconstruct complex problems, formulate verifiable hypotheses, identify architectural gaps, clarify decision impacts, and draft structured plans under .devcouncil/artifacts/ without making code mutations.",
		Model:            "inherit",
		EnableMCPTools:   true,
		EnableWriteTools: false,
		// The two tools this role's own prompt tells it to use.
		//
		// Without them the instruction above — "draft structured plans under
		// .devcouncil/artifacts/" — described work the child could not do: the
		// read-only floor removed the artifact tools, so a planner asked for a
		// plan could only describe one in prose that died with its turn. That
		// is the same defect as a system-prompt section naming a tool the model
		// was not offered, wearing a role definition instead.
		//
		// SECURITY IMPACT, small and stated. It admits exactly two names, both
		// of which write only through the artifact store — contained under
		// .devcouncil/artifacts/, revisioned, and passing the same policy gate
		// as any other artifact write. It does not admit write_file, patch_file,
		// delete_file or exec_command, so a planner still cannot touch
		// repository source or run a command. The widening is from "no
		// mutations" to "may record a plan".
		WriteExceptions: []string{
			"devcouncil_create_artifact",
			"devcouncil_update_artifact",
		},
	})

	_ = r.register(true, Definition{
		Name:             "stress_tester",
		Role:             "Adversarial Stress Tester & Hardener",
		Description:      "Rigorously stress-tests logic, probes edge cases (empty, nil, concurrent, timeout), and disrupts assumptions prior to deployment.",
		SystemPrompt:     "You are a specialized adversarial stress tester. Attack solutions with boundary conditions, concurrent races, malformed inputs, and timeouts. Disrupt existing logic with tests to prove resilience and maximal hardening.",
		Model:            "inherit",
		EnableMCPTools:   true,
		EnableWriteTools: true,
	})

	_ = r.register(true, Definition{
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
//
// This is the in-process seam: a host assembling a catalogue at start-up may
// write whatever it likes, including over a shipped name. What it deliberately
// does not decide is whether a *model* may overwrite a shipped role. That is a
// question about the authority behind a call, and only the tool boundary knows
// a call came from a model, so it is answered there — IsBuiltIn is what the
// boundary asks. See devcouncil.Registry.defineSubagent.
func (r *Registry) Register(def Definition) error { return r.register(false, def) }

func (r *Registry) register(builtIn bool, def Definition) error {
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
	if builtIn {
		r.builtIn[def.Name] = true
	}
	return nil
}

// IsBuiltIn reports whether a name is one of the roles this harness ships.
//
// The shipped roles are the catalogue an operator has already reviewed: critic
// is read-only and denied the MCP group, research may not write. Register
// overwrites by name, so "define a role" and "rewrite the shipped critic to say
// you may write, with MCP on" are the same call — a reviewed permission widened
// inside a single turn, with nothing in the transcript that reads as a
// permission change. A caller that is enforcing an authority boundary asks this
// before writing.
func (r *Registry) IsBuiltIn(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.builtIn[name]
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
//
// The exported fields are written by the goroutine running the child and read
// by the control plane on another goroutine, so they are only ever touched
// under mu. They carry no json tags on purpose: handing an *Instance to
// encoding/json is exactly the unlocked read that raced, and Snapshot is the
// only supported way to render one.
type Instance struct {
	ConversationID string
	Type           string
	Role           string
	State          string
	StateDetail    string
	TranscriptURI  string

	mu sync.Mutex
	// cancel stops the child's work. It is the whole of what Kill can do, and
	// an Instance may not be registered without one -- see NewInstance and
	// InstanceManager.Register. It was previously an unassigned field: nothing
	// outside tests ever set it, so every kill found nil, cancelled nothing,
	// and reported success.
	cancel func()
	inbox  chan string
}

// NewInstance builds a live instance and binds the cancellation that makes Kill
// mean something.
//
// The handle is a required argument rather than an optional setter because the
// alternative is what this package shipped: an Instance assembled as a struct
// literal with no cancel, registered, listed to the model as a live child, and
// then "killed" without anything being cancelled. A constructor that cannot
// produce an unkillable instance is the only version of this that fails closed.
func NewInstance(conversationID, typeName, role string, cancel func()) (*Instance, error) {
	if strings.TrimSpace(conversationID) == "" {
		return nil, errors.New("agents: an instance needs a conversation ID; one nobody can name cannot be listed or killed")
	}
	if cancel == nil {
		return nil, fmt.Errorf("agents: subagent %q was built without a cancellation handle; "+
			"it could be registered and listed but never terminated", conversationID)
	}
	return &Instance{
		ConversationID: conversationID,
		Type:           typeName,
		Role:           role,
		State:          StateRunning,
		cancel:         cancel,
	}, nil
}

// SetState records a lifecycle move, and refuses one the machine does not
// allow.
//
// It returns an error rather than silently ignoring a refused move, and the
// caller is expected to report it. The move that matters is a child trying to
// land on completed after the control plane put it into canceling: the honest
// answer there is "this child was terminated", not "this child is done".
func (inst *Instance) SetState(state, detail string) error {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if !canTransition(inst.State, state) {
		return fmt.Errorf("agents: subagent %q cannot move from %s to %s",
			inst.ConversationID, inst.State, state)
	}
	inst.State = state
	inst.StateDetail = detail
	return nil
}

// InstanceSnapshot is a point-in-time copy of one instance, taken under its own
// lock.
//
// It exists because rendering the live struct was a data race on a path a model
// reaches directly: devcouncil_manage_subagents{"action":"list"} marshalled
// *Instance while pool goroutines wrote State and StateDetail through SetState,
// with no lock held on the reading side at all. The race detector fires on
// every run of that path.
type InstanceSnapshot struct {
	ConversationID string `json:"conversationId"`
	Type           string `json:"type"`
	Role           string `json:"role"`
	State          string `json:"state"`
	StateDetail    string `json:"stateDetail,omitempty"`
	TranscriptURI  string `json:"transcript,omitempty"`
	// Killable reports whether a cancellation handle is attached, so a listing
	// never presents a child the control plane cannot actually stop as one it
	// can.
	Killable bool `json:"killable"`
}

// Snapshot copies this instance's live state.
func (inst *Instance) Snapshot() InstanceSnapshot {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return InstanceSnapshot{
		ConversationID: inst.ConversationID,
		Type:           inst.Type,
		Role:           inst.Role,
		State:          inst.State,
		StateDetail:    inst.StateDetail,
		TranscriptURI:  inst.TranscriptURI,
		Killable:       inst.cancel != nil,
	}
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

// Register adds an instance, and refuses one that could never be terminated.
//
// The refusal is the point. Registering is what makes a child visible to the
// control plane -- listable, addressable, killable -- and an instance with no
// cancellation handle is only the first two. Taking it would put a row in the
// table that every kill answers "done" to and never touches.
func (m *InstanceManager) Register(inst *Instance) error {
	if inst == nil {
		return errors.New("agents: cannot register a nil subagent instance")
	}
	inst.mu.Lock()
	id, killable := inst.ConversationID, inst.cancel != nil
	inst.mu.Unlock()
	if strings.TrimSpace(id) == "" {
		return errors.New("agents: cannot register a subagent instance with no conversation ID")
	}
	if !killable {
		return fmt.Errorf("agents: subagent %q has no cancellation handle; "+
			"registering it would list a child this manager cannot terminate", id)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.instances[id] = inst
	return nil
}

// Get retrieves an instance by conversation ID.
func (m *InstanceManager) Get(id string) (*Instance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst, ok := m.instances[id]
	return inst, ok
}

// Snapshot returns a copy of every registered instance, sorted by ID.
//
// It replaces a List that handed out the live *Instance values. Every caller
// wanted them in order to read State, and reading State off a running child
// without its lock is the race this returns copies to close.
func (m *InstanceManager) Snapshot() []InstanceSnapshot {
	m.mu.RLock()
	live := make([]*Instance, 0, len(m.instances))
	for _, inst := range m.instances {
		live = append(live, inst)
	}
	m.mu.RUnlock()

	out := make([]InstanceSnapshot, 0, len(live))
	for _, inst := range live {
		out = append(out, inst.Snapshot())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ConversationID < out[j].ConversationID
	})
	return out
}

// SendMessage queues a message on a subagent's inbox.
//
// It reports that the message was queued, and that is all it may be read as.
// Nothing in this harness drains the inbox: a sub-agent runs through
// SubAgentRunner, which takes a prompt and returns a result and has no seam for
// anything delivered mid-flight. The queue is kept because it is the shape such
// a seam would take and because callers inside this package can read it; what
// no caller may do is turn "queued" into "the child was told". The control-plane
// tool refuses for exactly that reason rather than answering {"delivered":true}
// into a channel with no reader -- see devcouncil.Registry.sendMessage.
func (m *InstanceManager) SendMessage(id, message string) error {
	m.mu.RLock()
	inst, ok := m.instances[id]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("agents: subagent with conversation ID %q not found", id)
	}

	inst.mu.Lock()
	defer inst.mu.Unlock()

	if terminal(inst.State) {
		return fmt.Errorf("agents: subagent %s has already reached %s; there is nothing left to receive a message",
			id, inst.State)
	}

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
//
// It reports an error for every case in which nothing was terminated -- an ID
// nobody registered, a child that already finished, an instance with no
// cancellation handle -- because a kill that answers nil is read by its caller
// as a child that has been stopped. It previously answered nil for all three.
func (m *InstanceManager) Kill(id string) error {
	m.mu.RLock()
	inst, ok := m.instances[id]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("agents: subagent %q is not registered; nothing was terminated", id)
	}
	return inst.kill()
}

// kill delivers the cancellation and moves the instance into canceling, or
// explains why it could do neither.
func (inst *Instance) kill() error {
	inst.mu.Lock()
	switch {
	case terminal(inst.State):
		state := inst.State
		inst.mu.Unlock()
		return fmt.Errorf("agents: subagent %q already reached %s; nothing was terminated",
			inst.ConversationID, state)
	case inst.cancel == nil:
		inst.mu.Unlock()
		return fmt.Errorf("agents: subagent %q has no cancellation handle; nothing was terminated",
			inst.ConversationID)
	}
	cancel := inst.cancel
	inst.State = StateCanceling
	inst.StateDetail = "terminated by the control plane"
	inst.mu.Unlock()

	// Outside the lock: cancel() unblocks the child, which reports its own
	// state through this same mutex.
	cancel()
	return nil
}

// KillAll terminates every subagent that is still running, and names them.
//
// It returns the IDs it actually cancelled rather than a bare error, because
// the caller's job is to report what happened and "all subagents terminating"
// over an empty manager is a claim about children that do not exist. Nothing
// live to terminate is an error for the same reason: a check that could not run
// must not answer the way one that ran and succeeded does.
func (m *InstanceManager) KillAll() (killed []string, err error) {
	m.mu.RLock()
	live := make([]*Instance, 0, len(m.instances))
	for _, inst := range m.instances {
		live = append(live, inst)
	}
	m.mu.RUnlock()

	if len(live) == 0 {
		return nil, errors.New("agents: no subagents are registered; nothing was terminated")
	}

	var failures []string
	for _, inst := range live {
		id := inst.ConversationID
		if killErr := inst.kill(); killErr != nil {
			failures = append(failures, killErr.Error())
			continue
		}
		killed = append(killed, id)
	}
	sort.Strings(killed)
	sort.Strings(failures)

	if len(killed) == 0 {
		return nil, fmt.Errorf("agents: none of the %d registered subagents could be terminated: %s",
			len(live), strings.Join(failures, "; "))
	}
	if len(failures) > 0 {
		// Partial success is reported as both: the IDs that were cancelled are
		// real, and so are the ones that were not. Collapsing either half is
		// how a caller comes to believe it stopped everything.
		return killed, fmt.Errorf("agents: %d of %d subagents could not be terminated: %s",
			len(failures), len(live), strings.Join(failures, "; "))
	}
	return killed, nil
}
