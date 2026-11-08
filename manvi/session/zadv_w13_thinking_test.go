package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// Adversarial review of w13-thinking, item 9 and the retention side of item 2.

const advMarker = "sk-ADV-MARKER-9f2c"

func advScrub(s string) string { return strings.ReplaceAll(s, advMarker, "[redacted]") }

// advFrameFixture appends one conversation that carries a secret in every field
// the wire frame and a notice add to the log: the frame's system prompt and
// tool description, and the notice's text and prompt.
func advFrameFixture(t *testing.T, l *Log) {
	t.Helper()
	mustAppend(t, l, TurnStart, nil)
	mustAppend(t, l, WireFrame, FrameData{
		System: "instructions " + advMarker,
		Tools: []llm.ToolSchema{{
			Name: "echo", Description: "echoes " + advMarker, InputSchema: json.RawMessage(`{"type":"object","description":"` + advMarker + `"}`),
		}},
		Why: WhyOpened,
	})
	mustAppend(t, l, UserMessage, MessageData{Message: user("hello")})
	mustAppend(t, l, AssistantMessage, MessageData{Message: assistant("hi")})
	mustAppend(t, l, TurnEnd, nil)
	mustAppend(t, l, TurnStart, nil)
	mustAppend(t, l, UserMessage, MessageData{Message: user("again")})
	mustAppend(t, l, WireNotice, NoticeData{
		Text:   "The instructions have been updated. " + advMarker,
		Prompt: "instructions v2 " + advMarker, Offered: []string{"echo"},
	})
	mustAppend(t, l, AssistantMessage, MessageData{Message: assistant("ok")})
	mustAppend(t, l, TurnEnd, nil)
}

// A credential in the wire frame or a notice reaches neither the record that
// goes to disk nor the next request, the same as system/prompt: the generic
// scrubber walks every payload, and the new payloads are in schemaKeys so their
// field names survive it.
func TestAdvScrubberCoversFrameAndNoticePayloads(t *testing.T) {
	l := NewLog()
	l.SetScrubber(advScrub)
	advFrameFixture(t, l)

	encoded, err := json.Marshal(l.Events())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), advMarker) {
		t.Errorf("a credential survived in the stored record:\n%s", encoded)
	}
	msgs, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	projected, _ := json.Marshal(msgs)
	if strings.Contains(string(projected), advMarker) {
		t.Errorf("a credential survived in the projected notice: %s", projected)
	}
	for _, e := range l.Events() {
		var probe map[string]any
		if len(e.Data) > 0 {
			if err := json.Unmarshal(e.Data, &probe); err != nil {
				t.Fatalf("scrubbing corrupted a %s payload: %v", e.Type, err)
			}
		}
	}
	state := l.Wire()
	if state.Frame == nil || state.Frame.System != "instructions [redacted]" || state.Delivered != "instructions v2 [redacted]" {
		t.Errorf("wire state after scrubbing = %+v", state)
	}
}

// The same events written by a build with no scrubber and restored under one
// (RestoreSensitiveLog) come out scrubbed and fold into the same wire state.
func TestAdvRestoreSensitiveLogScrubsFrameAndNotice(t *testing.T) {
	plain := NewLog()
	advFrameFixture(t, plain)
	restored, err := RestoreSensitiveLog(plain.Events(), advScrub)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(restored.Events())
	if strings.Contains(string(encoded), advMarker) {
		t.Errorf("a credential survived a sensitive restore:\n%s", encoded)
	}
	if got := restored.Wire().Delivered; got != "instructions v2 [redacted]" {
		t.Errorf("Delivered = %q after a sensitive restore", got)
	}
}

// Retention. Store.Save trims a session to its byte budget by dropping whole
// OLD turns (trimEncoded). The wire frame is appended inside the first turn, so
// a trimmed file loses the frame that the surviving, still-signed blocks were
// produced under, and a resumed conversation re-opens a frame from the latest
// prompt and the current tools instead. The frame is meant to be what the log
// remembers across a resume; a trim must not forget it. (The trim also removes
// the oldest messages, which is itself an edit of the prefix of what survives,
// so the practical cost is bounded; the frame still should not silently reset.)
func TestAdvFrameSurvivesTheRetentionTrim(t *testing.T) {
	store := NewStore(t.TempDir())
	store.setLimits(6000, 0)
	id := newID(t)

	l := NewLog()
	for i, prompt := range []string{"one", "two", "three", "four", "five"} {
		mustAppend(t, l, TurnStart, nil)
		if i == 0 {
			mustAppend(t, l, WireFrame, FrameData{System: "the first prompt", Why: WhyOpened,
				Tools: []llm.ToolSchema{{Name: "echo", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}})
		}
		mustAppend(t, l, SystemPrompt, SystemPromptData{Text: "later prompt"})
		mustAppend(t, l, UserMessage, MessageData{Message: user(prompt + strings.Repeat("y", 1500))})
		mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "t", Signature: "sig-" + prompt}, llm.TextBlock{Text: "done"}}}})
		mustAppend(t, l, TurnEnd, nil)
	}
	report, err := store.Save(id, l)
	if err != nil {
		t.Fatal(err)
	}
	if report.TrimmedTurns == 0 {
		t.Fatal("the fixture was not trimmed, so the test says nothing")
	}
	restored, _, err := NewStore(store.Dir()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	frame := restored.Wire().Frame
	if frame == nil || frame.System != "the first prompt" {
		t.Errorf("after a trim that dropped %d turns the resumed log's frame = %+v, want the first prompt's frame; "+
			"the signed blocks that survive were produced under it", report.TrimmedTurns, frame)
	}
}
