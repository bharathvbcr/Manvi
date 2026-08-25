// Package tools is the tool registry and its guarded execution pipeline.
//
// The pipeline is three waterfalls around a tool body:
//
//	tools/pre-execute   approval, policy, budget — may deny or rewrite
//	tools/execute       around-dispatch: timeouts, retries, metrics
//	tools/post-execute  redaction, truncation, added context
//
// A pre-execute listener that returns without calling next short-circuits: the
// tool body never runs and the loop is not told which listener decided. That is
// what lets the DevCouncil write gate and an interactive approval prompt sit
// side by side as ordinary listeners, neither knowing the other exists and
// neither requiring a change to the loop.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"

	"manvi/core/bus"
	"manvi/llm"
)

// Group constants for logical categorization of tools.
const (
	GroupCore     = "core"
	GroupTask     = "task"
	GroupNav      = "nav"
	GroupSubagent = "subagent"
	GroupMCP      = "mcp"
	GroupArtifact = "artifact"
	GroupQuestion = "question"
)

// ToolSummary is a compact descriptive summary of a registered tool for discovery.
type ToolSummary struct {
	Name        string `json:"name"`
	Group       string `json:"group,omitempty"`
	Description string `json:"description"`
	Active      bool   `json:"active"`
	ReadOnly    bool   `json:"read_only"`
	Extended    bool   `json:"extended"`
}

// Call is one tool invocation as the model asked for it.
type Call struct {
	ID        llm.CallID
	Name      string
	Arguments json.RawMessage
}

// Result is the single model-facing outcome of a call.
type Result struct {
	Text    string
	IsError bool
	// Rule and Severity are set when policy decided the outcome, so the session
	// log can record *why* without the loop having to understand policy.
	Rule     string
	Severity string
	// GrantID is set when an override cleared a block.
	GrantID   string
	GrantedBy string
	// Demoted is set when a gate mode or the development posture allowed
	// something the rules denied, naming the flag responsible.
	Demoted string
	// Degraded names checks that could not run. A result carrying these is not
	// a clean pass, and the session log must be able to say so later.
	Degraded []string
	// Widened is set when the write was authorised by scope an executor added
	// to its own task rather than by the plan the task was created with. It is
	// separate from GrantID because it outlives one: the grant that first
	// argued for the path expires, and this does not.
	Widened string
	// PipelinePanic names the stage of the tool pipeline that panicked, when
	// one did. It is separate from Blocked because the two are different
	// refusals: the gate deciding against a call is the system working, and a
	// stage failing to finish is a defect in this harness.
	//
	// It exists because the outcome summary could not tell them apart. A
	// panicking stage sets Degraded, which makes Qualified() true, which the
	// run summary renders as "allowed but not on the rules alone" — for a call
	// whose own text says it "was refused because a stage that could not finish
	// is not a stage that approved". Under --quiet that line is the only trace,
	// and it says the opposite of what happened.
	PipelinePanic string
	// Blocked reports that the gate refused this call. It is the gate's own
	// verdict, carried rather than re-derived.
	//
	// The fields above cannot reconstruct it. Rule is set on allowed results
	// too — annotate puts one on every qualified allow, naming the rule that
	// would have blocked it — and IsError is set by any tool that simply
	// failed. Their conjunction is not the verdict either: a command the gate
	// allowed under a demotion and which then exited non-zero on its own has
	// both, and was counted as a gate refusal. That misreport landed hardest in
	// the one posture where the true count is zero by construction: a --yolo
	// run, where nothing can be refused, reported "1 of 9 tool call(s) were
	// refused by the gate" for a shell command that failed on a bad cd.
	//
	// Reading a refusal that did not happen is the dangerous direction — it
	// tells an operator a gate contained something when nothing was contained.
	// Only the gate knows, so only the gate sets this.
	Blocked bool
}

// Qualified reports whether the outcome was reached by anything other than the
// rules themselves — an override, a demotion, or a check that did not run.
// A successful result that is not qualified is an ordinary pass.
func (r Result) Qualified() bool {
	return r.GrantID != "" || r.Demoted != "" || r.Widened != "" || len(r.Degraded) > 0
}

// Errorf builds an error result.
func Errorf(format string, args ...any) Result {
	return Result{Text: fmt.Sprintf(format, args...), IsError: true}
}

// Handler runs a tool.
type Handler func(ctx context.Context, call Call) Result

// Tool is a registered tool.
type Tool struct {
	Schema  llm.ToolSchema
	Handler Handler
	// Group is the category/subsystem this tool belongs to.
	Group string
	// ReadOnly marks a tool that cannot mutate. Search agents get only these.
	ReadOnly bool
	// Extended marks a tool outside the core edit loop — task lifecycle, code
	// graph, sub-agent dispatch.
	//
	// Tool-selection accuracy falls as the offered set grows, and a mid-size
	// local model has to choose from the same list a frontier model does while
	// every schema also costs context on every request. Marking the periphery
	// lets a caller offer the ten tools that do the work without maintaining a
	// second list that drifts from this one.
	Extended bool
	// Requires names the tools this one's only path to success runs through.
	//
	// It exists because narrowing the offered set used to be able to produce a
	// dead end. `llm.local.core_tools_only` drops every Extended tool, and it
	// shipped offering `devcouncil_verify_task` — which cannot report anything
	// without a lease — while dropping `devcouncil_checkout_task`, the one tool
	// that takes one. The model was handed a tool that could never succeed, and
	// the refusal it produced named the tool that had just been taken away.
	//
	// Stating the prerequisite here rather than in a test's list of pairs is
	// what keeps that from happening again: a new tool that reaches a lease
	// check says so once, and UnsatisfiedIn catches any profile that would
	// strand it. A list of pairs written beside the registry is true on the day
	// it is written and quietly wrong afterwards.
	Requires []string
}

// PreExecute is the waterfall event for the pre-execute stage. A listener may
// rewrite Call, or set Decided to short-circuit.
type PreExecute struct {
	Ctx  context.Context
	Call Call
	// Decided, when non-nil, is the outcome; the tool body is skipped.
	Decided *Result
	// panicked carries a listener panic back out of the waterfall. It is
	// unexported because it is not something a listener sets — only the
	// recovery around the stage does.
	panicked any
}

// Execute is the around-dispatch waterfall.
type Execute struct {
	Ctx    context.Context
	Call   Call
	Result Result
	// dispatch runs the registered tool body. A listener wrapping this stage
	// calls next(), which eventually reaches the body.
	dispatch func(context.Context, Call) Result
	// ran records whether the body has been dispatched. It is a pointer
	// because the event travels through the waterfall by value.
	ran *bool
}

// Dispatch runs the tool body. Listeners that want to wrap execution — a
// timeout, a retry, a metric — call this from inside their own handling.
func (e Execute) Dispatch() Result {
	if e.ran != nil {
		*e.ran = true
	}
	return e.dispatch(e.Ctx, e.Call)
}

// PostExecute is the post-execute waterfall: redaction, truncation, context.
type PostExecute struct {
	Ctx    context.Context
	Call   Call
	Result Result
}

// Registry holds tools and runs the pipeline.
type Registry struct {
	mu          sync.RWMutex
	tools       map[string]Tool
	active      map[string]bool
	dynamicMode bool
	bus         *bus.Bus
	// scrub removes watched credentials from every result text. Nil means no
	// scrubbing, which is what a registry built without a composition root
	// gets — tests, mostly.
	scrub func(string) string
}

// SetScrubber installs the credential backstop every tool result passes
// through.
//
// It belongs here rather than at the call sites because this is the one place
// every result from every tool converges, and the alternative was what the
// harness actually did: the scrubber was wired to the *display* layer only —
// the renderer and the JSON sink — so what a person saw on their terminal was
// clean while what went to disk and to the provider was not. A subprocess that
// prints its own environment is the case the scrubber's own documentation names,
// and `run_command` never sets cmd.Env, so `env` returned the harness's own API
// keys; the loop appended that text verbatim to the session file and sent it in
// the next request body.
//
// Scrubbing at the surface that renders is scrubbing the one consumer that was
// never the risk.
func (r *Registry) SetScrubber(scrub func(string) string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scrub = scrub
}

func (r *Registry) scrubText(text string) string {
	if text == "" {
		return text
	}
	r.mu.RLock()
	scrub := r.scrub
	r.mu.RUnlock()
	if scrub == nil {
		return text
	}
	return scrub(text)
}

// NewRegistry returns a registry bound to an event bus.
func NewRegistry(b *bus.Bus) *Registry {
	return &Registry{
		tools:  map[string]Tool{},
		active: map[string]bool{},
		bus:    b,
	}
}

// Register adds a tool. A duplicate name is an error: two tools answering to
// one name would make which body ran depend on registration order.
func (r *Registry) Register(t Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.Schema.Name == "" {
		return fmt.Errorf("tools: tool has no name")
	}
	if _, dup := r.tools[t.Schema.Name]; dup {
		return fmt.Errorf("tools: %q is already registered", t.Schema.Name)
	}
	if t.Handler == nil {
		return fmt.Errorf("tools: %q has no handler", t.Schema.Name)
	}
	r.tools[t.Schema.Name] = t
	if r.dynamicMode && (!t.Extended || t.Group == GroupCore) {
		r.active[t.Schema.Name] = true
	}
	return nil
}

// EnableDynamic turns on dynamic tool activation mode and activates the initial set of tools.
// If initial is empty, defaults to activating all non-extended (or GroupCore) tools.
func (r *Registry) EnableDynamic(initial ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dynamicMode = true
	r.active = make(map[string]bool)

	if len(initial) == 0 {
		for name, t := range r.tools {
			if !t.Extended || t.Group == GroupCore {
				r.active[name] = true
			}
		}
	} else {
		for _, name := range initial {
			if _, ok := r.tools[name]; ok {
				r.active[name] = true
			}
		}
	}
	r.resolveDependenciesLocked()
}

// IsDynamic reports whether dynamic tool activation is enabled.
func (r *Registry) IsDynamic() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.dynamicMode
}

// SetDynamicMode explicitly enables or disables dynamic mode.
func (r *Registry) SetDynamicMode(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dynamicMode = enabled
	if enabled && len(r.active) == 0 {
		for name, t := range r.tools {
			if !t.Extended || t.Group == GroupCore {
				r.active[name] = true
			}
		}
		r.resolveDependenciesLocked()
	}
}

// Activate marks named tools or entire tool groups as active in the offered set.
// It also transitively activates any prerequisite tools declared in Requires.
func (r *Registry) Activate(namesOrGroups ...string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var newlyActivated []string
	for _, target := range namesOrGroups {
		matched := false
		// Check if target is a group name
		for name, t := range r.tools {
			if strings.EqualFold(t.Group, target) {
				matched = true
				if !r.active[name] {
					r.active[name] = true
					newlyActivated = append(newlyActivated, name)
				}
			}
		}
		// Check if target is a specific tool name
		if t, ok := r.tools[target]; ok {
			matched = true
			if !r.active[t.Schema.Name] {
				r.active[t.Schema.Name] = true
				newlyActivated = append(newlyActivated, t.Schema.Name)
			}
		}
		if !matched {
			return nil, fmt.Errorf("tools: unknown tool or group %q", target)
		}
	}

	r.resolveDependenciesLocked()
	sort.Strings(newlyActivated)
	return newlyActivated, nil
}

// Deactivate deactivates the specified tools or groups from the offered set.
func (r *Registry) Deactivate(namesOrGroups ...string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var deactivated []string
	for _, target := range namesOrGroups {
		for name, t := range r.tools {
			if strings.EqualFold(t.Group, target) || name == target {
				if r.active[name] {
					delete(r.active, name)
					deactivated = append(deactivated, name)
				}
			}
		}
	}
	sort.Strings(deactivated)
	return deactivated
}

// resolveDependenciesLocked transitively activates prerequisites in Requires for all active tools.
func (r *Registry) resolveDependenciesLocked() {
	changed := true
	for changed {
		changed = false
		for name, active := range r.active {
			if !active {
				continue
			}
			t, ok := r.tools[name]
			if !ok {
				continue
			}
			for _, req := range t.Requires {
				if _, exists := r.tools[req]; exists && !r.active[req] {
					r.active[req] = true
					changed = true
				}
			}
		}
	}
}

// Schemas returns the tool schemas to offer a model, sorted by name so a
// request is byte-identical across runs.
func (r *Registry) Schemas() []llm.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.schemasLocked(func(t Tool) bool { return true })
}

// CoreSchemas returns the tools in the core edit loop, omitting those marked
// Extended.
func (r *Registry) CoreSchemas() []llm.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.schemasLocked(func(t Tool) bool { return !t.Extended })
}

// CoreReadOnlySchemas is the intersection: core tools that cannot mutate.
func (r *Registry) CoreReadOnlySchemas() []llm.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.schemasLocked(func(t Tool) bool { return !t.Extended && t.ReadOnly })
}

// ActiveSchemas returns schemas of all active tools when dynamic mode is on,
// or all schemas when dynamic mode is off, sorted deterministically by name.
func (r *Registry) ActiveSchemas() []llm.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.dynamicMode {
		return r.schemasLocked(func(t Tool) bool { return true })
	}
	return r.schemasLocked(func(t Tool) bool { return r.active[t.Schema.Name] })
}

// ActiveCoreSchemas returns core active schemas.
func (r *Registry) ActiveCoreSchemas() []llm.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.dynamicMode {
		return r.schemasLocked(func(t Tool) bool { return !t.Extended })
	}
	return r.schemasLocked(func(t Tool) bool { return r.active[t.Schema.Name] && !t.Extended })
}

// ActiveReadOnlySchemas returns read-only active schemas.
func (r *Registry) ActiveReadOnlySchemas() []llm.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.dynamicMode {
		return r.schemasLocked(func(t Tool) bool { return t.ReadOnly })
	}
	return r.schemasLocked(func(t Tool) bool { return r.active[t.Schema.Name] && t.ReadOnly })
}

// ActiveCoreReadOnlySchemas returns core read-only active schemas.
func (r *Registry) ActiveCoreReadOnlySchemas() []llm.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.dynamicMode {
		return r.schemasLocked(func(t Tool) bool { return !t.Extended && t.ReadOnly })
	}
	return r.schemasLocked(func(t Tool) bool { return r.active[t.Schema.Name] && !t.Extended && t.ReadOnly })
}

func (r *Registry) schemasLocked(filter func(Tool) bool) []llm.ToolSchema {
	out := make([]llm.ToolSchema, 0, len(r.tools))
	for _, t := range r.tools {
		if filter(t) {
			out = append(out, t.Schema)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Search performs a case-insensitive search across tool names, groups, and descriptions.
func (r *Registry) Search(query string) []ToolSummary {
	r.mu.RLock()
	defer r.mu.RUnlock()

	q := strings.ToLower(strings.TrimSpace(query))
	var out []ToolSummary
	for _, t := range r.tools {
		nameMatch := strings.Contains(strings.ToLower(t.Schema.Name), q)
		groupMatch := strings.Contains(strings.ToLower(t.Group), q)
		descMatch := strings.Contains(strings.ToLower(t.Schema.Description), q)
		if q == "" || nameMatch || groupMatch || descMatch {
			out = append(out, ToolSummary{
				Name:        t.Schema.Name,
				Group:       t.Group,
				Description: t.Schema.Description,
				Active:      !r.dynamicMode || r.active[t.Schema.Name],
				ReadOnly:    t.ReadOnly,
				Extended:    t.Extended,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListSummaries returns summaries of all registered tools.
func (r *Registry) ListSummaries() []ToolSummary {
	return r.Search("")
}

// ActiveGroups names the groups that have at least one tool in the offered set,
// sorted so two runs with the same set produce the same list.
//
// It exists so guidance can follow capability. A prompt that explains dev-map
// navigation to a model holding no navigation tools is spending context on a
// capability that is not there, and under dynamic loading the offered set is
// decided per turn — so the question cannot be answered once at startup.
//
// Outside dynamic mode every registered group is offered, so every group is
// reported: the answer stays true rather than becoming empty for callers that
// never narrowed anything.
func (r *Registry) ActiveGroups() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	seen := map[string]bool{}
	for name, t := range r.tools {
		if t.Group == "" {
			continue
		}
		if r.dynamicMode && !r.active[name] {
			continue
		}
		seen[t.Group] = true
	}
	out := make([]string, 0, len(seen))
	for group := range seen {
		out = append(out, group)
	}
	sort.Strings(out)
	return out
}

// UnsatisfiedIn reports the tools in an offered set whose declared
// prerequisites that set does not also offer, and any prerequisite naming a
// tool that is not registered at all.
func (r *Registry) UnsatisfiedIn(offered []llm.ToolSchema) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	inProfile := make(map[string]bool, len(offered))
	for _, s := range offered {
		inProfile[s.Name] = true
	}

	var gaps []string
	for _, t := range r.tools {
		if !inProfile[t.Schema.Name] {
			continue
		}
		for _, need := range t.Requires {
			if _, registered := r.tools[need]; !registered {
				gaps = append(gaps, fmt.Sprintf(
					"%s requires %q, which is not a registered tool", t.Schema.Name, need))
				continue
			}
			if !inProfile[need] {
				gaps = append(gaps, fmt.Sprintf(
					"%s is offered but %s, which it needs to succeed, is not",
					t.Schema.Name, need))
			}
		}
	}
	sort.Strings(gaps)
	return gaps
}

// ReadOnlySchemas returns only the tools that cannot mutate.
func (r *Registry) ReadOnlySchemas() []llm.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.schemasLocked(func(t Tool) bool { return t.ReadOnly })
}

// Subset returns a registry holding only the tools that pass keep, sharing this
// registry's event bus.
func (r *Registry) Subset(keep func(Tool) bool) *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := &Registry{
		tools:       make(map[string]Tool, len(r.tools)),
		active:      make(map[string]bool, len(r.active)),
		dynamicMode: r.dynamicMode,
		bus:         r.bus,
	}
	for name, t := range r.tools {
		if keep(t) {
			out.tools[name] = t
			if r.active[name] {
				out.active[name] = true
			}
		}
	}
	return out
}

// stagePanic converts a panic in one pipeline stage into a refusal.
//
// Every stage fails closed, and each for its own reason.
//
// Pre-execute is where the write gate and the approval prompt live. A listener
// that panics there has not decided anything, and the tempting reading — carry
// on as though that listener had not been registered — is the one that must
// never be taken: it is exactly a gate that did not run, wearing the face of a
// gate that ran and allowed. So the call is refused.
//
// Execute is the tool body. A panic there may have left side effects behind —
// a partial write, a spawned process — and nothing here can undo them. What it
// must not do is report success, so the outcome is an error that names the
// stage rather than a result the model would read as work completed.
//
// Post-execute is redaction and truncation. This is the least obvious and the
// worst to get wrong: the result exists and looks fine, and returning it is
// returning output that the redactor never finished passing over. A panicking
// redactor is precisely when the raw text must not be handed back.
//
// Degraded is set in every case, so Result.Qualified() is true and the session
// log records that this outcome was not reached by the rules running cleanly.
func stagePanic(stage string, panicked any) Result {
	fmt.Fprintf(os.Stderr, "manvi: tool pipeline: %s stage panicked: %v\n%s\n",
		stage, panicked, debug.Stack())
	return Result{
		IsError:       true,
		PipelinePanic: stage,
		Text: fmt.Sprintf(
			"the %s stage of the tool pipeline failed: %v; the call was refused because a stage "+
				"that could not finish is not a stage that approved", stage, panicked),
		Degraded: []string{stage + " stage panicked: " + fmt.Sprint(panicked)},
	}
}

// Run executes one call through the full pipeline.
func (r *Registry) Run(ctx context.Context, call Call) (result Result) {
	defer func() {
		// A backstop for anything the per-stage recoveries below do not cover
		// — the bus itself, or a panic raised while building a refusal. It
		// fails closed for the same reason they do.
		if panicked := recover(); panicked != nil {
			result = stagePanic("tool", panicked)
		}
		// Scrubbed last, on the named return, so every way out of this function
		// is covered: the ordinary path, a listener short-circuit, a refusal, a
		// post-execute rewrite, and the panic recovery immediately above. A
		// credential in a panic message is still a credential.
		result.Text = r.scrubText(result.Text)
	}()

	pre, err := r.preExecute(ctx, call)
	if err != nil {
		return Errorf("tool pipeline: %v", err)
	}
	if pre.panicked != nil {
		return stagePanic("pre-execute", pre.panicked)
	}
	// A listener may have rewritten the call — a path normaliser, a scope
	// clamp — so the rewritten form is what runs and what post-execute sees.
	call = pre.Call

	if pre.Decided != nil {
		// Short-circuited. The body never runs, and the loop is not told which
		// listener decided; it only sees the outcome.
		result = *pre.Decided
		// A listener that short-circuits with a rule refused the call before it
		// ran, and this is the seam a policy gate mounts on — so this is where
		// that refusal becomes Result.Blocked. Keying on the rule rather than
		// on the error alone keeps the claim narrow: only policy sets a Rule,
		// and a listener that short-circuits for some other reason (malformed
		// arguments, a stage it could not reach) is not claiming to have
		// refused anything and must not be counted as a gate denial.
		if result.IsError && result.Rule != "" {
			result.Blocked = true
		}
	} else {
		execResult, panicked, err := r.execute(ctx, call)
		if err != nil {
			return Errorf("tool pipeline: %v", err)
		}
		if panicked != nil {
			return stagePanic("execute", panicked)
		}
		result = execResult
	}

	post, panicked, err := r.postExecute(ctx, call, result)
	if err != nil {
		return Errorf("tool pipeline: %v", err)
	}
	if panicked != nil {
		return stagePanic("post-execute", panicked)
	}
	return post
}

// preExecute runs the approval and policy waterfall, catching a panic rather
// than letting it past the gate.
func (r *Registry) preExecute(ctx context.Context, call Call) (ev PreExecute, err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			ev = PreExecute{Ctx: ctx, Call: call, panicked: panicked}
			err = nil
		}
	}()
	return bus.Waterfall(r.bus, PreExecute{Ctx: ctx, Call: call})
}

// execute runs the around-dispatch waterfall and, if no listener wrapped it,
// the tool body.
func (r *Registry) execute(ctx context.Context, call Call) (result Result, panicked any, err error) {
	defer func() {
		if p := recover(); p != nil {
			panicked = p
			result = Result{}
			err = nil
		}
	}()
	ran := false
	exec, err := bus.Waterfall(r.bus, Execute{
		Ctx:      ctx,
		Call:     call,
		dispatch: r.dispatch,
		ran:      &ran,
	})
	if err != nil {
		return Result{}, nil, err
	}
	// A listener that wrapped dispatch already ran the body; one that only
	// observed did not, so run it here. Asking whether it ran — rather than
	// whether the result looks empty — is what keeps a tool from executing
	// twice.
	if !ran {
		exec.Result = r.dispatch(ctx, call)
	}
	return exec.Result, nil, nil
}

// postExecute runs redaction and truncation, catching a panic rather than
// returning text the redactor did not finish with.
func (r *Registry) postExecute(ctx context.Context, call Call, in Result) (out Result, panicked any, err error) {
	defer func() {
		if p := recover(); p != nil {
			panicked = p
			out = Result{}
			err = nil
		}
	}()
	post, err := bus.Waterfall(r.bus, PostExecute{Ctx: ctx, Call: call, Result: in})
	if err != nil {
		return Result{}, nil, err
	}
	return post.Result, nil, nil
}

func (r *Registry) dispatch(ctx context.Context, call Call) Result {
	r.mu.RLock()
	tool, ok := r.tools[call.Name]
	isActive := !r.dynamicMode || r.active[call.Name]
	r.mu.RUnlock()
	if !ok {
		// An unknown tool is an error result rather than a panic or an empty
		// success: the model must be told it asked for something that does not
		// exist, and the turn continues.
		return Errorf("unknown tool %q", call.Name)
	}
	if !isActive {
		return Errorf("tool %q (group %q) is registered but not currently active in context; call devcouncil_activate_tools with {\"tools\": [%q]} to activate it", call.Name, tool.Group, tool.Schema.Name)
	}
	return tool.Handler(ctx, call)
}

// Has reports whether a tool is registered.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.tools[name]
	return ok
}
