package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

// Adversarial review of fix-b. The predicate llm.Message.HasSendableContent is
// the single "survives encoding" test the log, the stale pass and ValidateRequest
// share with the encoder. Every assistant shape the stream decoder can produce
// or a journal can hold is run through the whole adapter, and the request that
// reaches the server is compared with what the predicate said.

func assistantShapes() map[string][]llm.ContentBlock {
	img := llm.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}}
	return map[string][]llm.ContentBlock{
		"no blocks":                    nil,
		"empty text":                   {llm.TextBlock{}},
		"whitespace text":              {llm.TextBlock{Text: " \n\t"}},
		"nbsp text":                    {llm.TextBlock{Text: " "}},
		"unsigned thinking":            {llm.ReasoningBlock{Text: "cut off"}},
		"unsigned empty thinking":      {llm.ReasoningBlock{}},
		"signed thinking":              {llm.ReasoningBlock{Text: "t", Signature: "sig"}},
		"signed empty thinking":        {llm.ReasoningBlock{Signature: "sig"}},
		"redacted thinking":            {llm.ReasoningBlock{Redacted: true, Signature: "blob"}},
		"redacted thinking, no data":   {llm.ReasoningBlock{Redacted: true}},
		"tool call, nil input":         {llm.ToolCallBlock{ID: "toolu_1", Name: "echo"}},
		"tool call, empty object":      {llm.ToolCallBlock{ID: "toolu_1", Name: "echo", Arguments: json.RawMessage(`{}`)}},
		"image only":                   {img},
		"image with hollow text":       {llm.TextBlock{Text: " "}, img},
		"hollow text then signed":      {llm.TextBlock{Text: " "}, llm.ReasoningBlock{Text: "t", Signature: "s"}},
		"unsigned then whitespace":     {llm.ReasoningBlock{Text: "t"}, llm.TextBlock{Text: "  "}},
		"unsigned thinking, tool call": {llm.ReasoningBlock{Text: "t"}, llm.ToolCallBlock{ID: "toolu_2", Name: "echo"}},
	}
}

// For every shape: what the server receives is what the predicate says survives.
// A message the predicate calls sendable is on the wire with a non-empty content
// list, and one it calls hollow is absent. Disagreement either way is a defect:
// a sendable message that is not sent hides content from the model, and a hollow
// one that is sent is a 400.
func TestAdvFixBPredicateEqualsWhatTheServerReceives(t *testing.T) {
	for name, blocks := range assistantShapes() {
		t.Run(name, func(t *testing.T) {
			msg := llm.Message{Role: llm.RoleAssistant, Content: blocks}
			adapter, server := adapterFor(t, minimalStream)
			stream, err := adapter.Stream(adaptertest.Ctx(), llm.Request{
				Model: "claude-opus-5-5", MaxTokens: 100,
				Messages: []llm.Message{userMsg("hi"), msg, userMsg("next")},
			})
			if err != nil {
				t.Fatalf("Stream refused user, assistant(%s), user: %v", name, err)
			}
			_ = stream.Close()
			reqs := server.Requests()
			if len(reqs) != 1 {
				t.Fatalf("%d requests", len(reqs))
			}
			var body struct {
				Messages []struct {
					Role    string            `json:"role"`
					Content []json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal([]byte(reqs[0]), &body); err != nil {
				t.Fatal(err)
			}
			assistants := 0
			for i, m := range body.Messages {
				if len(m.Content) == 0 {
					t.Errorf("message %d (%s) went out with no content", i, m.Role)
				}
				if m.Role == "assistant" {
					assistants++
				}
			}
			want := 0
			if msg.HasSendableContent() {
				want = 1
			}
			if assistants != want {
				t.Errorf("predicate says sendable=%v, the server received %d assistant messages: %s", msg.HasSendableContent(), assistants, reqs[0])
			}
		})
	}
}

// ValidateRequest judges the sequence that is sent. For every shape between a
// notice and the next user message, the verdict must be the one the wire form
// deserves: a sendable answer is accepted, a hollow one is refused (user, system,
// user is a 400, shared/prompt-caching.md, "Mid-conversation system messages").
func TestAdvFixBPlacementVerdictFollowsThePredicateForEveryShape(t *testing.T) {
	for name, blocks := range assistantShapes() {
		t.Run(name, func(t *testing.T) {
			msg := llm.Message{Role: llm.RoleAssistant, Content: blocks}
			err := ValidateRequest(llm.Request{
				Model: "claude-opus-5-5", MaxTokens: 100,
				Messages: []llm.Message{userMsg("hi"), sysMsg("notice"), msg, userMsg("next")},
			}, ThinkingAdaptive)
			if msg.HasSendableContent() && err != nil {
				t.Errorf("a notice answered by a sendable turn was refused: %v", err)
			}
			if !msg.HasSendableContent() && err == nil {
				t.Error("user, system, user was accepted")
			}
		})
	}
}

// Two notices separated only by a hollow reply are adjacent on the wire; a
// notice, a hollow reply and the end of the request is a notice that is last,
// which is fine. Both are judged on the sent form.
func TestAdvFixBAdjacentNoticesThroughAHollowReplyAreRefused(t *testing.T) {
	hollow := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "cut"}}}
	good := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}}}
	cases := map[string]struct {
		msgs   []llm.Message
		accept bool
	}{
		"notice, hollow, notice, good, user": {[]llm.Message{userMsg("a"), sysMsg("n1"), hollow, sysMsg("n2"), good, userMsg("b")}, false},
		"notice, hollow, end":                {[]llm.Message{userMsg("a"), sysMsg("n1"), hollow}, true},
		"user, hollow first, notice":         {[]llm.Message{userMsg("a"), hollow, sysMsg("n1")}, true},
		"notice, good, notice (no user)":     {[]llm.Message{userMsg("a"), sysMsg("n1"), good, sysMsg("n2")}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateRequest(llm.Request{Model: "claude-opus-5-5", MaxTokens: 100, Messages: c.msgs}, ThinkingAdaptive)
			if c.accept && err != nil {
				t.Errorf("refused a valid sent form: %v", err)
			}
			if !c.accept && err == nil {
				t.Error("accepted an invalid sent form")
			}
		})
	}
}
