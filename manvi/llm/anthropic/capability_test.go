package anthropic

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
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
	// Each of these returns a 400 for {type: "disabled"} at every effort.
	for _, model := range []string{"claude-fable-5", "claude-fable-5-1", "claude-opus-5-5", "claude-sonnet-5-5"} {
		err := ValidateThinking(model, ThinkingDisabled, EffortLow)
		if err == nil || !strings.Contains(err.Error(), "always reasons") {
			t.Fatalf("%s: error = %v, want a refusal to disable reasoning", model, err)
		}
	}
	// Haiku 5.5 accepts disabled thinking at high effort or below.
	if err := ValidateThinking("claude-haiku-5-5", ThinkingDisabled, EffortHigh); err != nil {
		t.Fatalf("haiku 5.5 at high: %v", err)
	}
}

func TestCurrentGenerationIsServed(t *testing.T) {
	for _, model := range []string{"claude-fable-5-1", "claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"} {
		c, ok := Capability(model)
		if !ok {
			t.Fatalf("%s is not in the catalogue", model)
		}
		if c.ContextWindow != 1_000_000 || c.MaxOutputTokens != 128_000 || !c.SupportsReasoning {
			t.Errorf("%s = %+v", model, c)
		}
	}
}

// TestEveryServedModelIsPriced keeps the price table and the catalogue in
// step: a budget refuses a model it cannot price, so an unpriced entry is a
// model that works until someone sets a spend cap.
func TestEveryServedModelIsPriced(t *testing.T) {
	for _, model := range Models() {
		p, ok := PriceOf(model)
		if !ok {
			t.Errorf("%s is served but has no price", model)
			continue
		}
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", model, err)
		}
		if !p.ReasoningInOutput {
			t.Errorf("%s: Anthropic's output_tokens includes thinking", model)
		}
		if p.CacheWrite*4 != p.Input*5 {
			t.Errorf("%s: cache write %d is not 1.25x input %d", model, p.CacheWrite, p.Input)
		}
	}
	for model := range prices {
		if _, ok := Capability(model); !ok {
			t.Errorf("%s is priced but not served", model)
		}
	}
}

// TestPublishedRates pins every rate to Anthropic's pricing page
// (platform.claude.com/docs/en/about-claude/pricing, read 2026-10-09) as
// {input, 5m cache write, cache read, output} in $/MTok x 1000, so a change to
// one is a deliberate edit rather than a typo. Sonnet 5.5's cache read is
// $0.10 (0.05x); an earlier transcription from secondary notes said $0.20.
func TestPublishedRates(t *testing.T) {
	for model, want := range map[string][4]int64{
		"claude-fable-5-1":  {10_000, 12_500, 250, 50_000},
		"claude-fable-5":    {10_000, 12_500, 1_000, 50_000},
		"claude-opus-5-5":   {4_000, 5_000, 200, 20_000},
		"claude-opus-5":     {5_000, 6_250, 500, 25_000},
		"claude-opus-4-8":   {5_000, 6_250, 500, 25_000},
		"claude-sonnet-5-5": {2_000, 2_500, 100, 10_000},
		"claude-sonnet-5":   {2_000, 2_500, 200, 10_000},
		"claude-haiku-5-5":  {100, 125, 10, 500},
		"claude-haiku-4-5":  {1_000, 1_250, 100, 5_000},
	} {
		p, ok := PriceOf(model)
		if !ok {
			t.Errorf("%s has no price", model)
			continue
		}
		if got := [4]int64{p.Input, p.CacheWrite, p.CacheRead, p.Output}; got != want {
			t.Errorf("%s rates = %v, want %v", model, got, want)
		}
	}
	haiku, _ := PriceOf("claude-haiku-5-5")
	long := haiku.LongRates
	if haiku.LongPromptAbove != 100_000 || [4]int64{long.Input, long.CacheWrite, long.CacheRead, long.Output} != [4]int64{500, 625, 50, 2_500} {
		t.Errorf("haiku 5.5 long-prompt tier = %+v", haiku)
	}
}

func TestModelsThatDoNotReasonAreSentNoThinkingConfiguration(t *testing.T) {
	a := New("", nil)
	req := llm.Request{Model: "claude-haiku-4-5", MaxTokens: 10, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}}}
	body, err := a.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if body.Thinking != nil {
		t.Fatalf("thinking = %+v; Haiku 4.5 rejects adaptive thinking with a 400", body.Thinking)
	}
	req.Model = "claude-opus-5-5"
	if body, err = a.buildRequest(req); err != nil || body.Thinking == nil || body.Thinking.Type != "adaptive" {
		t.Fatalf("a reasoning model lost its thinking configuration: %+v, %v", body, err)
	}
}

func TestOnlyRefusalsOfTheRequestItselfAreUnbilled(t *testing.T) {
	a := New("", nil)
	for status, want := range map[int]bool{
		400: true, 401: true, 402: true, 403: true, 404: true, 413: true, 429: true, 529: true,
		200: false, 408: false, 409: false, 500: false, 502: false, 503: false, 504: false, 0: false,
	} {
		if got := a.RejectionUnbilled(status); got != want {
			t.Errorf("RejectionUnbilled(%d) = %v, want %v", status, got, want)
		}
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
// llm.PrepareHistoryFor is written generically, and this asserts that, handed
// this adapter, its behaviour coincides with Anthropic's actual rule rather
// than merely resembling it.
func TestNeutralLayerAgreesWithTheProviderRule(t *testing.T) {
	history := []llm.Message{{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "internal", Signature: "sig"},
			llm.TextBlock{Text: "answer"},
		},
		Provenance: &llm.AssistantProvenance{Provider: Name, Model: "claude-opus-5"},
	}}

	same, drops := llm.PrepareHistoryFor(history, &Adapter{}, "claude-opus-5")
	if len(drops) != 0 || len(same[0].Content) != 2 {
		t.Fatal("same model: reasoning must survive, matching ReplayableOn")
	}

	sibling, drops := llm.PrepareHistoryFor(history, &Adapter{}, "claude-opus-4-8")
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
