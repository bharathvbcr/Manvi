package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// stubProvider is a provider that serves a fixed set of model names and records
// every request it is asked to stream. It is what lets a test assert which
// model a child actually reached the wire on, rather than which one the
// dispatch intended.
type stubProvider struct {
	name   string
	serves []string
	seen   []llm.Request
}

func (p *stubProvider) Name() string { return p.name }

func (p *stubProvider) Capability(model string) (llm.Capability, bool) {
	for _, m := range p.serves {
		if m == model {
			return llm.Capability{
				Provider:          p.name,
				Model:             model,
				ContextWindow:     32768,
				MaxOutputTokens:   4096,
				SupportsTools:     true,
				SupportsStreaming: true,
			}, true
		}
	}
	return llm.Capability{}, false
}

func (p *stubProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.seen = append(p.seen, req)
	return &stubStream{}, nil
}

type stubStream struct{ done bool }

func (s *stubStream) Next() (llm.Chunk, error) { return llm.Chunk{}, io.EOF }
func (s *stubStream) Close() error             { return nil }
func (s *stubStream) Response() (llm.Response, error) {
	return llm.Response{
		Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}},
		},
		StopReason: llm.StopEndTurn,
	}, nil
}

// runnerWith builds a runner whose parent runs on `parent` at `parentModel`,
// with every provider registered so a role may name one.
func runnerWith(t *testing.T, parentModel string, providers ...*stubProvider) (*subAgentRunner, *llm.Registry) {
	t.Helper()
	reg := llm.NewRegistry()
	for _, p := range providers {
		if err := reg.Register(p); err != nil {
			t.Fatalf("registering provider %q: %v", p.Name(), err)
		}
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider:     providers[0],
		models:       reg,
		model:        parentModel,
		registry:     tools.NewRegistry(bus.New()),
		systemPrompt: "PARENT PROMPT",
	})
	return r, reg
}

func TestPlaceResolvesRolePlacements(t *testing.T) {
	frontier := &stubProvider{name: "anthropic", serves: []string{"claude-opus-4-5"}}
	local := &stubProvider{name: "local", serves: []string{"qwen3-27b-mlx", "claude-opus-4-5"}}

	for _, tc := range []struct {
		name         string
		spec         string
		wantProvider string
		wantModel    string
	}{
		{name: "empty inherits", spec: "", wantProvider: "anthropic", wantModel: "claude-opus-4-5"},
		{name: "inherit keyword", spec: "inherit", wantProvider: "anthropic", wantModel: "claude-opus-4-5"},
		{name: "qualified switches both", spec: "local/qwen3-27b-mlx",
			wantProvider: "local", wantModel: "qwen3-27b-mlx"},
		// A bare provider carries the parent's model across. It resolves only
		// because this stub local server happens to serve that name; the
		// failing case is asserted separately below.
		{name: "bare provider keeps the model", spec: "local",
			wantProvider: "local", wantModel: "claude-opus-4-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := runnerWith(t, "claude-opus-4-5", frontier, local)
			placed, err := r.place(r.cfg, devcouncil.SubAgentRequest{Label: "w", ModelSpec: tc.spec})
			if err != nil {
				t.Fatalf("place(%q): %v", tc.spec, err)
			}
			provider, model := placed.provider, placed.model
			if provider.Name() != tc.wantProvider || model != tc.wantModel {
				t.Fatalf("place(%q) = %s/%s, want %s/%s",
					tc.spec, provider.Name(), model, tc.wantProvider, tc.wantModel)
			}
		})
	}
}

// A role naming a provider this invocation never enabled must fail, and the
// message must list what is actually available. Falling back to the parent
// would run the work on the frontier model the role was written to avoid.
func TestPlaceRefusesAnUnregisteredProvider(t *testing.T) {
	frontier := &stubProvider{name: "anthropic", serves: []string{"claude-opus-4-5"}}
	r, _ := runnerWith(t, "claude-opus-4-5", frontier)

	_, err := r.place(r.cfg, devcouncil.SubAgentRequest{Label: "w", ModelSpec: "local/qwen3-27b-mlx"})
	if err == nil {
		t.Fatal("a role naming an unenabled provider was accepted")
	}
	if !strings.Contains(err.Error(), "local") || !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("the refusal does not name the missing provider: %v", err)
	}
	if !strings.Contains(err.Error(), "anthropic") {
		t.Errorf("the refusal does not say what is available: %v", err)
	}
}

// A bare provider that does not serve the parent's model must fail with a
// message naming the fix, not run the child on a model the server has never
// heard of.
func TestPlaceRefusesAModelTheProviderDoesNotServe(t *testing.T) {
	frontier := &stubProvider{name: "anthropic", serves: []string{"claude-opus-4-5"}}
	local := &stubProvider{name: "local", serves: []string{"qwen3-27b-mlx"}}
	r, _ := runnerWith(t, "claude-opus-4-5", frontier, local)

	_, err := r.place(r.cfg, devcouncil.SubAgentRequest{Label: "w", ModelSpec: "local"})
	if err == nil {
		t.Fatal("a placement onto a provider that cannot serve the model was accepted")
	}
	if !strings.Contains(err.Error(), "provider/model") {
		t.Errorf("the refusal does not name the fix: %v", err)
	}
}

// The end of the seam: a role's model and prompt must reach the wire, not just
// the dispatch. This is what "opus plans, qwen works" reduces to.
func TestRoleModelAndPromptReachTheWire(t *testing.T) {
	frontier := &stubProvider{name: "anthropic", serves: []string{"claude-opus-4-5"}}
	local := &stubProvider{name: "local", serves: []string{"qwen3-27b-mlx"}}
	r, _ := runnerWith(t, "claude-opus-4-5", frontier, local)

	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label:        "worker",
		Prompt:       "do the work",
		ModelSpec:    "local/qwen3-27b-mlx",
		SystemPrompt: "ROLE PROMPT",
	}); err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}

	if len(frontier.seen) != 0 {
		t.Fatalf("the parent's provider was billed for a child placed on local: %d request(s)", len(frontier.seen))
	}
	if len(local.seen) != 1 {
		t.Fatalf("the local provider saw %d requests, want 1", len(local.seen))
	}
	got := local.seen[0]
	if got.Model != "qwen3-27b-mlx" {
		t.Errorf("wire model = %q, want the role's model", got.Model)
	}
	if got.System != "ROLE PROMPT" {
		t.Errorf("wire system prompt = %q, want the role's prompt", got.System)
	}

	// The tier router reaches the wire as well: one mixed fan-out, dispatched
	// through the real tool, sends the builder to the heavy tier's model and
	// each of seven research children — a fan-out wide enough for basic — to
	// the basic tier's model, each under its own role's prompt.
	t.Run("mixed fan-out", func(t *testing.T) {
		flagReg, err := flags.NewHarnessRegistry("")
		if err != nil {
			t.Fatal(err)
		}
		_, pipeline, err := nativeTools(flagReg)
		if err != nil {
			t.Fatal(err)
		}
		runner, _ := harnessFor(pipeline)
		if runner == nil {
			t.Fatal("the native surface has no sub-agent runner attached")
		}
		p := &tierProvider{name: "anthropic", reasons: map[string][]string{
			"main-model": reasoningLevels, "heavy-model": reasoningLevels,
			"lite-model": reasoningLevels, "basic-model": reasoningLevels,
		}}
		models := llm.NewRegistry()
		if err := models.Register(p); err != nil {
			t.Fatal(err)
		}
		runner.attach(subAgentConfig{
			provider: p, models: models, model: "main-model",
			tiers: agents.TierProfile{
				agents.TierHeavy: {Model: "heavy-model"},
				agents.TierLite:  {Model: "lite-model"},
				agents.TierBasic: {Model: "basic-model"},
			},
			registry: pipeline, systemPrompt: "PARENT PROMPT",
		})

		subs := []map[string]any{{"type_name": "builder", "role": "builder", "prompt": "build it"}}
		for i := 0; i < 7; i++ {
			subs = append(subs, map[string]any{"type_name": "research", "role": "research", "prompt": "survey it"})
		}
		raw, err := json.Marshal(map[string]any{"subagents": subs})
		if err != nil {
			t.Fatal(err)
		}
		res := pipeline.Run(context.Background(), tools.Call{ID: "c1", Name: "devcouncil_invoke_subagent", Arguments: raw})
		if res.IsError {
			t.Fatalf("invoke_subagent: %s", res.Text)
		}

		byModel := map[string]int{}
		for _, req := range p.requests() {
			byModel[req.Model]++
			switch req.Model {
			case "heavy-model":
				if !strings.Contains(req.System, "feature builder") {
					t.Errorf("the heavy-tier request is not the builder's: %q", req.System)
				}
			case "basic-model":
				if !strings.Contains(req.System, "research subagent") {
					t.Errorf("a basic-tier request is not a researcher's: %q", req.System)
				}
			default:
				t.Errorf("a child reached the wire on %q", req.Model)
			}
		}
		if byModel["heavy-model"] != 1 || byModel["basic-model"] != 7 {
			t.Fatalf("requests by model = %v, want 1 on heavy-model and 7 on basic-model", byModel)
		}
	})
}

// A child with no role prompt inherits the parent's. The "self" role and the
// untyped fan-out both depend on this.
func TestAChildWithNoRolePromptInheritsTheParents(t *testing.T) {
	frontier := &stubProvider{name: "anthropic", serves: []string{"claude-opus-4-5"}}
	r, _ := runnerWith(t, "claude-opus-4-5", frontier)

	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label:  "self",
		Prompt: "carry on",
	}); err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}
	if len(frontier.seen) != 1 {
		t.Fatalf("the provider saw %d requests, want 1", len(frontier.seen))
	}
	if got := frontier.seen[0].System; got != "PARENT PROMPT" {
		t.Errorf("wire system prompt = %q, want the parent's", got)
	}
}
