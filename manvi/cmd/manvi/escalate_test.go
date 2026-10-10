package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// modelProvider answers each request from a function of the model it was sent
// to, and records every request under a lock.
type modelProvider struct {
	reasons map[string][]string
	answer  func(ctx context.Context, req llm.Request, n int) (llm.Response, error)

	mu   sync.Mutex
	seen []llm.Request
	byM  map[string]int
}

func (p *modelProvider) Name() string { return "anthropic" }

func (p *modelProvider) Capability(model string) (llm.Capability, bool) {
	levels, ok := p.reasons[model]
	if !ok {
		return llm.Capability{}, false
	}
	return llm.Capability{
		Provider: "anthropic", Model: model, ContextWindow: 32768, MaxOutputTokens: 4096,
		SupportsTools: true, SupportsStreaming: true,
		SupportsReasoning: len(levels) > 0, EffortLevels: levels,
	}, true
}

func (p *modelProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	p.seen = append(p.seen, req)
	if p.byM == nil {
		p.byM = map[string]int{}
	}
	p.byM[req.Model]++
	n := p.byM[req.Model]
	p.mu.Unlock()
	resp, err := p.answer(ctx, req, n)
	if err != nil {
		return nil, err
	}
	return &scriptedStream{resp: resp}, nil
}

func (p *modelProvider) requests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.seen...)
}

func (p *modelProvider) count(model string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byM[model]
}

var ladderProfile = agents.TierProfile{
	agents.TierBasic:   {Model: "basic-model"},
	agents.TierLite:    {Model: "lite-model"},
	agents.TierHeavy:   {Model: "heavy-model"},
	agents.TierMain:    {Model: "heavy-model"},
	agents.TierAdvisor: {Model: "advisor-model"},
}

func ladderRunner(t *testing.T, p *modelProvider) (*subAgentRunner, *subAgentMeter) {
	t.Helper()
	models := llm.NewRegistry()
	if err := models.Register(p); err != nil {
		t.Fatal(err)
	}
	meter := &subAgentMeter{}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: p, models: models, model: "heavy-model", tiers: ladderProfile,
		registry: surfaceRegistry(t), systemPrompt: "PARENT PROMPT", meter: meter,
	})
	return r, meter
}

func allModels() map[string][]string {
	return map[string][]string{
		"basic-model": nil, "lite-model": nil, "heavy-model": nil, "advisor-model": nil,
	}
}

// The ladder moves one tier up, skipping a tier that runs the same model, and
// stops at the advisor.
func TestNextTierClimbsToADifferentModelAndStopsAtTheAdvisor(t *testing.T) {
	for _, tc := range []struct {
		from, want agents.Tier
		ok         bool
	}{
		{agents.TierBasic, agents.TierLite, true},
		{agents.TierLite, agents.TierHeavy, true},
		// main is heavy's own model: not a rung.
		{agents.TierHeavy, agents.TierAdvisor, true},
		{agents.TierMain, agents.TierAdvisor, true},
		{agents.TierAdvisor, "", false},
		{"nonsense", "", false},
	} {
		got, ok := nextTier(ladderProfile, tc.from)
		if got != tc.want || ok != tc.ok {
			t.Errorf("nextTier(%s) = %q, %v; want %q, %v", tc.from, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := nextTier(nil, agents.TierLite); ok {
		t.Error("a nil profile has a ladder")
	}
}

// A child that comes back empty on lite is re-run once on heavy from a fresh
// conversation: the original prompt and an account of what failed, none of the
// first model's thinking, and a report naming the move.
func TestAnEmptyAnswerIsReRunOnTheNextTierFromAFreshConversation(t *testing.T) {
	p := &modelProvider{reasons: allModels()}
	p.answer = func(_ context.Context, req llm.Request, _ int) (llm.Response, error) {
		if req.Model == "lite-model" {
			resp := endTurn("")
			resp.Message.Content = append([]llm.ContentBlock{llm.ReasoningBlock{Text: "lite thinking"}}, resp.Message.Content...)
			resp.Usage = llm.Usage{InputTokens: 10, OutputTokens: 1, CacheWriteTokens: 100}
			return resp, nil
		}
		resp := endTurn("the real answer")
		resp.Usage = llm.Usage{InputTokens: 20, OutputTokens: 2, CacheWriteTokens: 300}
		return resp, nil
	}
	r, meter := ladderRunner(t, p)

	out, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "scan", Prompt: "find the leak", ReadOnly: true, Tier: agents.TierLite,
		Escalation: devcouncil.NewEscalationBudget(1),
	})
	if err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}
	if !strings.Contains(out.Summary, "the real answer") {
		t.Fatalf("the re-run's answer was not reported: %q", out.Summary)
	}
	if len(out.Escalations) != 1 {
		t.Fatalf("escalations = %+v, want one", out.Escalations)
	}
	if e := out.Escalations[0]; e.Child != "scan" || e.From != agents.TierLite || e.To != agents.TierHeavy ||
		!strings.Contains(e.Evidence, "empty summary") || !e.ColdCache {
		t.Errorf("escalation record = %+v", e)
	}

	seen := p.requests()
	if len(seen) != 2 || seen[0].Model != "lite-model" || seen[1].Model != "heavy-model" {
		t.Fatalf("requests went to %v, want lite then heavy", modelsOf(seen))
	}
	rerun := seen[1]
	if len(rerun.Messages) != 1 {
		t.Fatalf("the re-run replayed a transcript: %d messages", len(rerun.Messages))
	}
	for _, m := range rerun.Messages {
		for _, b := range m.Content {
			if _, thinking := b.(llm.ReasoningBlock); thinking {
				t.Fatal("the escalated request carries a thinking block from the previous model")
			}
		}
	}
	text := rerun.Messages[0].Content[0].(llm.TextBlock).Text
	if !strings.HasPrefix(text, "find the leak") || !strings.Contains(text, "empty summary") {
		t.Errorf("the re-run's prompt = %q", text)
	}
	// Cache writes of both attempts are in the child's usage, and the meter
	// keeps each model's own.
	if out.Usage.CacheWriteTokens != 400 {
		t.Errorf("child cache writes = %d, want both attempts' 400", out.Usage.CacheWriteTokens)
	}
	byModel := map[string]int{}
	for _, u := range meter.ByModel() {
		byModel[u.Model] = u.CacheWriteTokens
	}
	if byModel["anthropic/lite-model"] != 100 || byModel["anthropic/heavy-model"] != 300 {
		t.Errorf("per-model cache writes = %v", byModel)
	}
}

func modelsOf(reqs []llm.Request) []string {
	var out []string
	for _, r := range reqs {
		out = append(out, r.Model)
	}
	return out
}

// A re-run that shows the same evidence ends the ladder: a typed exhausted
// error, two requests and no third.
func TestAReRunThatIsEmptyAgainIsExhausted(t *testing.T) {
	p := &modelProvider{reasons: allModels()}
	p.answer = func(context.Context, llm.Request, int) (llm.Response, error) { return endTurn(""), nil }
	r, _ := ladderRunner(t, p)

	out, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "scan", Prompt: "p", ReadOnly: true, Tier: agents.TierBasic,
		Escalation: devcouncil.NewEscalationBudget(1),
	})
	var exhausted *devcouncil.ExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("err = %v, want *ExhaustedError", err)
	}
	if exhausted.From != agents.TierBasic || exhausted.To != agents.TierLite {
		t.Errorf("exhausted %s -> %s", exhausted.From, exhausted.To)
	}
	if got := len(p.requests()); got != 2 {
		t.Fatalf("%d requests; one fresh re-run is the whole ladder", got)
	}
	if len(out.Escalations) != 1 {
		t.Errorf("the exhausted child's report lost its escalation: %+v", out.Escalations)
	}
}

// The advisor is the ceiling, and a child with no tier or an operator's pinned
// model has no ladder: each leaves the first attempt exactly as it was.
func TestChildrenThatCannotClimbAreNotReRun(t *testing.T) {
	for name, req := range map[string]devcouncil.SubAgentRequest{
		"advisor":  {Tier: agents.TierAdvisor},
		"no tier":  {},
		"operator": {Tier: agents.TierLite, ModelSpec: "lite-model"},
		"no cap":   {Tier: agents.TierLite},
	} {
		t.Run(name, func(t *testing.T) {
			p := &modelProvider{reasons: allModels()}
			p.answer = func(context.Context, llm.Request, int) (llm.Response, error) { return endTurn(""), nil }
			r, _ := ladderRunner(t, p)
			req.Label, req.Prompt, req.ReadOnly = "c", "p", true
			if name != "no cap" {
				req.Escalation = devcouncil.NewEscalationBudget(1)
			}
			_, err := r.RunSubAgent(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "produced no answer") {
				t.Fatalf("err = %v, want the first attempt's own failure", err)
			}
			var exhausted *devcouncil.ExhaustedError
			if errors.As(err, &exhausted) {
				t.Fatal("a child that never climbed was reported exhausted")
			}
			if got := len(p.requests()); got != 1 {
				t.Fatalf("%d requests, want 1", got)
			}
		})
	}
}

// Eight children failing at once, one shared cap: exactly ceil(8/2) re-runs.
func TestAFanOutsEscalationsAreCappedUnderConcurrency(t *testing.T) {
	p := &modelProvider{reasons: allModels()}
	p.answer = func(_ context.Context, req llm.Request, _ int) (llm.Response, error) {
		if req.Model == "lite-model" {
			return endTurn(""), nil
		}
		return endTurn("ok"), nil
	}
	r, _ := ladderRunner(t, p)
	budget := devcouncil.NewEscalationBudget(8)

	var wg sync.WaitGroup
	var escalated atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, _ := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
				Label: "c", Prompt: "p", ReadOnly: true, Tier: agents.TierLite, Escalation: budget,
			})
			if len(out.Escalations) > 0 {
				escalated.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := escalated.Load(); got != 4 || budget.Cap() != 4 {
		t.Fatalf("%d children escalated under a cap of %d, want 4", got, budget.Cap())
	}
	if got := p.count("heavy-model"); got != 4 {
		t.Fatalf("%d re-runs reached the wire, want 4", got)
	}
}

// A provider failure on the re-run is a typed error that stays on the rung: no
// further request, on any model.
func TestAFailedReRunIsATypedErrorAndDoesNotClimbFurther(t *testing.T) {
	down := errors.New("anthropic: 529 overloaded")
	p := &modelProvider{reasons: allModels()}
	p.answer = func(_ context.Context, req llm.Request, _ int) (llm.Response, error) {
		if req.Model == "lite-model" {
			return endTurn(""), nil
		}
		return llm.Response{}, down
	}
	r, _ := ladderRunner(t, p)
	_, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "c", Prompt: "p", ReadOnly: true, Tier: agents.TierLite,
		Escalation: devcouncil.NewEscalationBudget(1),
	})
	var typed *devcouncil.EscalationError
	if !errors.As(err, &typed) || !errors.Is(err, down) {
		t.Fatalf("err = %v, want an *EscalationError wrapping the provider's", err)
	}
	if typed.From != agents.TierLite || typed.To != agents.TierHeavy {
		t.Errorf("escalation error %s -> %s", typed.From, typed.To)
	}
	if p.count("advisor-model") != 0 {
		t.Fatal("a failed re-run climbed to the advisor")
	}
}

// Cancelling the parent while the re-run is in flight stops it promptly and
// reports the cancellation on the typed error.
func TestCancellingDuringAReRunStopsIt(t *testing.T) {
	started := make(chan struct{})
	p := &modelProvider{reasons: allModels()}
	p.answer = func(ctx context.Context, req llm.Request, _ int) (llm.Response, error) {
		if req.Model == "lite-model" {
			return endTurn(""), nil
		}
		close(started)
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}
	r, _ := ladderRunner(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.RunSubAgent(ctx, devcouncil.SubAgentRequest{
			Label: "c", Prompt: "p", ReadOnly: true, Tier: agents.TierLite,
			Escalation: devcouncil.NewEscalationBudget(1),
		})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the cancellation", err)
		}
		var typed *devcouncil.EscalationError
		if !errors.As(err, &typed) {
			t.Fatalf("err = %v, want an *EscalationError", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the re-run did not stop when the parent was cancelled")
	}
}

// A parent already cancelled when the first attempt ends starts no re-run.
func TestACancelledParentStartsNoReRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &modelProvider{reasons: allModels()}
	p.answer = func(context.Context, llm.Request, int) (llm.Response, error) {
		cancel()
		return endTurn(""), nil
	}
	r, _ := ladderRunner(t, p)
	_, _ = r.RunSubAgent(ctx, devcouncil.SubAgentRequest{
		Label: "c", Prompt: "p", ReadOnly: true, Tier: agents.TierLite,
		Escalation: devcouncil.NewEscalationBudget(1),
	})
	if got := len(p.requests()); got != 1 {
		t.Fatalf("%d requests after cancellation, want 1", got)
	}
}

// Prompt caching: a tier's system prompt is the same bytes on every step of
// the child's turn, and the re-run on another tier carries the same role
// prompt, so only the model's own cache is cold.
func TestTheRoleSystemPromptIsByteIdenticalAcrossStepsAndTiers(t *testing.T) {
	p := &modelProvider{reasons: allModels()}
	p.answer = func(_ context.Context, req llm.Request, n int) (llm.Response, error) {
		switch {
		case req.Model == "lite-model" && n == 1:
			return wantToolCall("fake_read", `{}`), nil
		case req.Model == "lite-model":
			return endTurn(""), nil
		}
		return endTurn("ok"), nil
	}
	r, _ := ladderRunner(t, p)
	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "c", Prompt: "p", ReadOnly: true, Tier: agents.TierLite,
		SystemPrompt: "ROLE PROMPT", Escalation: devcouncil.NewEscalationBudget(1),
	}); err != nil {
		t.Fatal(err)
	}
	seen := p.requests()
	if len(seen) != 3 {
		t.Fatalf("%d requests, want two lite steps and one heavy", len(seen))
	}
	for i, req := range seen {
		if req.System != seen[0].System {
			t.Errorf("request %d system prompt differs from the first:\n%q\n%q", i, req.System, seen[0].System)
		}
	}
	if !strings.Contains(seen[0].System, "ROLE PROMPT") {
		t.Errorf("system prompt lost the role: %q", seen[0].System)
	}
}
