package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// tierProvider serves a fixed set of models, some of which reason, and records
// every request under a lock so a concurrent fan-out can be asserted on.
type tierProvider struct {
	name string
	// reasons maps each served model to the effort levels it accepts; nil
	// levels mean the model does not reason.
	reasons map[string][]string
	script  []llm.Response

	mu   sync.Mutex
	seen []llm.Request
}

func (p *tierProvider) Name() string { return p.name }

func (p *tierProvider) Capability(model string) (llm.Capability, bool) {
	levels, ok := p.reasons[model]
	if !ok {
		return llm.Capability{}, false
	}
	return llm.Capability{
		Provider: p.name, Model: model,
		ContextWindow: 32768, MaxOutputTokens: 4096,
		SupportsTools: true, SupportsStreaming: true,
		SupportsReasoning: len(levels) > 0, EffortLevels: levels,
	}, true
}

func (p *tierProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, req)
	resp := endTurn("done")
	if n := len(p.script); n > 0 {
		resp = p.script[n-1]
		if turn := len(p.seen) - 1; turn < n {
			resp = p.script[turn]
		}
	}
	return &scriptedStream{resp: resp}, nil
}

func (p *tierProvider) requests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.seen...)
}

var reasoningLevels = []string{"low", "medium", "high"}

// D2. The checkpoint critic was dispatched with a label, a prompt and a verdict
// marker and nothing else, so it held the parent's whole write surface and ran
// on the parent's model. It must run as the read-only critic role, on the
// advisor tier.
//
// The write tool's absence is proved from the registry, not the offered
// schemas: the child names fake_write anyway, and Registry.Run dispatches by
// name whether or not the schema was offered.
func TestTheCheckpointCriticIsAReadOnlyAdvisor(t *testing.T) {
	p := &tierProvider{
		name:    "anthropic",
		reasons: map[string][]string{"parent-model": reasoningLevels, "advisor-model": reasoningLevels},
		script: []llm.Response{
			wantToolCall("fake_write", `{}`),
			endTurn("reviewed\n" + devcouncil.VerdictMarker + " FAIL\n- the lock is never released"),
		},
	}
	models := llm.NewRegistry()
	if err := models.Register(p); err != nil {
		t.Fatal(err)
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: p, models: models, model: "parent-model",
		tiers:        agents.TierProfile{agents.TierAdvisor: {Model: "advisor-model"}},
		registry:     surfaceRegistry(t),
		systemPrompt: "PARENT PROMPT",
	})

	s, _ := newSensor(t, &fakeVerifier{report: devcouncil.PathReport{
		Verdict: devcouncil.VerdictFailed, Findings: []string{"still broken"},
	}})
	s.runner = r
	e := &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"a.go"}, Bounce: 1}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	seen := p.requests()
	if len(seen) < 2 {
		t.Fatalf("the critic made %d request(s); it never got an answer to its tool call", len(seen))
	}
	for i, req := range seen {
		if req.Model != "advisor-model" {
			t.Errorf("critic request %d went to %q, want the advisor tier's model", i, req.Model)
		}
	}
	for _, schema := range seen[0].Tools {
		if schema.Name == "fake_write" || strings.HasPrefix(schema.Name, "fake_mcp") {
			t.Errorf("the critic was offered %s", schema.Name)
		}
	}
	answer := ""
	for _, block := range seen[len(seen)-1].Messages[len(seen[len(seen)-1].Messages)-1].Content {
		if result, ok := block.(llm.ToolResultBlock); ok {
			for _, nested := range result.Content {
				if text, ok := nested.(llm.TextBlock); ok {
					answer += text.Text
				}
			}
		}
	}
	if strings.Contains(answer, "ran fake_write") || !strings.Contains(answer, "unknown tool") {
		t.Fatalf("the critic's call to fake_write reached a handler: %q", answer)
	}
	if !strings.Contains(seen[0].System, "critic") {
		t.Errorf("the critic ran without the critic role's prompt: %q", seen[0].System)
	}
	if !strings.Contains(e.Inject, "the lock is never released") {
		t.Errorf("the critic's verdict did not reach the parent: %q", e.Inject)
	}
}

// D3. A child placed on a tier runs at that tier's effort. Carrying the
// parent's effort and ceiling across put a child on a model that does not
// reason with a ceiling it can never climb, and the loop refused it before
// its first step.
func TestAChildOnANonReasoningTierStartsWithItsOwnEffort(t *testing.T) {
	p := &tierProvider{
		name: "anthropic",
		reasons: map[string][]string{
			"parent-model": reasoningLevels,
			"basic-model":  nil,
			"lite-model":   reasoningLevels,
			"heavy-model":  reasoningLevels,
		},
	}
	models := llm.NewRegistry()
	if err := models.Register(p); err != nil {
		t.Fatal(err)
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: p, models: models, model: "parent-model",
		effort: "low", effortCeiling: "high",
		tiers: agents.TierProfile{
			agents.TierBasic: {Model: "basic-model"},
			agents.TierLite:  {Model: "lite-model", Effort: "medium"},
			agents.TierHeavy: {Model: "heavy-model"},
			agents.TierMain:  {Model: "parent-model"},
		},
		registry:     surfaceRegistry(t),
		systemPrompt: "PARENT PROMPT",
	})

	for _, tc := range []struct {
		tier       agents.Tier
		wantModel  string
		wantEffort string
	}{
		// Does not reason: no effort, and no ceiling to refuse it with.
		{tier: agents.TierBasic, wantModel: "basic-model", wantEffort: ""},
		// The tier sets its own effort.
		{tier: agents.TierLite, wantModel: "lite-model", wantEffort: "medium"},
		// The tier sets none and its model reasons: the parent's effort.
		{tier: agents.TierHeavy, wantModel: "heavy-model", wantEffort: "low"},
		// A tier on the parent's own model is the parent's placement.
		{tier: agents.TierMain, wantModel: "parent-model", wantEffort: "low"},
	} {
		before := len(p.requests())
		if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
			Label: string(tc.tier), Prompt: "look", ReadOnly: true, Tier: tc.tier,
		}); err != nil {
			t.Fatalf("a child on the %s tier did not start: %v", tc.tier, err)
		}
		seen := p.requests()[before:]
		if len(seen) == 0 {
			t.Fatalf("the %s child made no request", tc.tier)
		}
		if seen[0].Model != tc.wantModel || seen[0].Effort != tc.wantEffort {
			t.Errorf("%s child: wire model=%q effort=%q, want %q at %q",
				tc.tier, seen[0].Model, seen[0].Effort, tc.wantModel, tc.wantEffort)
		}
	}
}

// The same class on an operator's own placement: a role pinned to a model that
// does not reason keeps no effort it cannot honour, and the result says so.
func TestAnOperatorPlacementDropsAnEffortItsModelCannotHonour(t *testing.T) {
	p := &tierProvider{
		name:    "anthropic",
		reasons: map[string][]string{"parent-model": reasoningLevels, "plain-model": nil},
	}
	models := llm.NewRegistry()
	if err := models.Register(p); err != nil {
		t.Fatal(err)
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: p, models: models, model: "parent-model",
		effort: "low", effortCeiling: "high",
		registry: surfaceRegistry(t), systemPrompt: "PARENT PROMPT",
	})
	out, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "pinned", Prompt: "look", ModelSpec: "plain-model",
	})
	if err != nil {
		t.Fatalf("a child the operator placed on a non-reasoning model did not start: %v", err)
	}
	if got := p.requests()[0]; got.Model != "plain-model" || got.Effort != "" {
		t.Errorf("wire model=%q effort=%q, want plain-model with no effort", got.Model, got.Effort)
	}
	if !strings.Contains(out.Summary, `parent's effort "low"`) {
		t.Errorf("the dropped effort was not reported: %q", out.Summary)
	}
}

// A tier the profile does not hold, or no profile at all, inherits the
// parent's model and effort: tiers only resolve where a profile is set.
func TestATierWithNoProfileInheritsTheParent(t *testing.T) {
	p := &tierProvider{name: "gemini", reasons: map[string][]string{"parent-model": reasoningLevels}}
	models := llm.NewRegistry()
	if err := models.Register(p); err != nil {
		t.Fatal(err)
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: p, models: models, model: "parent-model", effort: "low",
		registry: surfaceRegistry(t), systemPrompt: "PARENT PROMPT",
	})
	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "w", Prompt: "work", Tier: agents.TierHeavy,
	}); err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}
	if got := p.requests()[0]; got.Model != "parent-model" || got.Effort != "low" {
		t.Errorf("wire model=%q effort=%q, want the parent's", got.Model, got.Effort)
	}
}

// A tier whose model the session's provider does not serve is refused, not run
// on the parent's model under the tier's name.
func TestATierTheProviderDoesNotServeIsRefused(t *testing.T) {
	p := &tierProvider{name: "anthropic", reasons: map[string][]string{"parent-model": reasoningLevels}}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: p, model: "parent-model",
		tiers:    agents.TierProfile{agents.TierLite: {Model: "missing-model"}},
		registry: surfaceRegistry(t), systemPrompt: "PARENT PROMPT",
	})
	_, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "w", Prompt: "work", Tier: agents.TierLite,
	})
	if err == nil || !strings.Contains(err.Error(), "missing-model") {
		t.Fatalf("a tier on an unserved model was not refused by name: %v", err)
	}
	if len(p.requests()) != 0 {
		t.Fatal("the refused child reached the wire")
	}
}

// `manvi agents` shows every shipped role with its tier and that tier's model.
func TestShowAgentsPrintsRoleTierAndModel(t *testing.T) {
	t.Setenv("MANVI_MODEL", "")
	reg, err := newRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Set(flags.Human, flags.LLMDefaultProvider, "anthropic"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := showAgents(&out, reg); err != nil {
		t.Fatal(err)
	}
	profile := agents.DefaultTierProfile()
	for _, def := range agents.NewRegistry().List() {
		want := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(def.Name) + `\s+` +
			regexp.QuoteMeta(string(def.Tier)) + `\s+` + regexp.QuoteMeta(profile[def.Tier].Model) + `\s`)
		if !want.MatchString(out.String()) {
			t.Errorf("no row %s → %s → %s in:\n%s", def.Name, def.Tier, profile[def.Tier].Model, out.String())
		}
	}
	if !strings.Contains(out.String(), "(main agent)") {
		t.Errorf("the main agent's row is missing:\n%s", out.String())
	}
}

// The critic's role narrows the real native surface — read-only and without
// the MCP group — and that narrowing must not strand a tool, or every
// escalation would be refused before the critic ran. The fakes above cannot
// show that; this runs it over the surface a session actually has.
func TestTheCheckpointCriticRunsOverTheNativeSurface(t *testing.T) {
	flagReg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	native, pipeline, err := nativeTools(flagReg)
	if err != nil {
		t.Fatal(err)
	}
	runner, _ := harnessFor(pipeline)
	if runner == nil {
		t.Fatal("the native surface has no sub-agent runner attached")
	}
	p := &tierProvider{
		name:    "anthropic",
		reasons: map[string][]string{"parent-model": reasoningLevels, "advisor-model": reasoningLevels},
		script:  []llm.Response{endTurn("reviewed\n" + devcouncil.VerdictMarker + " PASS")},
	}
	models := llm.NewRegistry()
	if err := models.Register(p); err != nil {
		t.Fatal(err)
	}
	runner.attach(subAgentConfig{
		provider: p, models: models, model: "parent-model",
		tiers:    agents.TierProfile{agents.TierAdvisor: {Model: "advisor-model"}},
		registry: pipeline, systemPrompt: "PARENT PROMPT",
	})

	s, _ := newSensor(t, &fakeVerifier{report: devcouncil.PathReport{
		Verdict: devcouncil.VerdictFailed, Findings: []string{"still broken"},
	}})
	s.runner, s.roles = runner, native.Roles()
	e := &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"a.go"}, Bounce: 1}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.Inject, "could not complete") {
		t.Fatalf("the critic could not run over the native surface: %q", e.Inject)
	}
	seen := p.requests()
	if len(seen) == 0 || seen[0].Model != "advisor-model" {
		t.Fatalf("the critic did not reach the advisor tier's model: %d request(s)", len(seen))
	}
	readOnly := map[string]bool{}
	for _, schema := range pipeline.ReadOnlySchemas() {
		readOnly[schema.Name] = true
	}
	for _, schema := range seen[0].Tools {
		if !readOnly[schema.Name] {
			t.Errorf("the critic was offered the mutating tool %s", schema.Name)
		}
	}
}
