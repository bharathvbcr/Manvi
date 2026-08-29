package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"manvi/agent"
	"manvi/agents"
	"manvi/core/bus"
	"manvi/devcouncil"
	"manvi/llm"
	"manvi/session"
	"manvi/tools"
	"manvi/ui"
)

// subAgentRunner runs one delegated turn against a real model.
//
// devcouncil owns the tool that dispatches sub-agents but deliberately cannot
// run one: a turn needs a provider, a model, a tool registry and a session log,
// none of which that package has or should acquire. This is the other side of
// that seam, and it lives here because this is where those four things are
// already built.
//
// Before this existed the tool reported `"status":"completed"` for work it had
// never done — it decoded the prompt and never read it. Refusing was the first
// correction; this is the second. The order matters: an unattached runner still
// refuses, so the capability being absent can never again look like success.
type subAgentRunner struct {
	mu sync.RWMutex
	// cfg is nil until attach is called. A nil cfg is a refusal, not a
	// fallback: fabricating a result is the defect this type replaced.
	cfg *subAgentConfig
}

type subAgentConfig struct {
	provider llm.Provider
	models   *llm.Registry
	model    string
	effort   string
	// effortCeiling is how far a stuck child may raise its own effort. A child
	// runs the same loop against the same model and gets stuck the same way, so
	// leaving it empty here opted every fan-out out of a mechanism its parent
	// was deliberately configured for.
	effortCeiling   string
	registry        *tools.Registry
	coreToolsOnly   bool
	assertInvariant bool
	systemPrompt    string
	// native is the devcouncil surface this run's tools are bound to. It is
	// held so a child can be given its own lease session derived from the
	// dispatching agent's, rather than writing through the one every agent in
	// the process shared.
	native *devcouncil.Registry
	// meter accumulates what every child of this run spent, so the run's own
	// usage line is the whole bill and not just the dispatching agent's share.
	meter *subAgentMeter
	// sink receives the child's evidence events, attributed to it.
	//
	// The child keeps its own log — sharing the parent's would interleave two
	// conversations into one projection, and the projection is what the next
	// request is built from. But nothing observed that log, so the evidence in
	// it went nowhere. Forwarding to the sink is not sharing the log: it
	// reaches the faces and the NDJSON stream without touching what the parent
	// replays to the model.
	sink ui.Sink
}

// maxSubAgentSteps bounds a child's turn independently of its parent's ceiling.
//
// A child is given a single scoped instruction, not an open-ended session, and
// a fan-out multiplies whatever this number is by the number of children. The
// parent's own ceiling is the wrong bound here: it was chosen for one agent
// working through a whole task, and inheriting it lets a four-way fan-out spend
// four full turns' worth of budget on what the dispatching agent framed as a
// side question.
const maxSubAgentSteps = 12

// attach gives the runner what it needs to actually run a turn.
//
// It is late-bound because devcouncil.Deps is built before the tool registry it
// would need, and long before a provider has been resolved. Constructing the
// runner empty and filling it in once those exist keeps one object identity for
// the Deps to hold, without inventing a second construction order.
func (r *subAgentRunner) attach(cfg subAgentConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = &cfg
}

// place resolves where one child is to run: the provider that will serve it and
// the model that provider is asked for.
//
// The spec arrives unparsed because only this side holds the provider registry
// — devcouncil describes the role, this resolves it. A spec that inherits, or a
// runner with no registry to resolve against, yields the parent's pair
// unchanged, which is what every child did before roles could name a model.
//
// It fails rather than falling back. A role that asks for a provider this
// invocation never enabled, or a model that provider does not serve, is a
// misconfiguration the operator has to see: quietly running the child on the
// parent's frontier model instead would answer the prompt, bill the wrong
// budget, and report nothing — and the whole point of naming a local worker is
// that the work does *not* go to the expensive model.
// The config is passed in rather than re-read. RunSubAgent already takes a
// snapshot under RLock and nil-checks it; re-reading r.cfg here was both an
// unsynchronised read racing attach — which the TUI calls on every submission,
// because provider, model and effort are switchable mid-session — and a second
// answer to "which config is this child running under", so a child could be
// placed against a different one than the registry and floor were built from.
func (r *subAgentRunner) place(cfg *subAgentConfig, req devcouncil.SubAgentRequest) (llm.Provider, string, error) {

	known := func(name string) bool {
		if cfg.models == nil {
			return false
		}
		_, ok := cfg.models.Get(name)
		return ok
	}

	spec := agents.ParsePlacement(req.ModelSpec, known)
	if spec.Inherits() {
		return cfg.provider, cfg.model, nil
	}

	provider, model := cfg.provider, cfg.model
	if spec.Provider != "" {
		if cfg.models == nil {
			return nil, "", fmt.Errorf(
				"sub-agent %q asks to run on provider %q, but this invocation resolved no provider registry",
				req.Label, spec.Provider)
		}
		p, ok := cfg.models.Get(spec.Provider)
		if !ok {
			return nil, "", fmt.Errorf(
				"sub-agent %q asks to run on provider %q, which is not enabled here (have %v)",
				req.Label, spec.Provider, cfg.models.Names())
		}
		provider = p
	}
	if spec.Model != "" {
		model = spec.Model
	}

	// Checked here as well as in the loop so the message names the child and
	// the fix. A bare provider carries the parent's model across to an adapter
	// that has no reason to serve it — "local" alone hands a frontier model
	// name to a local server — and the loop's own resolve would report that as
	// an unserved model without saying which role asked or what to write
	// instead.
	if _, ok := provider.Capability(model); !ok {
		return nil, "", fmt.Errorf(
			"sub-agent %q cannot run: provider %q does not serve model %q; name the placement as \"provider/model\"",
			req.Label, provider.Name(), model)
	}
	return provider, model, nil
}

// RunSubAgent carries out one delegated instruction and reports what came back.
func (r *subAgentRunner) RunSubAgent(ctx context.Context, req devcouncil.SubAgentRequest) (devcouncil.SubAgentResult, error) {
	r.mu.RLock()
	cfg := r.cfg
	r.mu.RUnlock()

	if cfg == nil {
		return devcouncil.SubAgentResult{}, fmt.Errorf(
			"sub-agents are not available in this invocation: no model is attached to run one")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return devcouncil.SubAgentResult{}, fmt.Errorf("sub-agent %q was given no prompt", req.Label)
	}

	// The two rules no role may touch. Composed as a function so the narrowed
	// surface below is literally this one plus more removals — a role can only
	// ever intersect with it, never union.
	floor := func(t tools.Tool) bool {
		// No child may spawn children. The depth bound is structural rather
		// than a counter: the dispatching tools are absent from the child's
		// registry, so there is no path to a grandchild to count. A counter
		// would have to be threaded through every layer and would still be one
		// missed increment away from a fan-out that multiplies without limit.
		//
		// The whole group goes, not one name. Removing only
		// devcouncil_spawn_subagents left the bound one tool wide:
		// devcouncil_invoke_subagent calls the very same runner, so a child
		// holding it produced a grandchild, and devcouncil_manage_subagents let
		// that child kill its own siblings out of a shared instance manager.
		// The named check stays beside the group check on purpose — if that
		// tool is ever regrouped, the structural rule must not quietly move
		// with it.
		if t.Group == tools.GroupSubagent || t.Schema.Name == spawnSubagentsTool {
			return false
		}
		// ReadOnly is enforced by absence, not by hiding the schema. The
		// contract on SubAgentRequest says a runner that cannot honour it must
		// fail rather than hand back a child that can write, and a registry
		// that still holds the write tools honours nothing: Registry.Run
		// dispatches by name whether or not the schema was offered.
		//
		// It is a floor and not a ceiling for the caller: a dispatch that asked
		// for a non-mutating child never receives a writing one because the
		// role permits writes. The named exceptions below are not an escape
		// from that. They are stripped at the dispatch site whenever the caller
		// asked for read-only — see spawnSubagents and invokeSubagents — so
		// what reaches here is only ever a role lifting its *own* floor for
		// tools it named one at a time.
		if req.ReadOnly && !t.ReadOnly && !req.Surface.PermitsWrite(t.Schema.Name) {
			return false
		}
		return true
	}

	registry := cfg.registry.Subset(floor)
	if req.Surface.Constrains() {
		// A role's declared surface removes further, and only further: Permits
		// is consulted after the floor has already said yes, so naming a tool
		// in allowed_tools cannot hand back something the floor took away.
		//
		// Enforced by absence for the same reason the floor is. Offering the
		// child a narrowed schema list while its registry still held the MCP
		// tools would leave `enable_mcp_tools: false` meaning "the model was
		// not reminded of these", which is not what an operator writing it
		// believes they wrote.
		narrowed := cfg.registry.Subset(func(t tools.Tool) bool {
			return floor(t) && req.Surface.Permits(t.Schema.Name, t.Group)
		})
		if stranded := surfaceDeadEnds(cfg.registry, registry, narrowed); len(stranded) > 0 {
			return devcouncil.SubAgentResult{}, fmt.Errorf(
				"sub-agent %q cannot run: its role's tool surface strands a tool that can never succeed — %s; "+
					"name the prerequisite in allowed_tools, or drop the tool that needs it",
				req.Label, strings.Join(stranded, "; "))
		}
		registry = narrowed
	}

	// A child gets its own log. Sharing the parent's would interleave two
	// conversations into one projection, and the projection is what the next
	// request is built from — the parent would find the child's tool results
	// answering calls it never made.
	log := newSessionLog()
	if cfg.sink != nil {
		label := req.Label
		log.Observe(func(e session.Event) {
			// Assistant chunks are the child's prose stream, and the child's
			// answer already returns through the tool result. Forwarding them
			// would interleave two transcripts in the terminal for no gain.
			// Everything else is evidence about what the child's tools and the
			// gate actually did, which is exactly what was being lost.
			if e.Type == session.AssistantChunk {
				return
			}
			for _, out := range ui.Project(e) {
				// Filtered on the *projected* kind, not the session type,
				// because the projection is where the damage is: a child's
				// UserMessage becomes KindTurnStart and its TurnEnd becomes
				// KindTurnEnd. Forwarding those put four turn.start and four
				// turn.end lines inside one turn on the NDJSON stream — whose
				// documented contract is that a CI job can delimit turns by
				// kind — and rendered each child's prompt in the terminal with
				// the same ▌ banner a user submission gets.
				//
				// A child's turn is not a turn of this session. Its evidence
				// belongs here; its frame does not.
				if out.Kind == ui.KindTurnStart || out.Kind == ui.KindTurnEnd {
					continue
				}
				out.Agent = label
				cfg.sink.Emit(out)
			}
		})
	}

	provider, model, err := r.place(cfg, req)
	if err != nil {
		return devcouncil.SubAgentResult{}, err
	}

	// A role's own prompt replaces the parent's. Empty means the role had none
	// — the "self" role, or an untyped fan-out — and the child inherits, which
	// is what those cases want.
	systemPrompt := cfg.systemPrompt
	if strings.TrimSpace(req.SystemPrompt) != "" {
		systemPrompt = req.SystemPrompt
	}
	// A child asked for a judgement is told, in its own prompt, the exact shape
	// the judgement has to arrive in. The instruction and the reader are
	// written together in verdict.go for the reason every parse-the-prose
	// scheme fails: a contract only one side knows about is not a contract.
	if strings.TrimSpace(req.Verdict) != "" {
		systemPrompt = strings.TrimRight(systemPrompt, "\n") + "\n\n" + verdictInstruction()
	}

	loop, err := agent.NewLoop(agent.Config{
		Provider: provider,
		// The same model registry the parent uses, so a child's request gets the
		// same assembly-time capability check. Without it an impossible request
		// surfaces as a 400 mid-turn instead of a refusal before the wire.
		Registry:      cfg.models,
		Model:         model,
		Effort:        cfg.effort,
		EffortCeiling: cfg.effortCeiling,
		SystemPrompt:  systemPrompt,
		MaxSteps:      maxSubAgentSteps,
		// ReadOnly narrows the schemas the child is offered to match the
		// registry it actually has. Both are needed and they answer different
		// questions: this one stops the model wasting steps asking for tools
		// that would come back "unknown", the registry subset above is what
		// makes the answer "unknown" rather than "done".
		ReadOnly:        req.ReadOnly,
		CoreToolsOnly:   cfg.coreToolsOnly,
		AssertInvariant: cfg.assertInvariant,
	}, bus.New(), log, registry)
	if err != nil {
		return devcouncil.SubAgentResult{}, fmt.Errorf("sub-agent %q could not be started: %w", req.Label, err)
	}

	// The child acts under its own lease session, copied from the dispatching
	// agent's so its writes are judged against whatever task that agent holds,
	// and its own from then on. Attached to the context because the context is
	// what already reaches every tool handler; the tools this child calls read
	// their session from there and fall back to the top-level agent's only
	// when nothing attached one.
	//
	// Without this, every concurrently-running child wrote through one shared
	// TaskID and Token. Under the race detector an eight-way fan-out of
	// children checking out eight different tasks reported four data races,
	// and a child holding TASK-C verified itself against TASK-E.
	childCtx := ctx
	if cfg.native != nil {
		childCtx = devcouncil.WithSession(ctx, cfg.native.RootSession().NewChildSession(req.Leases))
	}

	outcome, err := loop.Run(childCtx, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Text: req.Prompt}},
	})
	// Recorded before the error is examined. A child that failed part-way
	// still spent what it spent, and a run that only counts successful
	// children under-reports exactly the turns an operator is investigating.
	spent := devcouncil.SubAgentUsage{
		InputTokens:     outcome.Usage.InputTokens,
		OutputTokens:    outcome.Usage.OutputTokens,
		ReasoningTokens: outcome.Usage.ReasoningTokens,
		CacheReadTokens: outcome.Usage.CacheReadTokens,
	}
	cfg.meter.add(spent)
	if err != nil {
		// The paths travel even on the failure path. A child that wrote three
		// files and then died left three changed files behind, and a parent
		// told nothing about them cannot verify what is now in its tree.
		return devcouncil.SubAgentResult{
			Steps: outcome.Steps, Usage: spent,
			Wrote: outcome.Wrote, WroteTruncated: outcome.WroteTruncated,
		}, fmt.Errorf("sub-agent %q failed: %w", req.Label, err)
	}

	summary := strings.TrimSpace(outcome.Final.Text())
	if summary == "" {
		// The dispatch treats an empty summary as a failure, and it is right
		// to: a child that finished with nothing to say did not do the work its
		// prompt described. Saying so here means the report names the child
		// rather than leaving the caller to infer it from a blank field.
		return devcouncil.SubAgentResult{
				Steps: outcome.Steps, Usage: spent,
				Wrote: outcome.Wrote, WroteTruncated: outcome.WroteTruncated,
			}, fmt.Errorf(
				"sub-agent %q ran %d step(s) and produced no answer", req.Label, outcome.Steps)
	}
	// Everything worth saying about the turn, from the one function that
	// answers that question, rather than a third hand-rolled copy.
	//
	// outcomeNotices was extracted because two faces had drifted apart. There
	// were three: this one covered a truncated answer and the step ceiling and
	// nothing else, so a child whose every write the gate refused, or which ran
	// against a server that could not parse tool calls, or whose pipeline
	// panicked, returned fluent prose with no hint of any of it. The parent
	// consumes that prose as fact — silently returning a partial or unbacked
	// answer as a complete one is the class of lie this type exists to remove.
	//
	// Appended to the summary rather than logged, because the dispatching model
	// is this turn's only reader: the child's own session log is not persisted.
	for _, n := range outcomeNotices(outcome, maxSubAgentSteps) {
		summary += "\n\n[this sub-agent: " + n.Text + "]"
	}

	return devcouncil.SubAgentResult{
		Summary: summary, Steps: outcome.Steps, Usage: spent,
		Wrote: outcome.Wrote, WroteTruncated: outcome.WroteTruncated,
		// Read out of the child's own answer against a stated contract, and
		// reconciled to fail closed. A judging role is asked for one line in a
		// fixed shape; anything else — a missing line, a pass that still lists
		// objections — comes back as no judgement rather than as a pass, which
		// is the only direction it is safe to be wrong in.
		Verdict: parseVerdict(req.Verdict, summary),
	}, nil
}

// spawnSubagentsTool is the dispatching tool a child must never be given.
const spawnSubagentsTool = "devcouncil_spawn_subagents"

// surfaceDeadEnds names the tools a role's own narrowing stranded: offered to
// the child while the prerequisite they cannot succeed without was removed.
//
// tools.Tool.Requires exists because narrowing an offered set was once able to
// build exactly this. `llm.local.core_tools_only` shipped offering
// devcouncil_verify_task, which cannot report anything without a lease, while
// dropping devcouncil_checkout_task, the only tool that takes one — and the
// refusal the model got back named the tool that had just been taken away. An
// allowlist is a second way to build that dead end, from a role definition
// instead of a flag.
//
// The comparison against the floor's own set is the load-bearing part. A
// read-only child already loses devcouncil_checkout_task while keeping the
// read-only devcouncil_verify_task that needs it, so a bare UnsatisfiedIn on
// the narrowed set reports a gap for every read-only child ever dispatched.
// That gap is the caller's read_only, not the role's list, and blaming the role
// for it would turn a true report into noise nobody reads. Only what the role
// added is the role's to answer for.
func surfaceDeadEnds(parent *tools.Registry, floor, narrowed *tools.Registry) []string {
	inherited := map[string]bool{}
	for _, gap := range parent.UnsatisfiedIn(floor.Schemas()) {
		inherited[gap] = true
	}
	var added []string
	for _, gap := range parent.UnsatisfiedIn(narrowed.Schemas()) {
		if !inherited[gap] {
			added = append(added, gap)
		}
	}
	return added
}

// subAgentMeter totals what every sub-agent of one run spent.
//
// It exists because the number a run prints is the number a benchmark records,
// and that number counted the dispatching agent alone. Children do the bulk of
// a fan-out's work and therefore the bulk of its spend: measured on an
// eight-way fan-out, the run reported 2,200 of 38,200 input tokens actually
// consumed. A cost figure that is wrong by seventeen times in the cheap
// direction is worse than none, because it is acted on.
//
// It is a total rather than a per-child ledger because that is what the usage
// line needs; per-child figures already travel back in each fan-out's own tool
// result, where the dispatching model can read them.
type subAgentMeter struct {
	mu    sync.Mutex
	usage devcouncil.SubAgentUsage
}

func (m *subAgentMeter) add(u devcouncil.SubAgentUsage) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usage.Add(u)
}

// Total returns what the children of this run have spent so far.
func (m *subAgentMeter) Total() devcouncil.SubAgentUsage {
	if m == nil {
		return devcouncil.SubAgentUsage{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usage
}
