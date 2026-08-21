package llm

import (
	"context"
	"testing"
)

// neverReplays models an OpenAI-compatible adapter: the wire has no field that
// carries a thinking block back, so nothing is replayable, not even onto the
// model that produced it.
type neverReplays struct{ name string }

func (n neverReplays) Name() string { return n.name }
func (n neverReplays) Capability(string) (Capability, bool) {
	return Capability{Provider: n.name}, true
}
func (n neverReplays) Stream(context.Context, Request) (Stream, error) { return nil, nil }
func (n neverReplays) ReplayableOn(from, to string) bool               { return false }

// sameModelReplays models a hosted API that signs thinking per model.
type sameModelReplays struct{ name string }

func (s sameModelReplays) Name() string { return s.name }
func (s sameModelReplays) Capability(string) (Capability, bool) {
	return Capability{Provider: s.name}, true
}
func (s sameModelReplays) Stream(context.Context, Request) (Stream, error) { return nil, nil }
func (s sameModelReplays) ReplayableOn(from, to string) bool               { return from == to }

// silentProvider implements no opinion, so the pre-interface behaviour applies.
type silentProvider struct{ name string }

func (s silentProvider) Name() string { return s.name }
func (s silentProvider) Capability(string) (Capability, bool) {
	return Capability{Provider: s.name}, true
}
func (s silentProvider) Stream(context.Context, Request) (Stream, error) { return nil, nil }

func reasoningHistory(provider, model string) []Message {
	return []Message{{
		Role:       RoleAssistant,
		Provenance: &AssistantProvenance{Provider: provider, Model: model},
		Content: []ContentBlock{
			ReasoningBlock{Text: "thinking about the edit"},
			TextBlock{Text: "here is the plan"},
		},
	}}
}

func countReasoning(messages []Message) int {
	n := 0
	for _, m := range messages {
		for _, b := range m.Content {
			if b.Kind() == KindReasoning {
				n++
			}
		}
	}
	return n
}

// The defect this locks: a local model that thinks out loud produced a
// ReasoningBlock, PrepareHistory kept it because the model matched, and the
// OpenAI-compatible adapter then refused the whole message — so the turn died
// on the step after the model first reasoned.
func TestReasoningIsNotReplayedToAnAdapterThatCannotCarryIt(t *testing.T) {
	const model = "mlx-community/Qwen3.8-27B-4bit"
	history := reasoningHistory("local", model)

	out, drops := PrepareHistoryFor(history, neverReplays{name: "local"}, model)
	if got := countReasoning(out); got != 0 {
		t.Fatalf("reasoning survived onto an adapter that cannot carry it: %d block(s)", got)
	}
	if len(drops) != 1 {
		t.Fatalf("the removal was not reported: %d drop(s)", len(drops))
	}
	if len(out[0].Content) != 1 {
		t.Fatalf("expected the text block to survive, got %d block(s)", len(out[0].Content))
	}
}

func TestReasoningIsReplayedWhenTheAdapterSaysItCan(t *testing.T) {
	history := reasoningHistory("anthropic", "claude-opus-5")

	same, drops := PrepareHistoryFor(history, sameModelReplays{name: "anthropic"}, "claude-opus-5")
	if countReasoning(same) != 1 {
		t.Fatal("reasoning was dropped on the model that produced it")
	}
	if len(drops) != 0 {
		t.Fatalf("unexpected drops: %v", drops)
	}

	sibling, drops := PrepareHistoryFor(history, sameModelReplays{name: "anthropic"}, "claude-opus-4-8")
	if countReasoning(sibling) != 0 {
		t.Fatal("reasoning was replayed onto a sibling model")
	}
	if len(drops) != 1 {
		t.Fatalf("the sibling drop was not reported: %v", drops)
	}
}

// An adapter with no opinion keeps the behaviour that predates the interface,
// so adding it cannot silently change a provider nobody updated.
func TestProviderWithoutAnOpinionKeepsSameModelReplay(t *testing.T) {
	history := reasoningHistory("legacy", "m1")

	same, _ := PrepareHistoryFor(history, silentProvider{name: "legacy"}, "m1")
	if countReasoning(same) != 1 {
		t.Fatal("same-model reasoning was dropped for a provider that stated no policy")
	}
	other, _ := PrepareHistoryFor(history, silentProvider{name: "legacy"}, "m2")
	if countReasoning(other) != 0 {
		t.Fatal("cross-model reasoning survived for a provider that stated no policy")
	}
}

// A nil provider is a programming error, and the response to one must not be
// to quietly throw the conversation away.
//
// Returning nil here read as "refusing", but no caller can tell that apart from
// "this history is empty": the turn goes out with no context and nothing says
// why. Handing the messages back unchanged keeps the failure where it belongs —
// on the nil provider the caller is about to use — instead of converting it
// into silent context loss. agent.NewLoop rejects a nil provider, so this path
// is unreachable in a real run; that is the reason to make it harmless rather
// than the reason to leave it destructive.
func TestPrepareHistoryForDoesNotDiscardHistoryForANilProvider(t *testing.T) {
	in := reasoningHistory("local", "m")
	out, drops := PrepareHistoryFor(in, nil, "m")
	if len(out) != len(in) {
		t.Fatalf("a nil provider discarded history: %d messages in, %d out", len(in), len(out))
	}
	if len(drops) != 0 {
		t.Fatalf("a nil provider reported %d drop(s); it transformed nothing", len(drops))
	}
}

// An assistant turn whose only content is reasoning must be omitted, not sent
// hollow.
//
// Reachable, not theoretical: a local server cut off inside a <think> block
// produces exactly this message, and llm/local reports reasoning as never
// replayable, so the block is always stripped. Left in place the message
// reaches the wire with zero content blocks and the encoder refuses it —
// "message has no sendable content" — which kills the turn. Worse, the message
// stays in the session log, so every later request in that session fails the
// same way, including after a --resume.
func TestReasoningOnlyMessageIsOmittedRatherThanSentHollow(t *testing.T) {
	history := []Message{
		{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: "go"}}},
		{
			Role:       RoleAssistant,
			Provenance: &AssistantProvenance{Provider: "local", Model: "qwen3"},
			Content:    []ContentBlock{ReasoningBlock{Text: "cut off mid-thought"}},
		},
		{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: "continue"}}},
	}

	out, drops := PrepareHistory(history, "local", "other-model")

	for i, msg := range out {
		if len(msg.Content) == 0 {
			t.Fatalf("message %d (%s) survived with no content; no wire can carry it", i, msg.Role)
		}
	}
	if len(out) != 2 {
		t.Fatalf("expected the reasoning-only message to be omitted, got %d messages", len(out))
	}
	// The omission has to be reported, or history shrinks with no record.
	var emptied int
	for _, d := range drops {
		if d.Emptied {
			emptied++
		}
	}
	if emptied != 1 {
		t.Fatalf("expected 1 drop reporting an emptied message, got %d (drops: %v)", emptied, drops)
	}

	// A message that still has sendable content keeps it and is not omitted.
	mixed := []Message{{
		Role:       RoleAssistant,
		Provenance: &AssistantProvenance{Provider: "local", Model: "qwen3"},
		Content: []ContentBlock{
			ReasoningBlock{Text: "thinking"},
			TextBlock{Text: "the answer"},
		},
	}}
	kept, _ := PrepareHistory(mixed, "local", "other-model")
	if len(kept) != 1 || len(kept[0].Content) != 1 {
		t.Fatalf("a message with sendable content was altered: %+v", kept)
	}
}
