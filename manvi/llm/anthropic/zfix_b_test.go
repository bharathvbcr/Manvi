package anthropic

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

// The encoder omits an assistant message left with nothing to send (empty or
// whitespace text, thinking cut off before its signature). A system message
// followed by such a message is therefore followed by the next user message on
// the wire: user, system, user, which shared/prompt-caching.md ("Mid-
// conversation system messages") refuses. Placement is checked on the form that
// is sent, so the mismatch is refused here and no request goes out.
func TestAdvPlacementIsValidatedOnTheWireForm(t *testing.T) {
	hollow := map[string]llm.Message{
		"whitespace text":   {Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "  "}}},
		"unsigned thinking": {Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "cut off"}}},
	}
	for name, h := range hollow {
		t.Run(name, func(t *testing.T) {
			adapter, server := adapterFor(t, minimalStream)
			_, err := adapter.Stream(adaptertest.Ctx(), llm.Request{
				Model: "claude-opus-5-5", MaxTokens: 100,
				Messages: []llm.Message{userMsg("hi"), sysMsg("notice"), h, userMsg("next")},
			})
			if err == nil {
				t.Fatal("sent user, system, user: the API refuses it and so would every later request")
			}
			if n := len(server.Requests()); n != 0 {
				t.Errorf("%d requests reached the server", n)
			}
		})
	}
}

// A system message whose answering turn survives encoding is sent.
func TestAdvPlacementAcceptsAnAnswerThatSurvivesEncoding(t *testing.T) {
	signed := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "t", Signature: "sig"}}}
	redacted := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ReasoningBlock{Redacted: true, Signature: "blob"}}}
	for name, a := range map[string]llm.Message{"signed thinking": signed, "redacted thinking": redacted} {
		t.Run(name, func(t *testing.T) {
			adapter, _ := adapterFor(t, minimalStream)
			if _, err := adapter.Stream(adaptertest.Ctx(), llm.Request{
				Model: "claude-opus-5-5", MaxTokens: 100,
				Messages: []llm.Message{userMsg("hi"), sysMsg("notice"), a, userMsg("next")},
			}); err != nil {
				t.Fatalf("a notice answered by an encodable turn was refused: %v", err)
			}
		})
	}
}

// The encoder and llm.Message.HasSendableContent are one predicate, not two that
// drift: the log decides which assistant messages exist with the second, the
// wire sends the ones the first keeps.
func TestEncoderKeepsExactlyWhatHasSendableContent(t *testing.T) {
	for name, blocks := range map[string][]llm.ContentBlock{
		"text":               {llm.TextBlock{Text: "x"}},
		"empty text":         {llm.TextBlock{}},
		"whitespace text":    {llm.TextBlock{Text: " \t\n"}},
		"signed thinking":    {llm.ReasoningBlock{Text: "t", Signature: "s"}},
		"unsigned thinking":  {llm.ReasoningBlock{Text: "t"}},
		"redacted thinking":  {llm.ReasoningBlock{Redacted: true, Signature: "d"}},
		"unsigned then text": {llm.ReasoningBlock{Text: "t"}, llm.TextBlock{Text: "x"}},
		"hollow pair":        {llm.ReasoningBlock{Text: "t"}, llm.TextBlock{Text: " "}},
		"tool call":          {llm.ToolCallBlock{ID: "c", Name: "n"}},
		"no blocks":          nil,
	} {
		msg := llm.Message{Role: llm.RoleAssistant, Content: blocks}
		_, keep, err := toWireMessage(msg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if keep != msg.HasSendableContent() {
			t.Errorf("%s: encoder keeps %v, predicate says %v", name, keep, msg.HasSendableContent())
		}
	}
}
