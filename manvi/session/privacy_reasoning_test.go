package session

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

const reasoningSecret = "hunter2"

func reasoningScrub(s string) string { return strings.ReplaceAll(s, reasoningSecret, "[REDACTED]") }

func reasoningCount(msgs []llm.Message) (n int) {
	for _, m := range msgs {
		for _, b := range m.Content {
			if _, ok := b.(llm.ReasoningBlock); ok {
				n++
			}
		}
	}
	return n
}

// redactedMark reads the marker off the persisted payload, not through a typed
// field, so the assertion is about what reaches the journal.
func redactedMark(t *testing.T, ev Event) float64 {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(ev.Data, &payload); err != nil {
		t.Fatal(err)
	}
	n, _ := payload["reasoning_redacted"].(float64)
	return n
}

// A signed thinking block must reach the provider exactly as it was produced or
// not at all: the API rejects a block whose text no longer matches its
// signature ("Modified thinking blocks are rejected with a 400 error"). When
// the scrubber would change a signed block's text, the block goes whole, the
// secret is not persisted, and the event says a block was removed.
func TestScrubbedSignedReasoningIsDroppedWhole(t *testing.T) {
	l := NewLog()
	l.SetScrubber(reasoningScrub)
	mustAppend(t, l, UserMessage, MessageData{Message: user("hi")})
	mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningBlock{Text: "the password is hunter2", Signature: "SIG-OVER-ORIGINAL-TEXT"},
		llm.TextBlock{Text: "ok"},
	}}})

	msgs, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if n := reasoningCount(msgs); n != 0 {
		t.Fatalf("%d reasoning block(s) replayed after the scrubber changed a signed one: %#v", n, msgs[1].Content)
	}
	if got := msgs[1].Text(); got != "ok" {
		t.Fatalf("the visible text was not kept: %q", got)
	}
	events := l.Events()
	last := events[len(events)-1]
	if strings.Contains(string(last.Data), reasoningSecret) || strings.Contains(string(last.Data), "SIG-OVER-ORIGINAL-TEXT") {
		t.Fatalf("persisted payload still carries the secret or the orphaned signature: %s", last.Data)
	}
	if redactedMark(t, last) != 1 {
		t.Fatalf("the event does not say a signed reasoning block was removed: %s", last.Data)
	}
}

// Within one message the API checks the run of thinking blocks as a unit: "you
// can't rearrange, edit, or partially drop them". Dropping only the block the
// scrubber changed would leave a gap in the run, so the whole run goes.
func TestScrubbedSignedReasoningDropsTheWholeRun(t *testing.T) {
	l := NewLog()
	l.SetScrubber(reasoningScrub)
	mustAppend(t, l, UserMessage, MessageData{Message: user("go")})
	mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningBlock{Text: "clean thought", Signature: "SIG-A"},
		llm.ReasoningBlock{Text: "the password is hunter2", Signature: "SIG-B"},
		llm.ReasoningBlock{Signature: "ENCRYPTED-C", Redacted: true},
		llm.ToolCallBlock{ID: "c1", Name: "read", Arguments: json.RawMessage(`{}`)},
	}}})
	msgs, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if n := reasoningCount(msgs); n != 0 {
		t.Fatalf("a partial run of thinking blocks survived: %#v", msgs[1].Content)
	}
	if len(msgs[1].ToolCalls()) != 1 {
		t.Fatalf("the tool call must stay: %#v", msgs[1].Content)
	}
	events := l.Events()
	if got := redactedMark(t, events[len(events)-1]); got != 3 {
		t.Fatalf("marker counts %v removed blocks, want 3", got)
	}
}

// Everything the scrubber leaves alone is left alone: a signed block whose text
// it does not change (including the empty text of an omitted display) replays
// byte for byte, and unsigned reasoning from a provider with no signatures is
// still scrubbed in place.
func TestScrubKeepsSignedReasoningItDoesNotChange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block llm.ReasoningBlock
		want  llm.ReasoningBlock
	}{
		{"unchanged text", llm.ReasoningBlock{Text: "nothing secret", Signature: "SIG"}, llm.ReasoningBlock{Text: "nothing secret", Signature: "SIG"}},
		{"omitted display", llm.ReasoningBlock{Signature: "SIG-OMITTED"}, llm.ReasoningBlock{Signature: "SIG-OMITTED"}},
		{"encrypted", llm.ReasoningBlock{Signature: "DATA", Redacted: true}, llm.ReasoningBlock{Signature: "DATA", Redacted: true}},
		{"unsigned", llm.ReasoningBlock{Text: "the password is hunter2"}, llm.ReasoningBlock{Text: "the password is [REDACTED]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLog()
			l.SetScrubber(reasoningScrub)
			mustAppend(t, l, UserMessage, MessageData{Message: user("go")})
			mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				tc.block, llm.TextBlock{Text: "ok"},
			}}})
			msgs, err := l.DeriveMessages()
			if err != nil {
				t.Fatal(err)
			}
			got, ok := msgs[1].Content[0].(llm.ReasoningBlock)
			if !ok || got != tc.want {
				t.Fatalf("replayed %#v, want %#v", msgs[1].Content[0], tc.want)
			}
			events := l.Events()
			if mark := redactedMark(t, events[len(events)-1]); mark != 0 {
				t.Fatalf("event marked redacted (%v) though no block was removed", mark)
			}
		})
	}
}

// A journal that carries a signed block with a secret in it, restored through
// the sensitive path, gets the same treatment before its first projection.
func TestRestoreSensitiveLogDropsScrubbedSignedReasoning(t *testing.T) {
	payload, err := json.Marshal(MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningBlock{Text: "the password is hunter2", Signature: "SIG-OVER-ORIGINAL-TEXT"},
		llm.TextBlock{Text: "ok"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	userPayload, err := json.Marshal(MessageData{Message: user("hi")})
	if err != nil {
		t.Fatal(err)
	}
	l, err := RestoreSensitiveLog([]Event{
		{Seq: 1, Type: UserMessage, Turn: 1, Data: userPayload},
		{Seq: 2, Type: AssistantMessage, Turn: 1, Data: payload},
	}, reasoningScrub)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if n := reasoningCount(msgs); n != 0 {
		t.Fatalf("restored history replays %d reasoning block(s): %#v", n, msgs[1].Content)
	}
	events := l.Events()
	if strings.Contains(string(events[1].Data), reasoningSecret) || redactedMark(t, events[1]) != 1 {
		t.Fatalf("restored event not scrubbed and marked: %s", events[1].Data)
	}
}

func signedTurn(think, text, sig string) MessageData {
	return MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningBlock{Text: think, Signature: sig},
		llm.TextBlock{Text: text},
	}}}
}

func journalOf(t *testing.T, payloads ...any) []Event {
	t.Helper()
	events := make([]Event, 0, len(payloads))
	for i, p := range payloads {
		var typ Type
		switch v := p.(type) {
		case MessageData:
			typ = AssistantMessage
			if v.Message.Role == llm.RoleUser {
				typ = UserMessage
			}
		case ToolResultData:
			typ = ToolResult
		default:
			t.Fatalf("unsupported payload %T", p)
		}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, Event{Seq: i + 1, Type: typ, Turn: 1, Data: data})
	}
	return events
}

func signedAt(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		sig := ""
		for _, b := range m.Content {
			if r, ok := b.(llm.ReasoningBlock); ok {
				sig = r.Signature
			}
		}
		out = append(out, sig)
	}
	return out
}

// A live append scrubs before the next request is built, so the model mints
// turn two's thinking against the scrubbed history and its signature stays
// valid. Turn one's signed run goes; turn two keeps its own, byte for byte.
func TestAppendTimeScrubKeepsALaterTurnsSignature(t *testing.T) {
	l := NewLog()
	l.SetScrubber(reasoningScrub)
	mustAppend(t, l, UserMessage, MessageData{Message: user("one")})
	mustAppend(t, l, AssistantMessage, signedTurn("the password is hunter2", "a", "SIG-1"))
	mustAppend(t, l, UserMessage, MessageData{Message: user("two")})
	mustAppend(t, l, AssistantMessage, signedTurn("a clean thought", "b", "SIG-2"))
	msgs, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if got := signedAt(msgs); len(got) != 2 || got[0] != "" || got[1] != "SIG-2" {
		t.Fatalf("signatures per assistant turn = %q, want turn 1 dropped and turn 2 kept", got)
	}
}

// A restore rewrites a history the provider already signed, so the signed
// reasoning after the first rewrite is stale whatever kind of string changed:
// a thinking block (its own message too), a text block, a user message, a tool
// result. Each message that lost its run records the count.
func TestRestoreScrubDropsSignedReasoningAfterAnEdit(t *testing.T) {
	secretText := signedTurn("a clean thought", "token hunter2 here", "SIG-1")
	secretThought := signedTurn("the password is hunter2", "a", "SIG-1")
	secretUser := MessageData{Message: user("my password is hunter2")}
	secretResult := ToolResultData{ToolCallID: "c1", Text: "found hunter2"}
	next := signedTurn("a second thought", "b", "SIG-2")
	third := signedTurn("a third thought", "c", "SIG-3")
	call := MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningBlock{Text: "look", Signature: "SIG-0"},
		llm.ToolCallBlock{ID: "c1", Name: "read", Arguments: json.RawMessage(`{}`)}}}}

	for _, tc := range []struct {
		name     string
		journal  []any
		wantSigs []string
		marks    []float64 // reasoning_redacted per assistant event, in order
	}{
		{"a text block counts as an edit", []any{MessageData{Message: user("one")}, secretText, MessageData{Message: user("two")}, next, third},
			[]string{"SIG-1", "", ""}, []float64{0, 1, 1}},
		{"the message's own thinking changed", []any{MessageData{Message: user("one")}, secretThought, MessageData{Message: user("two")}, next},
			[]string{"", ""}, []float64{1, 1}},
		{"a user message", []any{secretUser, signedTurn("early", "a", "SIG-0"), next},
			[]string{"", ""}, []float64{1, 1}},
		{"a tool result", []any{MessageData{Message: user("one")}, call, secretResult, next},
			[]string{"SIG-0", ""}, []float64{0, 1}},
		{"nothing rewritten", []any{MessageData{Message: user("one")}, signedTurn("clean", "a", "SIG-1"), MessageData{Message: user("two")}, next},
			[]string{"SIG-1", "SIG-2"}, []float64{0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := RestoreSensitiveLog(journalOf(t, tc.journal...), reasoningScrub)
			if err != nil {
				t.Fatal(err)
			}
			msgs, err := l.DeriveMessages()
			if err != nil {
				t.Fatal(err)
			}
			got := signedAt(msgs)
			if !reflect.DeepEqual(got, tc.wantSigs) {
				t.Fatalf("signed reasoning per assistant turn = %q, want %q", got, tc.wantSigs)
			}
			var marks []float64
			for _, ev := range l.Events() {
				if ev.Type == AssistantMessage {
					marks = append(marks, redactedMark(t, ev))
				}
			}
			if !reflect.DeepEqual(marks, tc.marks) {
				t.Fatalf("reasoning_redacted per assistant event = %v, want %v", marks, tc.marks)
			}
			for _, ev := range l.Events() {
				if strings.Contains(string(ev.Data), reasoningSecret) {
					t.Fatalf("secret survived the restore: %s", ev.Data)
				}
			}
		})
	}
}

// Restoring what a restore already produced changes nothing, and does not
// recompute the count of what was dropped.
func TestRestoreSensitiveLogIsAFixedPoint(t *testing.T) {
	first, err := RestoreSensitiveLog(journalOf(t,
		MessageData{Message: user("one")},
		MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "pw is hunter2", Signature: "SIG-1"},
			llm.ReasoningBlock{Signature: "ENC", Redacted: true},
			llm.TextBlock{Text: "a <b> & c"}}}},
		signedTurn("later", "b", "SIG-2")), reasoningScrub)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RestoreSensitiveLog(first.Events(), reasoningScrub)
	if err != nil {
		t.Fatal(err)
	}
	a, b := first.Events(), second.Events()
	for i := range a {
		if string(a[i].Data) != string(b[i].Data) {
			t.Errorf("event %d is not a fixed point:\n%s\n%s", i, a[i].Data, b[i].Data)
		}
	}
	if n := redactedMark(t, b[1]); n != 2 {
		t.Errorf("reasoning_redacted = %v after a second restore, want 2", n)
	}
}

// A message whose only content was the dropped run is empty. The projection
// leaves it out, and the log still restores and derives.
func TestAMessageOfOnlyDroppedReasoningLeavesTheHistoryWellFormed(t *testing.T) {
	l := NewLog()
	l.SetScrubber(reasoningScrub)
	mustAppend(t, l, UserMessage, MessageData{Message: user("one")})
	mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "pw is hunter2", Signature: "SIG-1"}},
	}})
	mustAppend(t, l, UserMessage, MessageData{Message: user("two")})
	msgs, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range msgs {
		if len(m.Content) == 0 {
			t.Errorf("message %d is empty", i)
		}
	}
	if _, err := RestoreLog(l.Events()); err != nil {
		t.Fatal(err)
	}
}
