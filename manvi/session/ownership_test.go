package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func TestWarmProjectionUsesSanitizedOwnedPayload(t *testing.T) {
	l := scrubbingLog(t)
	mustAppend(t, l, UserMessage, MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "start"}}}})
	if _, err := l.DeriveMessages(); err != nil {
		t.Fatal(err)
	}
	m := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: fakeKey}}}
	mustAppend(t, l, AssistantMessage, MessageData{Message: m})
	m.Content[0] = llm.TextBlock{Text: "caller changed the message"}
	mustAppend(t, l, ToolResult, ToolResultData{ToolCallID: "c1", Text: fakeKey})
	warm, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreLog(l.Events())
	if err != nil {
		t.Fatal(err)
	}
	cold, err := restored.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	w, err := json.Marshal(warm)
	if err != nil {
		t.Fatal(err)
	}
	c, err := json.Marshal(cold)
	if err != nil {
		t.Fatal(err)
	}
	if string(w) != string(c) || strings.Contains(string(w), fakeKey) {
		t.Fatalf("warm history differs from canonical sanitized history: warm=%s cold=%s", w, c)
	}
}

func TestEventAndProjectionReadersCannotMutateJournal(t *testing.T) {
	l := NewLog()
	l.Observe(func(e Event) {
		if len(e.Data) > 0 {
			e.Data[0] = '!'
		}
	})
	e, err := l.Append(UserMessage, MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(l.Events()[0].Data) {
		t.Fatal("observer mutated journal")
	}
	e.Data[0] = '!'
	events := l.Events()
	events[0].Data[0] = '!'
	if !json.Valid(l.Events()[0].Data) {
		t.Fatal("returned event mutated journal")
	}
	messages, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	img, ok := messages[0].Content[0].(llm.ImageBlock)
	if !ok {
		t.Fatal("missing image")
	}
	img.Data[0] = 9
	unchanged, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	original, ok := unchanged[0].Content[0].(llm.ImageBlock)
	if !ok || original.Data[0] != 1 {
		t.Fatal("projection reader mutated retained image")
	}
	snapshot := l.Events()
	restored, err := RestoreLog(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot[0].Data[0] = '!'
	if !json.Valid(restored.Events()[0].Data) {
		t.Fatal("restore retained caller-owned bytes")
	}
}
