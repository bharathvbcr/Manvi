package session

import (
	"encoding/json"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"reflect"
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
	_, err := l.Append(AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}}, llm.ReasoningBlock{Text: "plain thought", Signature: "AQID"}}}})
	if err != nil {
		t.Fatalf("text scrubber corrupted binary payload: %v", err)
	}
	messages, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	// The signature is opaque: a scrubber target inside it is not rewritten. (A
	// signed block whose text the scrubber would change is dropped whole instead
	// of being rewritten beside its signature; see privacy_reasoning_test.go.)
	reasoning, ok := messages[0].Content[1].(llm.ReasoningBlock)
	if !ok || reasoning.Text != "plain thought" || reasoning.Signature != "AQID" {
		t.Fatal("opaque signature changed or an unchanged signed block was not replayed as recorded")
	}
	public, err := l.PublicEvents()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public[0].Data), "signature") {
		t.Fatal("private reasoning signature exported")
	}
}

const keySecret = "tok-SECRET-123"

func keyScrub(s string) string { return strings.ReplaceAll(s, keySecret, "[redacted]") }

// keyedCall is an assistant message whose tool arguments hold the secret only in
// object keys: at the top of the arguments, nested, and inside an array of
// objects. No value carries it, so only a walk that rewrites keys can remove it.
func keyedCall() MessageData {
	return MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ToolCallBlock{ID: "c1", Name: "http", Arguments: json.RawMessage(
			`{"` + keySecret + `":"v","nested":{"https://h.example/?t=` + keySecret + `":1},"list":[{"` + keySecret + `":true},{"ok":"fine"}]}`)},
	}}}
}

func TestScrubRewritesObjectKeysInStoredAndProjectedHistory(t *testing.T) {
	l := NewLog()
	l.SetScrubber(keyScrub)
	mustAppend(t, l, UserMessage, MessageData{Message: user("go")})
	mustAppend(t, l, AssistantMessage, keyedCall())
	// A top-level key, outside any message: the walk covers every event type.
	mustAppend(t, l, StepEnd, map[string]any{keySecret: 1, "usage": map[string]any{keySecret + "-a": 2}})

	stored, err := json.Marshal(l.Events())
	if err != nil {
		t.Fatal(err)
	}
	messages, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	projected, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	public, err := l.PublicEvents()
	if err != nil {
		t.Fatal(err)
	}
	exported, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string][]byte{"stored events": stored, "provider history": projected, "public events": exported} {
		if strings.Contains(string(got), keySecret) {
			t.Errorf("a secret in an object key survived into the %s:\n%s", name, got)
		}
	}
	if !strings.Contains(string(projected), `"ok":"fine"`) {
		t.Errorf("an unrelated key was lost from the provider history:\n%s", projected)
	}
}

// A rewrite that touched only keys is still a rewrite: a restore reads the flag
// to know the history the provider signed has changed.
func TestKeyOnlyRewriteIsReportedAndStalesLaterSignedReasoning(t *testing.T) {
	_, rewrote, err := scrubPayload(AssistantMessage, mustJSON(t, keyedCall()), keyScrub)
	if err != nil {
		t.Fatal(err)
	}
	if !rewrote {
		t.Fatal("a key-only rewrite was not reported")
	}

	later := signedTurn("a second thought", "b", "SIG-2")
	journal := journalOf(t, MessageData{Message: user("one")}, keyedCall(), MessageData{Message: user("two")}, later)
	restored, err := RestoreSensitiveLog(journal, keyScrub)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := restored.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if got := signedAt(msgs); len(got) != 2 || got[1] != "" {
		t.Fatalf("signed reasoning after a key-only edit = %q, want it dropped", got)
	}
	for _, ev := range restored.Events() {
		if strings.Contains(string(ev.Data), keySecret) {
			t.Fatalf("secret key survived the restore: %s", ev.Data)
		}
	}
}

// Keys that scrub to the same marker keep every entry, in an order set by the
// original key bytes rather than by map iteration, and a literal key that
// already reads as the marker keeps its own name.
func TestKeysThatScrubToOneMarkerAreAllKept(t *testing.T) {
	const doc = `{"[redacted]":"literal","b-` + keySecret + `":2,"a-` + keySecret + `":1,"c":3}`
	scrub := func(s string) string {
		if strings.Contains(s, keySecret) {
			return "[redacted]"
		}
		return s
	}
	var first json.RawMessage
	for i := 0; i < 50; i++ {
		out, rewrote, err := scrubPayload(StepEnd, json.RawMessage(doc), scrub)
		if err != nil {
			t.Fatal(err)
		}
		if !rewrote {
			t.Fatal("collapsing keys was not reported as a rewrite")
		}
		if first == nil {
			first = out
		} else if string(out) != string(first) {
			t.Fatalf("collision handling is not deterministic:\n%s\n%s", first, out)
		}
	}
	var literal map[string]any
	if err := json.Unmarshal(first, &literal); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"[redacted]": "literal", "[redacted]#2": float64(1), "[redacted]#3": float64(2), "c": float64(3)}
	if !reflect.DeepEqual(literal, want) {
		t.Fatalf("scrubbed object = %v, want %v", literal, want)
	}
	// Scrubbing what a scrub produced changes nothing: the suffixed names are
	// not secret-shaped, so a restore of a restore is a fixed point.
	again, rewrote, err := scrubPayload(StepEnd, first, scrub)
	if err != nil {
		t.Fatal(err)
	}
	if rewrote || string(again) != string(first) {
		t.Fatalf("a second scrub changed the object: %s (rewrote=%v)", again, rewrote)
	}
}

// One key's scrubbed name can be another key's original name; no entry may
// overwrite another while they move.
func TestRenamedKeysDoNotOverwriteEachOther(t *testing.T) {
	scrub := func(s string) string {
		switch s {
		case "x":
			return "y"
		case "y":
			return "z"
		}
		return s
	}
	out, rewrote, err := scrubPayload(StepEnd, json.RawMessage(`{"x":1,"y":2}`), scrub)
	if err != nil {
		t.Fatal(err)
	}
	if !rewrote || string(out) != `{"y":1,"z":2}` {
		t.Fatalf("scrubbed object = %s (rewrote=%v), want {\"y\":1,\"z\":2}", out, rewrote)
	}
}

// Keys are rewritten by the same scrubber that rewrites values, so a structural
// key survives only because it is not secret-shaped. This pins that for the
// payloads the agent emits: with a scrubber armed and no secret anywhere, every
// event reads back as it was appended.
func TestStructuralKeysSurviveAnArmedScrubber(t *testing.T) {
	appendAll := func(l *Log) {
		mustAppend(t, l, SystemPrompt, SystemPromptData{Text: "be brief"})
		mustAppend(t, l, UserMessage, MessageData{Message: user("go")})
		mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{
			Role:       llm.RoleAssistant,
			Provenance: &llm.AssistantProvenance{Provider: "gemini", Model: "m", ReplayState: json.RawMessage(`{"steps":[]}`)},
			Content: []llm.ContentBlock{
				llm.ReasoningBlock{Text: "think", Signature: "SIG"},
				llm.TextBlock{Text: "ok"},
				llm.ToolCallBlock{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go","opts":{"n":1},"list":[{"k":"v"}]}`)},
			}}, ReasoningRedacted: 2})
		mustAppend(t, l, ToolResult, ToolResultData{ToolCallID: "c1", Text: "body", IsError: true})
		mustAppend(t, l, ToolResult, ToolResultData{ToolCallID: "c2", Content: llm.Content{
			llm.TextBlock{Text: "body"},
			llm.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}},
		}})
		mustAppend(t, l, StepEnd, map[string]any{"usage": map[string]any{"input_tokens": 1}})
	}
	plain, armed := NewLog(), NewLog()
	armed.SetScrubber(keyScrub)
	appendAll(plain)
	appendAll(armed)
	want, got := plain.Events(), armed.Events()
	if len(want) != len(got) {
		t.Fatalf("%d events, want %d", len(got), len(want))
	}
	for i := range want {
		var w, g any
		if err := json.Unmarshal(want[i].Data, &w); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(got[i].Data, &g); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(w, g) {
			t.Errorf("event %d (%s) changed under an armed scrubber:\n got %s\nwant %s", want[i].Seq, want[i].Type, got[i].Data, want[i].Data)
		}
	}
}
