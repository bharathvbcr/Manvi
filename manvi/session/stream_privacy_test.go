package session

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func TestSensitiveStreamFragmentsCannotReconstructSecret(t *testing.T) {
	l := NewLog()
	l.SetSensitiveScrubber(func(s string) string { return strings.ReplaceAll(s, "private-member", "[redacted]") })
	var observed []Event
	l.Observe(func(e Event) { observed = append(observed, e) })
	for _, kind := range []llm.ChunkKind{llm.ChunkText, llm.ChunkReasoning, llm.ChunkToolCallDelta} {
		for _, fragment := range []string{"private-", "member"} {
			mustAppend(t, l, AssistantChunk, llm.Chunk{Kind: kind, Text: fragment, ArgumentsRaw: fragment, ToolCallID: llm.CallID(fragment)})
		}
	}
	mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: llm.Content{llm.TextBlock{Text: "private-member"}}}})
	for _, events := range [][]Event{l.Events(), observed} {
		for _, e := range events {
			if e.Type != AssistantChunk {
				continue
			}
			var data map[string]json.RawMessage
			if err := json.Unmarshal(e.Data, &data); err != nil {
				t.Fatal(err)
			}
			if string(data["withheld"]) != "true" || len(data) != 1 {
				t.Fatalf("stream fragment escaped privacy boundary: %s", e.Data)
			}
		}
	}
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
	if !reflect.DeepEqual(warm, cold) {
		t.Fatal("restored history differs")
	}
	encoded, err := json.Marshal(warm)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-member") || !strings.Contains(string(encoded), "[redacted]") {
		t.Fatal("complete message not sanitized")
	}
}

func TestOrdinaryLogRetainsStreaming(t *testing.T) {
	l := NewLog()
	e, err := l.Append(AssistantChunk, llm.Chunk{Kind: llm.ChunkText, Text: "visible"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(e.Data), "visible") {
		t.Fatal("ordinary streaming changed")
	}
}

func TestSensitiveRestoreSanitizesBeforeFirstProjection(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, AssistantChunk, llm.Chunk{Kind: llm.ChunkText, Text: "legacy-secret"})
	mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: llm.Content{llm.TextBlock{Text: "legacy-secret"}}}})
	imported := l.Events()
	restored, err := RestoreSensitiveLog(imported, func(s string) string { return strings.ReplaceAll(s, "legacy-secret", "[redacted]") })
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range restored.Events() {
		if strings.Contains(string(e.Data), "legacy-secret") {
			t.Fatal("restored secret escaped")
		}
	}
	if !strings.Contains(string(imported[1].Data), "legacy-secret") {
		t.Fatal("caller-owned import mutated")
	}
	warm, err := restored.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	cold, err := RestoreLog(restored.Events())
	if err != nil {
		t.Fatal(err)
	}
	messages, err := cold.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(warm, messages) {
		t.Fatal("warm/restored privacy differs")
	}
}
