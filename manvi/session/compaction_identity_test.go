package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func callMessage(id string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ToolCallBlock{ID: llm.CallID(id), Name: "read", Arguments: json.RawMessage(`{}`)},
	}}
}

func appendEvent(t *testing.T, l *Log, typ Type, payload any) Event {
	t.Helper()
	ev, err := l.Append(typ, payload)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// compactResult records a compaction the way the planner does: naming the
// result by its sequence. The payload is spelled as the journal spells it, so a
// rename of the persisted field fails here.
func compactResult(t *testing.T, l *Log, result Event, callID, text string) Event {
	t.Helper()
	return appendEvent(t, l, ToolResultCompacted, map[string]any{
		"tool_call_id": callID, "result_seq": result.Seq, "text": text,
	})
}

// toolResultTexts is the text of every tool result in msgs, in history order.
func toolResultTexts(t *testing.T, msgs []llm.Message) []string {
	t.Helper()
	texts := []string{}
	for _, m := range msgs {
		for _, b := range m.Content {
			tr, ok := b.(llm.ToolResultBlock)
			if !ok {
				continue
			}
			if len(tr.Content) == 0 {
				t.Fatalf("tool result %s has no content", tr.ToolCallID)
			}
			text, ok := tr.Content[0].(llm.TextBlock)
			if !ok {
				t.Fatalf("tool result %s starts with %T, want text", tr.ToolCallID, tr.Content[0])
			}
			texts = append(texts, text.Text)
		}
	}
	return texts
}

// A provider that reuses a call id across responses ("call_0" is what several
// OpenAI-compatible local servers emit) must not have the compaction recorded
// for turn one's result replayed over turn two's unrelated one.
func TestCompactionOfOneResultDoesNotLeakAcrossReusedCallID(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, TurnStart, nil)
	mustAppend(t, l, UserMessage, MessageData{Message: user("one")})
	mustAppend(t, l, AssistantMessage, MessageData{Message: callMessage("call_0")})
	first := appendEvent(t, l, ToolResult, ToolResultData{ToolCallID: "call_0", Text: strings.Repeat("A", 5000)})
	compactResult(t, l, first, "call_0", "SHORT-OF-TURN-1")
	mustAppend(t, l, AssistantMessage, MessageData{Message: assistant("done")})
	mustAppend(t, l, TurnEnd, nil)

	mustAppend(t, l, TurnStart, nil)
	mustAppend(t, l, UserMessage, MessageData{Message: user("two")})
	mustAppend(t, l, AssistantMessage, MessageData{Message: callMessage("call_0")})
	mustAppend(t, l, ToolResult, ToolResultData{ToolCallID: "call_0", Text: "FRESH-RESULT-OF-TURN-2"})

	msgs, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	texts := toolResultTexts(t, msgs)
	if len(texts) != 2 {
		t.Fatalf("history holds %d tool results, want 2", len(texts))
	}
	if texts[1] != "FRESH-RESULT-OF-TURN-2" {
		t.Fatalf("turn 2's tool result was replaced by turn 1's compaction: model sees %q", texts[1])
	}
	if texts[0] != "SHORT-OF-TURN-1" {
		t.Fatalf("turn 1's result lost its own compaction: model sees %q", texts[0])
	}
}

// The same holds when the compaction lands after both results exist, and for a
// log rebuilt from its events rather than extended incrementally.
func TestCompactionByResultSeqSurvivesRebuild(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, UserMessage, MessageData{Message: user("go")})
	mustAppend(t, l, AssistantMessage, MessageData{Message: callMessage("call_0")})
	first := appendEvent(t, l, ToolResult, ToolResultData{ToolCallID: "call_0", Text: "first"})
	mustAppend(t, l, AssistantMessage, MessageData{Message: callMessage("call_0")})
	second := appendEvent(t, l, ToolResult, ToolResultData{ToolCallID: "call_0", Text: "second"})
	compactResult(t, l, second, "call_0", "SHORT-SECOND")

	restored, err := RestoreLog(l.Events())
	if err != nil {
		t.Fatal(err)
	}
	for name, log := range map[string]*Log{"live": l, "restored": restored} {
		msgs, err := log.DeriveMessages()
		if err != nil {
			t.Fatal(err)
		}
		texts := toolResultTexts(t, msgs)
		if len(texts) != 2 {
			t.Fatalf("%s: history holds %d tool results, want 2", name, len(texts))
		}
		if texts[0] != "first" {
			t.Errorf("%s: first result is %q, want it untouched", name, texts[0])
		}
		if texts[1] != "SHORT-SECOND" {
			t.Errorf("%s: second result is %q, want its compaction", name, texts[1])
		}
	}
	_ = first
}

// A journal written before compactions named a result carries only the call id.
// It must load, and project exactly as the code that wrote it did: the
// compaction applies to every result with that id. The events are spelled out
// as that code wrote them so a change to the field set cannot quietly rewrite
// what an old session replays.
func TestJournalWithoutResultSeqProjectsAsBefore(t *testing.T) {
	journal := []Event{
		{Seq: 1, Type: UserMessage, Turn: 1, Data: json.RawMessage(`{"message":{"role":"user","content":[{"kind":"text","text":"go"}]}}`)},
		{Seq: 2, Type: AssistantMessage, Turn: 1, Data: json.RawMessage(`{"message":{"role":"assistant","content":[{"kind":"tool_call","id":"call_0","name":"read","arguments":{}}]}}`)},
		{Seq: 3, Type: ToolResult, Turn: 1, Data: json.RawMessage(`{"tool_call_id":"call_0","text":"first result, long"}`)},
		{Seq: 4, Type: ToolResultCompacted, Turn: 1, Data: json.RawMessage(`{"tool_call_id":"call_0","text":"SHORT","from_bytes":18,"to_bytes":5}`)},
		{Seq: 5, Type: AssistantMessage, Turn: 1, Data: json.RawMessage(`{"message":{"role":"assistant","content":[{"kind":"tool_call","id":"call_0","name":"read","arguments":{}}]}}`)},
		{Seq: 6, Type: ToolResult, Turn: 1, Data: json.RawMessage(`{"tool_call_id":"call_0","text":"second result, long"}`)},
		{Seq: 7, Type: AssistantMessage, Turn: 1, Data: json.RawMessage(`{"message":{"role":"assistant","content":[{"kind":"text","text":"done"}]}}`)},
	}
	l, err := RestoreLog(journal)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 6 {
		t.Fatalf("projected %d messages, want 6", len(msgs))
	}
	texts := toolResultTexts(t, msgs)
	if len(texts) != 2 {
		t.Fatalf("history holds %d tool results, want 2", len(texts))
	}
	for i, got := range texts {
		if got != "SHORT" {
			t.Errorf("result %d is %q: an id-only compaction applies to every result with that id", i, got)
		}
	}
	calls, err := l.CompactedCalls()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := calls["call_0"]; !ok || len(calls) != 1 {
		t.Fatalf("CompactedCalls = %v, want exactly call_0", calls)
	}
}
