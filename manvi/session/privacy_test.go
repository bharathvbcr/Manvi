package session

import (
	"encoding/json"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"strings"
	"testing"
)

func TestScrubDecodedStringsWithoutChangingOpaqueContinuation(t *testing.T) {
	secret := "member \"A\"\\雪\n42"
	opaque := json.RawMessage(`{"signature":"opaque-marker","steps":[]}`)
	l := NewLog()
	l.SetScrubber(func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, secret, "[redacted]"), "opaque-marker", "[redacted]")
	})
	mustAppend(t, l, UserMessage, MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "start"}}}})
	if _, err := l.DeriveMessages(); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Provenance: &llm.AssistantProvenance{Provider: "gemini", Model: "model", ReplayState: opaque}, Content: []llm.ContentBlock{llm.TextBlock{Text: secret}}}})
	mustAppend(t, l, ToolResult, ToolResultData{ToolCallID: "call", Text: secret})
	for _, log := range []*Log{l, func() *Log {
		r, err := RestoreLog(l.Events())
		if err != nil {
			t.Fatal(err)
		}
		return r
	}()} {
		messages, err := log.DeriveMessages()
		if err != nil {
			t.Fatal(err)
		}
		text, ok := messages[1].Content[0].(llm.TextBlock)
		if !ok || text.Text != "[redacted]" {
			t.Errorf("escaped secret was not scrubbed")
		}
		if string(messages[1].Provenance.ReplayState) != string(opaque) {
			t.Errorf("opaque continuation was modified")
		}
		result, ok := messages[2].Content[0].(llm.ToolResultBlock)
		if !ok {
			t.Fatal("missing result")
		}
		text, ok = result.Content[0].(llm.TextBlock)
		if !ok || text.Text != "[redacted]" {
			t.Error("result escaped secret was not scrubbed")
		}
	}
}

func TestPublicEventsAndObserversOmitPrivateContinuation(t *testing.T) {
	l := NewLog()
	var observed Event
	l.Observe(func(e Event) { observed = e })
	private := json.RawMessage(`{"signature":"opaque-only-private"}`)
	mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Provenance: &llm.AssistantProvenance{Provider: "gemini", Model: "model", ReplayState: private}, Content: []llm.ContentBlock{llm.TextBlock{Text: "visible"}}}})
	public, err := l.PublicEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Event{public[0], observed} {
		if strings.Contains(string(e.Data), "replay_state") || strings.Contains(string(e.Data), "opaque-only-private") {
			t.Fatal("private continuation leaked into public projection")
		}
	}
	if !strings.Contains(string(l.Events()[0].Data), "opaque-only-private") {
		t.Fatal("private resumable journal lost continuation")
	}
	public[0].Data[0] = '!'
	if !json.Valid(l.Events()[0].Data) {
		t.Fatal("public reader mutated private journal")
	}
}

func TestTextScrubberDoesNotRewriteBinaryOrSignatureFields(t *testing.T) {
	l := NewLog()
	l.SetScrubber(func(s string) string { return strings.ReplaceAll(s, "AQID", "[redacted]") })
	_, err := l.Append(AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}}, llm.ReasoningBlock{Text: "AQID", Signature: "AQID"}}}})
	if err != nil {
		t.Fatalf("text scrubber corrupted binary payload: %v", err)
	}
	messages, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	reasoning, ok := messages[0].Content[1].(llm.ReasoningBlock)
	if !ok || reasoning.Text != "[redacted]" || reasoning.Signature != "AQID" {
		t.Fatal("opaque signature changed or visible reasoning not scrubbed")
	}
	public, err := l.PublicEvents()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public[0].Data), "signature") {
		t.Fatal("private reasoning signature exported")
	}
}
