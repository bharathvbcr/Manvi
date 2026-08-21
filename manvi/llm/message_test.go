package llm

import (
	"context"
	"encoding/json"
	"testing"
)

func assistantWith(provider, model string, blocks ...ContentBlock) Message {
	return Message{
		Role:       RoleAssistant,
		Content:    blocks,
		Provenance: &AssistantProvenance{Provider: provider, Model: model, ReplayState: json.RawMessage(`{"sig":"abc"}`)},
	}
}

// TestReplayStateSurvivesTheSameModel is the property the whole provenance
// mechanism exists for: an adapter gets its own lossless state back.
func TestReplayStateSurvivesTheSameModel(t *testing.T) {
	history := []Message{
		{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: "think"}}},
		assistantWith("anthropic", "m1",
			ReasoningBlock{Text: "internal", Signature: "sig"},
			TextBlock{Text: "answer"}),
	}

	out, drops := PrepareHistory(history, "anthropic", "m1")
	if len(drops) != 0 {
		t.Fatalf("same provider and model must drop nothing, got %v", drops)
	}
	if len(out[1].Content) != 2 {
		t.Fatal("reasoning must survive on the model that produced it")
	}
	if len(out[1].Provenance.ReplayState) == 0 {
		t.Fatal("replay state must survive on the same model")
	}
}

// TestCrossProviderHandoffIsExplicitlyLossy is the other half. The drop is
// reported so the loop can log it — an accidental drop and a deliberate one
// look identical downstream, so the difference has to be recorded here.
func TestCrossProviderHandoffIsExplicitlyLossy(t *testing.T) {
	history := []Message{
		assistantWith("anthropic", "m1",
			ReasoningBlock{Text: "internal", Signature: "sig"},
			TextBlock{Text: "answer"}),
	}

	out, drops := PrepareHistory(history, "gemini", "g1")
	if len(drops) != 1 {
		t.Fatalf("expected one reported drop, got %d", len(drops))
	}
	drop := drops[0]
	if drop.FromProvider != "anthropic" || drop.ToProvider != "gemini" {
		t.Fatalf("drop = %+v", drop)
	}
	if !drop.ReplayState || len(drop.Blocks) != 1 || drop.Blocks[0] != string(KindReasoning) {
		t.Fatalf("drop must name what was lost, got %+v", drop)
	}

	if len(out[0].Content) != 1 {
		t.Fatal("reasoning must be stripped for a provider that cannot replay it")
	}
	if out[0].Content[0].Kind() != KindText {
		t.Fatal("visible text must survive")
	}
	if len(out[0].Provenance.ReplayState) != 0 {
		t.Fatal("replay state must not cross a provider boundary")
	}
	if out[0].Provenance.Provider != "anthropic" {
		t.Fatal("provenance itself is preserved so the record still says who produced it")
	}
}

// TestSameProviderDifferentModelAlsoDrops: providers tie thinking state to a
// specific model. Replaying it onto a sibling is the failure this prevents.
func TestSameProviderDifferentModelAlsoDrops(t *testing.T) {
	history := []Message{assistantWith("anthropic", "m1", ReasoningBlock{Text: "x"})}
	_, drops := PrepareHistory(history, "anthropic", "m2")
	if len(drops) != 1 {
		t.Fatalf("a sibling model must not inherit reasoning state, got %d drops", len(drops))
	}
}

func TestUserMessagesAreNeverTouched(t *testing.T) {
	history := []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: "hi"}}}}
	out, drops := PrepareHistory(history, "gemini", "g1")
	if len(drops) != 0 || len(out[0].Content) != 1 {
		t.Fatal("user messages carry no provider state and must pass through unchanged")
	}
}

func TestContentBlockRoundTrip(t *testing.T) {
	original := Message{
		Role: RoleAssistant,
		Content: []ContentBlock{
			TextBlock{Text: "visible"},
			ReasoningBlock{Text: "thinking", Signature: "sig", Redacted: true},
			ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}},
			ToolCallBlock{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"p":"a"}`)},
			ToolResultBlock{ToolCallID: "c1", Content: []ContentBlock{TextBlock{Text: "out"}}, IsError: true},
		},
		Provenance: &AssistantProvenance{Provider: "anthropic", Model: "m1"},
	}

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Message
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}

	if len(decoded.Content) != len(original.Content) {
		t.Fatalf("round trip lost blocks: %d -> %d", len(original.Content), len(decoded.Content))
	}
	for i := range original.Content {
		if decoded.Content[i].Kind() != original.Content[i].Kind() {
			t.Fatalf("block %d changed kind: %s -> %s", i,
				original.Content[i].Kind(), decoded.Content[i].Kind())
		}
	}
	reasoning := decoded.Content[1].(ReasoningBlock)
	if reasoning.Signature != "sig" || !reasoning.Redacted {
		t.Fatalf("reasoning fields lost: %+v", reasoning)
	}
	result := decoded.Content[4].(ToolResultBlock)
	if !result.IsError || len(result.Content) != 1 {
		t.Fatalf("nested tool result lost: %+v", result)
	}
}

// TestUnknownBlockKindIsAnError: silently skipping a block a newer binary wrote
// would make an old binary's replay quietly differ from the original run.
func TestUnknownBlockKindIsAnError(t *testing.T) {
	var msg Message
	err := json.Unmarshal([]byte(`{"role":"assistant","content":[{"kind":"hologram"}]}`), &msg)
	if err == nil {
		t.Fatal("an unknown block kind must be an error, not a skipped block")
	}
}

func TestCapabilityValidateRefusesImpossibleRequests(t *testing.T) {
	c := Capability{
		Provider: "p", Model: "m",
		SupportsTools: false, SupportsReasoning: true,
		EffortLevels: []string{"low", "high"}, MaxOutputTokens: 1000,
	}

	if err := c.Validate(Request{Tools: []ToolSchema{{Name: "x"}}}); err == nil {
		t.Fatal("tools on a model without tool support must fail")
	}
	if err := c.Validate(Request{Effort: "extreme"}); err == nil {
		t.Fatal("an unsupported effort level must fail")
	}
	if err := c.Validate(Request{MaxTokens: 5000}); err == nil {
		t.Fatal("exceeding the output cap must fail")
	}
	if err := c.Validate(Request{Effort: "high", MaxTokens: 500}); err != nil {
		t.Fatalf("a serviceable request must pass: %v", err)
	}

	images := Request{Messages: []Message{{Content: []ContentBlock{ImageBlock{}}}}}
	if err := c.Validate(images); err == nil {
		t.Fatal("images on a text-only model must fail")
	}
}

func TestRegistryRefusesDuplicateProviderNames(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubProvider{name: "dup"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(stubProvider{name: "dup"}); err == nil {
		t.Fatal("two adapters answering to one name would make provenance ambiguous")
	}
}

func TestResolveNamesWhatIsWrong(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubProvider{name: "p", model: "known"}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := r.Resolve("absent", Request{Model: "known"}); err == nil {
		t.Fatal("an unknown provider must fail")
	}
	if _, _, err := r.Resolve("p", Request{Model: "unknown"}); err == nil {
		t.Fatal("an unknown model must fail rather than defaulting permissively")
	}
	if _, _, err := r.Resolve("p", Request{Model: "known"}); err != nil {
		t.Fatalf("a serviceable call must resolve: %v", err)
	}
}

type stubProvider struct {
	name  string
	model string
}

func (s stubProvider) Name() string { return s.name }
func (s stubProvider) Capability(model string) (Capability, bool) {
	if model != s.model {
		return Capability{}, false
	}
	return Capability{Provider: s.name, Model: model, SupportsTools: true}, true
}
func (s stubProvider) Stream(context.Context, Request) (Stream, error) { return nil, nil }
