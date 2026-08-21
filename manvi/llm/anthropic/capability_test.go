package anthropic

import (
	"strings"
	"testing"

	"manvi/llm"
)

func req(model string) llm.Request {
	return llm.Request{
		Model:    model,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}},
	}
}

// TestDisabledThinkingIsCappedAtHighEffort covers a documented 400 that is
// per-request: a conversation can raise effort on a later turn and start
// failing even though earlier turns succeeded.
func TestDisabledThinkingIsCappedAtHighEffort(t *testing.T) {
	for _, effort := range []string{EffortXHigh, EffortMax} {
		if err := ValidateThinking("claude-opus-5", ThinkingDisabled, effort); err == nil {
			t.Fatalf("disabled thinking at effort %q must be refused", effort)
		}
	}
	for _, effort := range []string{EffortLow, EffortMedium, EffortHigh} {
		if err := ValidateThinking("claude-opus-5", ThinkingDisabled, effort); err != nil {
			t.Fatalf("disabled thinking at effort %q should be permitted: %v", effort, err)
		}
	}
	// Adaptive is never constrained by effort.
	if err := ValidateThinking("claude-opus-5", ThinkingAdaptive, EffortMax); err != nil {
		t.Fatalf("adaptive thinking at max effort should be fine: %v", err)
	}
}

func TestAlwaysThinkingModelRefusesDisabling(t *testing.T) {
	err := ValidateThinking("claude-fable-5", ThinkingDisabled, EffortLow)
	if err == nil || !strings.Contains(err.Error(), "always reasons") {
		t.Fatalf("error = %v, want a refusal to disable reasoning", err)
	}
}

// TestThinkingDefaultDiffersByModel is the silent one: the same request that
// ran without reasoning on Opus 4.8 reasons on Opus 5, and max_tokens caps
// thinking plus response text together.
func TestThinkingDefaultDiffersByModel(t *testing.T) {
	if !ThinkingDefaultRuns("claude-opus-5") {
		t.Fatal("omitting the thinking parameter reasons on claude-opus-5")
	}
	if ThinkingDefaultRuns("claude-opus-4-8") {
		t.Fatal("omitting the thinking parameter does not reason on claude-opus-4-8")
	}
}

func TestSamplingParametersAreRefused(t *testing.T) {
	temp := 0.7
	r := req("claude-opus-5")
	r.Temperature = &temp

	err := ValidateRequest(r, ThinkingAdaptive)
	if err == nil {
		t.Fatal("current models reject sampling parameters; the harness must not send one")
	}
	for _, name := range RemovedParameters {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error should name %q, got %v", name, err)
		}
	}
}

func TestTrailingAssistantMessageIsRefused(t *testing.T) {
	r := req("claude-opus-5")
	r.Messages = append(r.Messages, llm.Message{
		Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: `{"name": "`}},
	})

	err := ValidateRequest(r, ThinkingAdaptive)
	if err == nil || !strings.Contains(err.Error(), "prefill") {
		t.Fatalf("error = %v, want a prefill refusal", err)
	}
}

func TestUnknownModelIsRefusedRatherThanDefaulted(t *testing.T) {
	if _, ok := Capability("claude-opus-4-9"); ok {
		t.Fatal("an unlisted model must be unknown, not assumed")
	}
	err := ValidateRequest(req("claude-opus-4-9"), ThinkingAdaptive)
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("error = %v, want an unknown-model refusal", err)
	}
}

// TestHaikuLimitsAreNotTheFamilyDefault guards the constant most likely to be
// wrong from memory: Haiku is 200K/64K where the rest of the family is 1M/128K,
// and it is the tier search fan-out runs on.
func TestHaikuLimitsAreNotTheFamilyDefault(t *testing.T) {
	haiku, ok := Capability("claude-haiku-4-5")
	if !ok {
		t.Fatal("claude-haiku-4-5 must be in the catalogue")
	}
	if haiku.ContextWindow != 200_000 || haiku.MaxOutputTokens != 64_000 {
		t.Fatalf("haiku = %d ctx / %d out, want 200000 / 64000", haiku.ContextWindow, haiku.MaxOutputTokens)
	}
	if haiku.SupportsReasoning {
		t.Fatal("haiku does not take a reasoning effort")
	}

	opus, _ := Capability("claude-opus-5")
	if opus.ContextWindow == haiku.ContextWindow {
		t.Fatal("the catalogue has flattened Haiku's limits onto the family default")
	}
}

func TestEffortAboveAModelsRangeIsRefused(t *testing.T) {
	r := req("claude-haiku-4-5")
	r.Effort = EffortMax
	if err := ValidateRequest(r, ThinkingAdaptive); err == nil {
		t.Fatal("an effort level on a model without reasoning must be refused at assembly")
	}
}

func TestOutputCapIsEnforcedAtAssembly(t *testing.T) {
	r := req("claude-haiku-4-5")
	r.MaxTokens = 100_000 // above Haiku's 64K cap, below the family's 128K
	if err := ValidateRequest(r, ThinkingAdaptive); err == nil {
		t.Fatal("exceeding a model's output cap must fail before the request is sent")
	}
}

// TestReplayabilityMatchesTheDocumentedRule pins the provider-specific statement
// of the rule the neutral layer implements generally.
func TestReplayabilityMatchesTheDocumentedRule(t *testing.T) {
	if !ReplayableOn("claude-opus-5", "claude-opus-5") {
		t.Fatal("thinking replays on the model that produced it")
	}
	if ReplayableOn("claude-opus-5", "claude-opus-4-8") {
		t.Fatal("a sibling model must not inherit reasoning state")
	}
	if ReplayableOn("", "") {
		t.Fatal("an unknown origin is not replayable")
	}
}

// TestNeutralLayerAgreesWithTheProviderRule is the cross-check that matters:
// llm.PrepareHistory is written generically, and this asserts its behaviour
// coincides with Anthropic's actual rule rather than merely resembling it.
func TestNeutralLayerAgreesWithTheProviderRule(t *testing.T) {
	history := []llm.Message{{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "internal", Signature: "sig"},
			llm.TextBlock{Text: "answer"},
		},
		Provenance: &llm.AssistantProvenance{Provider: Name, Model: "claude-opus-5"},
	}}

	same, drops := llm.PrepareHistory(history, Name, "claude-opus-5")
	if len(drops) != 0 || len(same[0].Content) != 2 {
		t.Fatal("same model: reasoning must survive, matching ReplayableOn")
	}

	sibling, drops := llm.PrepareHistory(history, Name, "claude-opus-4-8")
	if len(drops) != 1 || len(sibling[0].Content) != 1 {
		t.Fatal("sibling model: reasoning must be dropped and reported, matching ReplayableOn")
	}
}

func TestEveryCatalogueEntryIsSelfConsistent(t *testing.T) {
	for model, c := range catalogue {
		if c.Provider != Name || c.Model != model {
			t.Errorf("%s: descriptor disagrees with its key (%s/%s)", model, c.Provider, c.Model)
		}
		if c.ContextWindow <= 0 || c.MaxOutputTokens <= 0 {
			t.Errorf("%s: limits must be positive", model)
		}
		if c.SupportsReasoning && len(c.EffortLevels) == 0 {
			t.Errorf("%s: claims reasoning but lists no effort levels", model)
		}
		if !c.SupportsReasoning && len(c.EffortLevels) > 0 {
			t.Errorf("%s: lists effort levels but claims no reasoning", model)
		}
	}
}
