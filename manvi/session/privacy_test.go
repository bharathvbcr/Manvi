package session

import (
	"encoding/json"
	"fmt"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"reflect"
	"strings"
	"testing"
	"time"
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

// appendStructural appends one of every payload shape the agent emits that
// carries the record's structural keys, with no secret anywhere in it.
func appendStructural(t *testing.T, l *Log) {
	t.Helper()
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
	mustAppend(t, l, ToolCall, ToolCallData{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)})
	mustAppend(t, l, PolicyDenied, DenialData{ToolCallID: "c1", Tool: "read", Rule: "r", Severity: "block", Reason: "no"})
	mustAppend(t, l, StepEnd, StepEndData{Usage: llm.Usage{InputTokens: 1, OutputTokens: 2}})
}

// shapeOf lists every key path in a JSON document, leaving out what sits under
// a replay state or a tool call's arguments: adapter and model data, not the
// record's schema.
func shapeOf(t *testing.T, raw json.RawMessage) map[string]bool {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("not JSON: %v: %s", err, raw)
	}
	paths := map[string]bool{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, child := range v {
				paths[prefix+"/"+k] = true
				if k != "arguments" && k != "replay_state" {
					walk(prefix+"/"+k, child)
				}
			}
		case []any:
			for _, child := range v {
				walk(prefix+"[]", child)
			}
		}
	}
	walk("", doc)
	return paths
}

// An unarmed credential scrubber answers every text with one notice, so a
// walk that renamed keys through it would give every field of every record the
// same name. A real armed scrubber, one that is nil, and that unarmed notice
// are the three states a session log can hold; each must leave the record's
// keys as they were. (The notice is modelled with credentials.Withheld: an
// unarmed scrubber cannot be built from outside its package.)
func TestStructuralKeysSurviveEveryScrubberState(t *testing.T) {
	armed := credentials.NewScrubber()
	armed.WatchValue(keySecret, "test")
	if armed.Count() != 1 {
		t.Fatalf("test scrubber watches %d values, want 1", armed.Count())
	}
	unarmed := func(string) string {
		return credentials.Withheld + ": the credential scrubber could not watch a value]"
	}
	plain := NewLog()
	appendStructural(t, plain)
	var payloads []Event
	for _, e := range plain.Events() {
		if len(e.Data) > 0 {
			payloads = append(payloads, e)
		}
	}

	for name, scrub := range map[string]func(string) string{"armed": armed.Clean, "unarmed": unarmed} {
		t.Run(name+"/payloads", func(t *testing.T) {
			for _, e := range payloads {
				out, _, err := scrubPayload(e.Type, e.Data, scrub)
				if err != nil {
					t.Fatalf("%s: %v", e.Type, err)
				}
				want, got := shapeOf(t, e.Data), shapeOf(t, out)
				// A signed reasoning block whose text the scrubber changes is
				// dropped whole, which is its own rule, not a rename.
				delete(want, "/message/content[]/signature")
				delete(got, "/message/content[]/signature")
				for path := range want {
					if !got[path] {
						t.Errorf("%s: key %s was renamed or lost: %s", e.Type, path, out)
					}
				}
				for path := range got {
					if !want[path] {
						t.Errorf("%s: unexpected key %s: %s", e.Type, path, out)
					}
				}
			}
		})
	}

	t.Run("armed/log", func(t *testing.T) {
		l := NewLog()
		l.SetScrubber(armed.Clean)
		appendStructural(t, l)
		sameEvents(t, plain.Events(), l.Events())
	})
	t.Run("nil/log", func(t *testing.T) {
		public, err := plain.PublicEvents()
		if err != nil {
			t.Fatal(err)
		}
		for i, e := range plain.Events() {
			if e.Type != UserMessage && e.Type != AssistantMessage {
				continue
			}
			want := shapeOf(t, e.Data)
			delete(want, "/message/provenance/replay_state")
			if e.Type == AssistantMessage {
				delete(want, "/message/content[]/signature")
			}
			got := shapeOf(t, public[i].Data)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("public %s changed keys beyond the private continuation:\n got %v\nwant %v", e.Type, got, want)
			}
		}
	})
}

func sameEvents(t *testing.T, want, got []Event) {
	t.Helper()
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

// Every key of every record the agent emits is a schema key, so the list the
// walk consults cannot fall behind the records it protects.
func TestSchemaKeysCoverTheRecordsTheAgentEmits(t *testing.T) {
	l := NewLog()
	appendStructural(t, l)
	for _, e := range l.Events() {
		if len(e.Data) == 0 {
			continue
		}
		for path := range shapeOf(t, e.Data) {
			key := path[strings.LastIndex(path, "/")+1:]
			if !schemaKeys[key] {
				t.Errorf("%s: key %q is not in schemaKeys", e.Type, key)
			}
		}
	}
}

// A watched value can equal a structural name. Renaming that field would break
// the call or result with no error, so the schema's keys keep their names and
// the value the scrubber was told about is still removed where it is data.
func TestSchemaKeyNamedLikeASecretIsNotRenamed(t *testing.T) {
	for _, name := range []string{"kind", "role", "content", "message", "arguments", "provenance", "tool_call_id", "media_type", "reasoning_redacted", "is_error", "replay_state"} {
		t.Run(name, func(t *testing.T) {
			scrub := func(s string) string { return strings.ReplaceAll(s, name, "[redacted]") }
			plain, scrubbed := NewLog(), NewLog()
			scrubbed.SetScrubber(scrub)
			appendStructural(t, plain)
			appendStructural(t, scrubbed)
			sameEvents(t, plain.Events(), scrubbed.Events())
			if _, err := scrubbed.DeriveMessages(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Inside a tool call's arguments the keys are the model's own, so a schema word
// there is data like any other: scrubbed when watched, with the call's own
// fields beside it untouched.
func TestArgumentKeysAreNotSchemaKeys(t *testing.T) {
	scrub := func(s string) string { return strings.ReplaceAll(s, "name", "[redacted]") }
	out, rewrote, err := scrubPayload(AssistantMessage, mustJSON(t, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ToolCallBlock{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"name":1,"deep":{"name":2}}`)}}}}), scrub)
	if err != nil {
		t.Fatal(err)
	}
	if !rewrote || strings.Contains(string(out), `"name":1`) || strings.Contains(string(out), `"name":2`) {
		t.Fatalf("a watched argument key survived (rewrote=%v): %s", rewrote, out)
	}
	if !strings.Contains(string(out), `"name":"read"`) {
		t.Fatalf("the call's own name field was renamed: %s", out)
	}
}

// n keys that scrub to one name are n moves, not n squared: the suffix search
// resumes where the name last stopped. A quadratic search takes minutes here;
// the bound is generous so a slow machine does not trip it.
func TestManyCollidingKeysAreLinear(t *testing.T) {
	const n = 20000
	var doc strings.Builder
	doc.WriteString(`{"[redacted]#7":"literal"`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&doc, `,"k%05d":%d`, i, i)
	}
	doc.WriteString("}")
	start := time.Now()
	out, rewrote, err := scrubPayload(StepEnd, json.RawMessage(doc.String()), func(s string) string {
		if strings.HasPrefix(s, "k") {
			return "[redacted]"
		}
		return s
	})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("%d colliding keys took %v", n, took)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !rewrote || len(got) != n+1 || string(got["[redacted]#7"]) != `"literal"` {
		t.Fatalf("rewrote=%v, %d entries, want %d with the literal key kept", rewrote, len(got), n+1)
	}
	// Byte order of the original keys: k00000 takes the bare name, k00001 #2, and
	// the literal #7 is stepped over.
	if string(got["[redacted]"]) != "0" || string(got["[redacted]#2"]) != "1" || string(got["[redacted]#8"]) != "6" {
		t.Fatalf("suffixes are not in key order: %s %s %s", got["[redacted]"], got["[redacted]#2"], got["[redacted]#8"])
	}
}

// Where a key sits does not change what happens to it: arrays of arrays, deep
// nesting and a value that carries the same secret all lose it, and scrubbing the
// result again changes nothing.
func TestKeysAreScrubbedWhereverTheySit(t *testing.T) {
	cases := map[string]string{
		"array of arrays": `{"a":[[{"` + keySecret + `":1}],[[{"x-` + keySecret + `":2}]]]}`,
		"deep":            strings.Repeat(`{"k":`, 100) + `{"` + keySecret + `":1}` + strings.Repeat(`}`, 100),
		"key and value":   `{"` + keySecret + `":"` + keySecret + `"}`,
		"long key":        `{"` + strings.Repeat("k", 1<<20) + keySecret + `":1,"":2}`,
	}
	for name, doc := range cases {
		out, rewrote, err := scrubPayload(StepEnd, json.RawMessage(doc), keyScrub)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !rewrote || strings.Contains(string(out), keySecret) {
			t.Errorf("%s: rewrote=%v, secret survived", name, rewrote)
		}
		again, rewroteAgain, err := scrubPayload(StepEnd, out, keyScrub)
		if err != nil || rewroteAgain || string(again) != string(out) {
			t.Errorf("%s: a second scrub changed the result (rewrote=%v, err=%v)", name, rewroteAgain, err)
		}
	}
}

// A restore of a restore is a fixed point even when the journal holds keys that
// collide with the names the first restore chooses.
func TestRestoreSensitiveLogWithCollidingKeysIsAFixedPoint(t *testing.T) {
	call := MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ToolCallBlock{ID: "c1", Name: "http", Arguments: json.RawMessage(`{"` + keySecret + `":1,"[redacted]":2,"x-` + keySecret + `":3}`)}}}}
	journal := journalOf(t, MessageData{Message: user("one")}, call, MessageData{Message: user("two")}, signedTurn("t", "b", "SIG"))
	first, err := RestoreSensitiveLog(journal, keyScrub)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RestoreSensitiveLog(first.Events(), keyScrub)
	if err != nil {
		t.Fatal(err)
	}
	a, b := mustJSON(t, first.Events()), mustJSON(t, second.Events())
	if string(a) != string(b) {
		t.Fatalf("restore is not a fixed point:\n%s\n%s", a, b)
	}
	if strings.Contains(string(a), keySecret) {
		t.Fatalf("secret key survived: %s", a)
	}
}
