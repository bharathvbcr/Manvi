package xai

import (
	"strings"
	"testing"

	"manvi/llm"
)

func req(model string) llm.Request {
	return llm.Request{Model: model}
}

func TestEndpointAndSentinelMatchTheDocumentedWire(t *testing.T) {
	if DefaultBaseURL != "https://api.x.ai/v1" {
		t.Fatalf("base URL = %q", DefaultBaseURL)
	}
	if ChatCompletionsPath != "/chat/completions" {
		t.Fatalf("path = %q", ChatCompletionsPath)
	}
	if DoneSentinel != "[DONE]" {
		t.Fatalf("sentinel = %q; the OpenAI-compatible stream ends on [DONE]", DoneSentinel)
	}
}

// TestReasoningIsPerModelNotFamilyWide is the trap in an OpenAI-compatible API:
// the request shape is uniform, the accepted parameters are not.
func TestReasoningIsPerModelNotFamilyWide(t *testing.T) {
	reasoning, ok := Capability("grok-4.3")
	if !ok || !reasoning.SupportsReasoning {
		t.Fatal("grok-4.3 is documented as supporting reasoning_effort")
	}
	plain, ok := Capability("grok-4.6")
	if !ok {
		t.Fatal("grok-4.6 must be in the catalogue")
	}
	if plain.SupportsReasoning {
		t.Fatal("reasoning_effort must not be assumed family-wide")
	}

	r := req("grok-4.6")
	r.Effort = EffortHigh
	if err := ValidateRequest(r); err == nil {
		t.Fatal("an effort on a model without reasoning must fail at assembly")
	}
}

func TestUndocumentedOutputCapIsNotInvented(t *testing.T) {
	c, _ := Capability("grok-4.6")
	if c.MaxOutputTokens != 0 {
		t.Fatalf("MaxOutputTokens = %d; xAI does not document one, and a fabricated cap "+
			"would reject requests the provider accepts", c.MaxOutputTokens)
	}
	// "Not stated" must therefore not enforce.
	r := req("grok-4.6")
	r.MaxTokens = 1 << 20
	if err := ValidateRequest(r); err != nil {
		t.Fatalf("an undocumented cap must not gate: %v", err)
	}
}

func TestContextWindowsAreNotFlattened(t *testing.T) {
	for model, want := range map[string]int{
		"grok-4.6":       500_000,
		"grok-4.3":       1_000_000,
		"grok-build-0.1": 256_000,
	} {
		c, ok := Capability(model)
		if !ok {
			t.Fatalf("%s missing from the catalogue", model)
		}
		if c.ContextWindow != want {
			t.Errorf("%s context = %d, want %d", model, c.ContextWindow, want)
		}
	}
}

func TestNonChatModelsAreNotServed(t *testing.T) {
	// xAI publishes image and video models; this adapter drives chat
	// completions, so naming one must fail at assembly.
	for _, model := range []string{"grok-imagine-image", "grok-imagine-video"} {
		if _, ok := Capability(model); ok {
			t.Errorf("%s is not a chat-completions model", model)
		}
	}
}

func TestUnknownModelIsRefused(t *testing.T) {
	err := ValidateRequest(req("grok-9"))
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("error = %v", err)
	}
}

func TestReplayabilityIsClaimedOnlyWhenDocumented(t *testing.T) {
	if ReplayableOn("grok-4.6", "grok-4.6") {
		t.Fatal("xAI documents no cross-turn reasoning replay; claiming it would " +
			"produce silent quality loss instead of a reported drop")
	}
}
