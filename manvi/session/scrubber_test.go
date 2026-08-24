package session

import (
	"encoding/json"
	"strings"
	"testing"

	"manvi/llm"
)

// The log is the durable boundary and the projection boundary at once: what is
// appended here is what Store.Save writes to disk, and what DeriveMessages
// turns into the next request's history.
//
// That second half is why the backstop belongs on the log rather than on the
// writer. A credential removed at save time would still be in memory, still be
// projected, and still be sent to the provider on the following turn — the
// harness would have redacted the record of the leak and kept the leak.

const fakeKey = "sk-ant-api03-NOTAREALKEYAAAAAAAAAAAAAAAAAAAA"

func scrubbingLog(t *testing.T) *Log {
	t.Helper()
	l := NewLog()
	l.SetScrubber(func(s string) string {
		return strings.ReplaceAll(s, fakeKey, "[redacted]")
	})
	return l
}

func TestAnAppendedCredentialReachesNeitherDiskNorTheNextRequest(t *testing.T) {
	l := scrubbingLog(t)

	// Every payload shape that can carry model or subprocess text.
	if _, err := l.Append(TurnStart, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(UserMessage, MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "here is my key " + fakeKey}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(SystemPrompt, SystemPromptData{Text: "context includes " + fakeKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "I see " + fakeKey}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(ToolResult, ToolResultData{
		ToolCallID: "1", Text: "ANTHROPIC_API_KEY=" + fakeKey,
	}); err != nil {
		t.Fatal(err)
	}

	// The record that goes to disk.
	encoded, err := json.Marshal(l.Events())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), fakeKey) {
		t.Errorf("a credential survived into the events Store.Save writes:\n%s", encoded)
	}

	// Every event must still be valid JSON after the substitution — replacing
	// bytes inside an encoded document is only safe because neither the
	// credential nor the marker contains a character JSON escapes, and that is
	// worth asserting rather than reasoning about once.
	for _, e := range l.Events() {
		if len(e.Data) == 0 {
			continue
		}
		var any map[string]any
		if err := json.Unmarshal(e.Data, &any); err != nil {
			t.Fatalf("scrubbing corrupted the payload of a %s event: %v\ndata: %s", e.Type, err, e.Data)
		}
	}

	// And the history the next request is built from.
	messages, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	projected, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(projected), fakeKey) {
		t.Errorf("a credential survived into the message history sent to the provider:\n%s", projected)
	}
}

// A log with no scrubber installed is unchanged — that is what tests and any
// consumer without a composition root get.
func TestAnUnarmedLogIsUnchanged(t *testing.T) {
	l := NewLog()
	if _, err := l.Append(AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "plain text"}}}}); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(l.Events())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "plain text") {
		t.Fatalf("an unarmed log altered its payload: %s", encoded)
	}
}
