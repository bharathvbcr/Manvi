package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// criticFixture builds a serveCritic over a scripted provider and a tool
// registry that holds a write tool beside a read-only one. The provider's first
// turn calls the write tool, which is what a critic that tries to fix the code
// it was asked to judge would do.
func criticFixture(t *testing.T, wrote *atomic.Bool) (*serveCritic, *replay.Provider) {
	t.Helper()
	return criticFixtureOn(t, wrote, nil)
}

// criticFixtureOn is criticFixture on a session whose model is "m" and whose
// tier profile is tiers. The provider serves "m" and every tier model a test
// names, so a request's Model says which one the critic was placed on.
func criticFixtureOn(t *testing.T, wrote *atomic.Bool, tiers agents.TierProfile) (*serveCritic, *replay.Provider) {
	t.Helper()
	served := []llm.Capability{}
	for _, model := range []string{"m", "advisor-model", "claude-opus-5-5"} {
		served = append(served, llm.Capability{
			Provider: "replay", Model: model, ContextWindow: 32768,
			MaxOutputTokens: 4096, SupportsTools: true, SupportsStreaming: true,
		})
	}
	provider := replay.New(replay.Fixture{
		Provider:     "replay",
		Capabilities: served,
		Turns: []replay.Turn{
			{
				Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.ToolCallBlock{ID: "w1", Name: "write_thing", Arguments: json.RawMessage(`{}`)},
				}},
				StopReason: llm.StopToolUse,
			},
			{
				Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.TextBlock{Text: "I could not change anything.\nMANVI-VERDICT[1]: PASS\n"},
				}},
				StopReason: llm.StopEndTurn,
			},
		},
	})
	models := llm.NewRegistry()
	if err := models.Register(provider); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry(bus.New())
	for _, tool := range []tools.Tool{
		{
			Schema:   llm.ToolSchema{Name: "read_thing", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Handler:  func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "{}"} },
			ReadOnly: true,
		},
		{
			Schema: llm.ToolSchema{Name: "write_thing", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Handler: func(context.Context, tools.Call) tools.Result {
				wrote.Store(true)
				return tools.Result{Text: "written"}
			},
		},
	} {
		if err := registry.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	runner := &subAgentRunner{}
	runner.attach(subAgentConfig{
		provider: provider, models: models, model: "m", tiers: tiers,
		registry: registry, systemPrompt: "parent", meter: &subAgentMeter{},
	})
	def, ok := agents.NewRegistry().Get(criticRoleName)
	if !ok {
		t.Fatal("no built-in critic role")
	}
	return &serveCritic{build: func(context.Context) (*criticRuntime, error) {
		return &criticRuntime{runner: runner, def: def}, nil
	}}, provider
}

// The critic is read-only by absence. The registry the child runs with has no
// write tool, so asking for one is asking for a tool that does not exist — not
// asking for one that refuses.
func TestServeCriticHasNoWriteToolsAndAWriteAttemptFindsNone(t *testing.T) {
	var wrote atomic.Bool
	critic, provider := criticFixture(t, &wrote)

	summary, err := critic.Judge(context.Background(), "judge the change", 1)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if wrote.Load() {
		t.Fatal("the critic's write tool ran")
	}
	if !strings.Contains(summary, "MANVI-VERDICT[1]: PASS") {
		t.Fatalf("summary = %q", summary)
	}

	requests := provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("the provider saw %d requests, want 2", len(requests))
	}
	offered := map[string]bool{}
	for _, schema := range requests[0].Tools {
		offered[schema.Name] = true
	}
	if offered["write_thing"] {
		t.Fatalf("the critic was offered the write tool: %v", offered)
	}
	if !offered["read_thing"] {
		t.Fatalf("the critic was not offered the read-only tool: %v", offered)
	}
	// The write attempt came back as an error, not as a result.
	var refused bool
	for _, message := range requests[1].Messages {
		for _, block := range message.Content {
			if result, ok := block.(llm.ToolResultBlock); ok && result.ToolCallID == "w1" && result.IsError {
				refused = true
			}
		}
	}
	if !refused {
		t.Fatal("the write attempt was not refused")
	}
}

// The judging role's own prompt carries the per-criterion contract, so the
// reader and the instruction are the same package's two halves.
func TestServeCriticIsToldThePerCriterionVerdictContract(t *testing.T) {
	var wrote atomic.Bool
	critic, provider := criticFixture(t, &wrote)
	if _, err := critic.Judge(context.Background(), "judge", 3); err != nil {
		t.Fatal(err)
	}
	system := provider.Requests()[0].System
	for _, want := range []string{"MANVI-VERDICT[n]: PASS", "3 numbered acceptance criteria", "adversarial"} {
		if !strings.Contains(strings.ToLower(system), strings.ToLower(want)) {
			t.Errorf("system prompt lacks %q:\n%s", want, system)
		}
	}
}

// A build failure is not remembered: the provider that was unreachable once may
// be reachable next time, and a cached error would degrade every verification
// until the host restarted.
func TestServeCriticDoesNotCacheABuildFailure(t *testing.T) {
	var wrote atomic.Bool
	good, _ := criticFixture(t, &wrote)
	calls := 0
	critic := &serveCritic{build: func(ctx context.Context) (*criticRuntime, error) {
		calls++
		if calls == 1 {
			return nil, context.DeadlineExceeded
		}
		return good.build(ctx)
	}}
	if _, err := critic.Judge(context.Background(), "judge", 1); err == nil {
		t.Fatal("the first build failure was swallowed")
	}
	if _, err := critic.Judge(context.Background(), "judge", 1); err != nil {
		t.Fatalf("the failure was cached: %v", err)
	}
}

// requireCriticModel asserts every request the critic made went to want.
func requireCriticModel(t *testing.T, provider *replay.Provider, want string) {
	t.Helper()
	seen := provider.Requests()
	if len(seen) == 0 {
		t.Fatal("the critic made no request")
	}
	for i, req := range seen {
		if req.Model != want {
			t.Errorf("critic request %d went to %q, want %q", i, req.Model, want)
		}
	}
}

// The verification critic is a verdict, so the router places it on the advisor
// tier. It was dispatched with no tier, and a child with no tier runs on the
// session's model, which left the owner's tier profile bypassed.
func TestServeCriticRunsOnTheAdvisorTierNotTheSessionModel(t *testing.T) {
	var wrote atomic.Bool
	critic, provider := criticFixtureOn(t, &wrote, agents.TierProfile{
		agents.TierAdvisor: {Model: "advisor-model"},
		agents.TierLite:    {Model: "claude-opus-5-5"},
	})
	if _, err := critic.Judge(context.Background(), "judge the change", 1); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	requireCriticModel(t, provider, "advisor-model")
	if wrote.Load() {
		t.Fatal("the critic's write tool ran on the advisor tier")
	}
}

// An operator's advisor override, read through the same profile `manvi run`
// reads, is where the critic runs: nothing here names a model.
func TestServeCriticHonoursAnOperatorAdvisorOverride(t *testing.T) {
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierAdvisor)), "claude-opus-5-5")
	table, err := checkTiers(tierRegistry(t), anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	var wrote atomic.Bool
	critic, provider := criticFixtureOn(t, &wrote, table.Profile())
	if _, err := critic.Judge(context.Background(), "judge the change", 1); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	requireCriticModel(t, provider, "claude-opus-5-5")
}

// A session with no profile (a provider the tiers do not apply on) keeps the
// critic on the session's model, read-only all the same.
func TestServeCriticWithNoTierProfileStaysOnTheSessionModel(t *testing.T) {
	var wrote atomic.Bool
	critic, provider := criticFixtureOn(t, &wrote, nil)
	if _, err := critic.Judge(context.Background(), "judge the change", 1); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	requireCriticModel(t, provider, "m")
	if wrote.Load() {
		t.Fatal("the critic's write tool ran")
	}
}

// An advisor tier this binary cannot run must not stop serve starting: the
// sidecar also serves the board, the code graph and scans. Serve constructs the
// critic as it always did, and verification is what degrades, naming the tier on
// every call because a failed build is not cached. Nothing listens at the base
// URL, so a transport error here would mean the provider was contacted before
// the profile was refused.
func TestServeCriticStartsOnABadTierAndEveryVerificationNamesIt(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-fixture-critic")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("MANVI_MODEL", "")
	t.Setenv(flags.EnvKey(flags.LLMDefaultProvider), anthropic.Name)
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierAdvisor)), "claude-fable-9")

	critic := newServeCritic(tierRegistry(t))
	if critic == nil {
		t.Fatal("newServeCritic produced no critic")
	}
	for call := 1; call <= 2; call++ {
		_, err := critic.Judge(context.Background(), "judge the change", 1)
		requireTierRefusal(t, err, agents.TierAdvisor, "claude-fable-9", "catalogue")
	}
}
