package session

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// A derived history is the caller's to change. Replacing a block, rewriting a
// tool call's argument bytes or a tool result's content, or editing an image in
// place must not show in the next derivation — on the warm path, where the
// copy is all that stands between a reader and the projection.
func TestDerivedHistoryIsADeepCopy(t *testing.T) {
	l := NewLog()
	benchAppend(t, l, UserMessage, MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		llm.TextBlock{Text: "hello"},
		llm.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}},
	}}})
	benchAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ToolCallBlock{ID: "c1", Name: "grep", Arguments: json.RawMessage(`{"pattern":"x"}`)},
	}}})
	benchAppend(t, l, ToolResult, ToolResultData{ToolCallID: "c1", Content: llm.Content{llm.TextBlock{Text: "result"}}})

	first, err := l.DeriveMessages() // cold: builds the projection
	if err != nil {
		t.Fatal(err)
	}
	want, err := l.DeriveMessages() // warm
	if err != nil {
		t.Fatal(err)
	}

	for pass := 0; pass < 2; pass++ {
		msgs := first
		if pass == 1 {
			msgs, _ = l.DeriveMessages()
		}
		msgs[0].Content[0] = llm.TextBlock{Text: "changed"}
		msgs[0].Content[1].(llm.ImageBlock).Data[0] = 99
		msgs[1].Content[0].(llm.ToolCallBlock).Arguments[2] = 'Z'
		tr := msgs[2].Content[0].(llm.ToolResultBlock)
		tr.Content[0] = llm.TextBlock{Text: "changed"}
		msgs[2].Content[0] = llm.ToolResultBlock{ToolCallID: "other", Content: tr.Content}
		msgs[1] = llm.Message{Role: llm.RoleUser}
	}

	got, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a caller's edits reached the next derivation:\n got %#v\nwant %#v", got, want)
	}
}
