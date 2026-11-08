package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

// Adversarial review of w13-thinking, items 1, 7 and 8. Wire rules are from the
// claude-api skill's shared/prompt-caching.md, "Mid-conversation system
// messages": a system message follows a user message, is not messages[0], is
// text only, and "must be either the last entry in messages or be followed by
// an assistant turn".

func advAssistant(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: text}}}
}

// A system message followed by a user message is the log a crashed or failed
// turn leaves (user, notice, then the next turn's user). The API would refuse
// it, and ValidateRequest is the one place that can say so before it is sent.
func TestAdvSystemMessageFollowedByAUserMessageIsRefusedBeforeSending(t *testing.T) {
	req := llm.Request{
		Model:    "claude-opus-5-5",
		Messages: []llm.Message{userMsg("hi"), advAssistant("a"), userMsg("again"), sysMsg("notice"), userMsg("next")},
	}
	if err := ValidateRequest(req, ThinkingAdaptive); err == nil {
		t.Fatal("accepted [user, system, user]: the documented rule is last entry or followed by an assistant turn")
	}
}

func TestAdvAdjacentSystemMessagesAreRefused(t *testing.T) {
	req := llm.Request{
		Model:    "claude-opus-5-5",
		Messages: []llm.Message{userMsg("hi"), sysMsg("one"), sysMsg("two")},
	}
	if err := ValidateRequest(req, ThinkingAdaptive); err == nil {
		t.Fatal("accepted two adjacent system messages")
	}
}

// The placements the loop produces must stay accepted: after a tool_result user
// message (the documented example), at the end, and before an assistant turn.
func TestAdvDocumentedSystemMessagePlacementsAreAccepted(t *testing.T) {
	toolRound := []llm.Message{
		userMsg("go"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolCallBlock{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultBlock{ToolCallID: "c1", Content: llm.Content{llm.TextBlock{Text: "ok"}}}}},
		sysMsg("tools changed"),
	}
	for name, msgs := range map[string][]llm.Message{
		"after a tool_result":       toolRound,
		"before an assistant turn":  {userMsg("hi"), sysMsg("x"), advAssistant("a"), userMsg("n"), sysMsg("y")},
		"at the end after one user": {userMsg("hi"), sysMsg("x")},
	} {
		if err := ValidateRequest(llm.Request{Model: "claude-fable-5-1", Messages: msgs}, ThinkingAdaptive); err != nil {
			t.Errorf("%s: refused a documented placement: %v", name, err)
		}
	}
}

// Item 8. Through the public Stream path, a model that takes no system messages
// (Sonnet 5, Haiku 4.5) or is unknown must be refused before any byte reaches the
// server: a system message cannot reach the wire for it by any route.
func TestAdvSystemMessageNeverReachesTheWireForAnUnsupportedModel(t *testing.T) {
	for _, model := range []string{"claude-sonnet-5", "claude-haiku-4-5", "claude-opus-5-5-20261001", "claude-fable-6"} {
		t.Run(model, func(t *testing.T) {
			adapter, server := adapterFor(t, minimalStream)
			_, err := adapter.Stream(adaptertest.Ctx(), llm.Request{
				Model: model, MaxTokens: 100,
				Messages: []llm.Message{userMsg("hi"), sysMsg("context")},
			})
			if err == nil {
				t.Fatal("the request was accepted")
			}
			if n := len(server.Requests()); n != 0 {
				t.Errorf("%d requests reached the server", n)
			}
		})
	}
}

// Item 8, the catalogue. Haiku 5.5 supports the message per
// shared/prompt-caching.md ("... and Claude Haiku 5.5" in the per-turn reminder
// paragraph) and model-migration.md "Migrating to Claude Haiku 5.5"; its row in
// platform-availability.md omits it. Sonnet 5 and Haiku 4.5 are unsupported in
// every source. The catalogue must say exactly that.
func TestAdvMidConversationSystemSupportMatchesTheDocs(t *testing.T) {
	want := map[string]bool{
		"claude-fable-5-1": true, "claude-fable-5": true, "claude-opus-5-5": true, "claude-opus-5": true,
		"claude-opus-4-8": true, "claude-sonnet-5-5": true, "claude-haiku-5-5": true,
		"claude-sonnet-5": false, "claude-haiku-4-5": false,
	}
	for _, model := range Models() {
		w, listed := want[model]
		if !listed {
			t.Errorf("%s is in the catalogue but not in this review's table of the docs", model)
			continue
		}
		c, ok := Capability(model)
		if !ok || c.MidConversationSystem != w {
			t.Errorf("%s: MidConversationSystem = %v, docs say %v", model, c.MidConversationSystem, w)
		}
	}
}

// Item 1. tool_choice none rides alongside thinking and a replayed signed block
// on the 5.x models: it is the only tool_choice ever sent, and the request that
// carries it is otherwise the same bytes as one without it.
func TestAdvToolChoiceNoneWithThinkingChangesNothingElse(t *testing.T) {
	tool := llm.ToolSchema{Name: "echo", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}
	history := []llm.Message{
		userMsg("hi"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "t", Signature: "sig"}, llm.TextBlock{Text: "a"}}},
		userMsg("again"),
	}
	send := func(noTools bool) map[string]json.RawMessage {
		adapter, server := adapterFor(t, minimalStream)
		adapter.Thinking = ThinkingAdaptive
		stream, err := adapter.Stream(adaptertest.Ctx(), llm.Request{
			Model: "claude-sonnet-5-5", MaxTokens: 100, System: "s", Tools: []llm.ToolSchema{tool},
			Messages: history, NoToolCalls: noTools,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := adaptertest.Drain(stream); err != nil {
			t.Fatal(err)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(server.Requests()[0]), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	with, without := send(true), send(false)
	if string(with["tool_choice"]) != `{"type":"none"}` {
		t.Fatalf("tool_choice = %s, want none", with["tool_choice"])
	}
	if _, present := without["tool_choice"]; present {
		t.Errorf("tool_choice sent on an ordinary request: %s", without["tool_choice"])
	}
	if !strings.Contains(string(with["thinking"]), "adaptive") {
		t.Errorf("thinking = %s, want adaptive alongside tool_choice none", with["thinking"])
	}
	for _, key := range []string{"system", "tools", "messages", "thinking", "model"} {
		if string(with[key]) != string(without[key]) {
			t.Errorf("%s differs between the none request and the ordinary one:\n%s\n%s", key, with[key], without[key])
		}
	}
}

// NoToolCalls with no tools has nothing to forbid, and tool_choice with no tools
// is a 400 on the API; it must not be sent.
func TestAdvToolChoiceIsNotSentWithoutTools(t *testing.T) {
	adapter, server := adapterFor(t, minimalStream)
	stream, err := adapter.Stream(adaptertest.Ctx(), llm.Request{
		Model: "claude-opus-5-5", MaxTokens: 100, Messages: []llm.Message{userMsg("hi")}, NoToolCalls: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adaptertest.Drain(stream); err != nil {
		t.Fatal(err)
	}
	if body := server.Requests()[0]; strings.Contains(body, "tool_choice") {
		t.Errorf("tool_choice sent with no tools declared:\n%s", body)
	}
}
