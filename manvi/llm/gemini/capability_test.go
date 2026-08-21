package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"manvi/llm"
)

func TestWireContractMatchesTheCurrentAPI(t *testing.T) {
	if InteractionsPath != "/interactions" {
		t.Fatalf("path = %q; the current API is /v1beta/interactions", InteractionsPath)
	}
	if !strings.HasSuffix(DefaultBaseURL, "/v1beta") {
		t.Fatalf("base URL = %q", DefaultBaseURL)
	}
	// A bespoke header, not an Authorization bearer — the single easiest detail
	// to get wrong when porting an adapter from another provider.
	if APIKeyHeader != "x-goog-api-key" {
		t.Fatalf("auth header = %q", APIKeyHeader)
	}
	if StreamQuery != "alt=sse" {
		t.Fatalf("stream query = %q", StreamQuery)
	}
}

// TestNoRequestCanResumeAnEarlierInteraction is the invariant guard, and it
// replaces one that guarded the wrong thing.
//
// The old test asserted store == false, on the reasoning that server-side
// conversation state would break "the session log is the complete record of
// what the model saw". The goal was right and the mechanism was not: measured
// against the live endpoint, store=false makes every function_result a 400, so
// it did not buy statelessness -- it bought a provider that cannot use tools.
//
// What actually keeps the log complete is that this adapter sends the whole
// history every time and can never reference a stored interaction. That is
// enforced structurally: wireRequest has no previous_interaction_id field, so
// there is no value a caller could set. This test pins that absence, because a
// field added later would reintroduce the real hazard silently.
func TestNoRequestCanResumeAnEarlierInteraction(t *testing.T) {
	encoded, err := json.Marshal(wireRequest{Model: "gemini-3.7-flash"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for name := range fields {
		if strings.Contains(name, "previous") || strings.Contains(name, "interaction_id") {
			t.Fatalf("wireRequest carries %q: a request that can resume a stored "+
				"interaction means the session log is no longer everything the model read", name)
		}
	}
	if _, present := fields["store"]; !present {
		t.Error("store was omitted; the field must be sent explicitly, not left to a default " +
			"that may change")
	}
}

func TestStepEventVocabularyIsPresent(t *testing.T) {
	// The response is a timeline of steps, not candidates with nested parts.
	for _, event := range []string{EventStepStart, EventStepDelta, EventStepStop} {
		if !strings.HasPrefix(event, "step.") {
			t.Errorf("%q is not a step event", event)
		}
	}
	for _, event := range []string{EventInteractionCreated, EventInteractionCompleted, EventInteractionNeedsInput} {
		if !strings.HasPrefix(event, "interaction.") {
			t.Errorf("%q is not an interaction event", event)
		}
	}
	if DeltaText != "text" || DeltaArguments != "arguments" || DeltaThought != "thought" {
		t.Fatal("delta discriminators must match the documented values")
	}
	if DoneSentinel != "[DONE]" || EventDone != "done" {
		t.Fatal("done sentinel and event must match the documented interactions stream terminator")
	}
}

func TestToolWireDiscriminators(t *testing.T) {
	if TypeFunction != "function" || TypeFunctionCall != "function_call" || TypeFunctionResult != "function_result" {
		t.Fatal("tool discriminators must match the documented interactions shapes")
	}
}

func TestOnlyChatModelsAreServed(t *testing.T) {
	// Google publishes image, video, music, embedding, and robotics models on
	// the same index; naming one here must fail at assembly.
	for _, model := range []string{
		"gemini-3.1-flash-image", "veo-3.1-generate-preview",
		"gemini-embedding-001", "lyria-3-pro-preview",
	} {
		if _, ok := Capability(model); ok {
			t.Errorf("%s is not a text chat model", model)
		}
	}
	if _, ok := Capability("gemini-3.7-flash"); !ok {
		t.Fatal("gemini-3.7-flash must be served")
	}
}

func TestUndocumentedLimitsAreNotInvented(t *testing.T) {
	c, _ := Capability("gemini-3.7-flash")
	if c.ContextWindow != 0 || c.MaxOutputTokens != 0 {
		t.Fatalf("limits = %d/%d; the model index does not state them, and a "+
			"fabricated figure would gate requests the provider accepts",
			c.ContextWindow, c.MaxOutputTokens)
	}
}

func TestUnknownModelIsRefused(t *testing.T) {
	err := ValidateRequest(llm.Request{Model: "gemini-4-ultra"})
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("error = %v", err)
	}
}

// TestReplayabilityIsClaimedOnlyForTheSameModel replaces a test that asserted
// no replay path exists at all.
//
// That was true of thought *text*, which this stream never delivers, and false
// of the thing that actually has to travel: the thought signature that
// authorises sending a function_call back. Measured live, a replayed call
// without one is refused and with one is accepted — so claiming nothing is
// replayable made the adapter strip the only state it needed, and the model was
// left continuing from results for calls it could not see.
//
// A signature is minted by one model for one moment of its own reasoning, so it
// does not travel to a different model, and a cross-model handoff must still
// strip it and report the loss.
func TestReplayabilityIsClaimedOnlyForTheSameModel(t *testing.T) {
	if !ReplayableOn("gemini-3.7-flash", "gemini-3.7-flash") {
		t.Error("the same model cannot replay its own call signatures; every tool call would " +
			"be dropped from history and the model would not see what it had done")
	}
	for _, tc := range [][2]string{
		{"gemini-3.7-flash", "gemini-2.5-pro"},
		{"gemini-2.5-pro", "gemini-3.7-flash"},
		{"", "gemini-3.7-flash"},
		{"gemini-3.7-flash", ""},
	} {
		if ReplayableOn(tc[0], tc[1]) {
			t.Errorf("a signature was claimed replayable from %q to %q; it is minted for one "+
				"model's own reasoning", tc[0], tc[1])
		}
	}
}
