package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// Public returns an owned event suitable for UI and export. Adapter-private
// continuation data is deliberately absent; this is not a resumable journal.
func (e Event) Public() (Event, error) {
	if (e.Type == UserMessage || e.Type == AssistantMessage) && len(e.Data) > 0 {
		// The rewrite re-encodes an object or array into new bytes, so the
		// event's own are not copied first only to be replaced. A bare scalar
		// comes back as a window onto the input, and is copied.
		raw, err := rewritePayload(e.Data, nil, true, nil, false, 0)
		if err != nil {
			return Event{}, err
		}
		if len(raw) > 0 && raw[0] != '{' && raw[0] != '[' {
			raw = append(json.RawMessage(nil), raw...)
		}
		e.Data = raw
		return e, nil
	}
	return cloneEvent(e), nil
}

// PublicEvents omits opaque provider continuation state. Events and Store keep
// the private resumable journal; applications must restrict its retention.
func (l *Log) PublicEvents() ([]Event, error) {
	// Not Events(): Public returns an owned event on its own, so copying
	// every payload first only to copy or replace it again was waste.
	events := l.snapshot()
	for i := range events {
		e, err := events[i].Public()
		if err != nil {
			return nil, fmt.Errorf("session: public event %d: %w", events[i].Seq, err)
		}
		events[i] = e
	}
	return events, nil
}

// scrubPayload is how a payload is scrubbed on its way into a log, for both
// the live append and a sensitive restore, so the two cannot differ. It also
// reports whether the scrubber rewrote any string in the payload, which is what
// a restore needs to know to keep later signed reasoning valid.
func scrubPayload(t Type, raw json.RawMessage, scrub func(string) string) (json.RawMessage, bool, error) {
	rewrote := false
	watching := func(s string) string {
		out := scrub(s)
		if out != s {
			rewrote = true
		}
		return out
	}
	if t == AssistantMessage {
		var err error
		if raw, err = dropScrubbedSignedReasoning(raw, watching); err != nil {
			return nil, false, err
		}
	}
	out, err := rewritePayload(raw, watching, false, nil, false, 0)
	return out, rewrote, err
}

// dropPrefixInvalidatedReasoning removes the signed reasoning from an assistant
// message and adds the count to its reasoning_redacted. A restore uses it for
// the messages after one it rewrote; see RestoreSensitiveLog.
func dropPrefixInvalidatedReasoning(raw json.RawMessage) (json.RawMessage, error) {
	var data MessageData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	msg, n := llm.DropSignedReasoning(data.Message)
	if n == 0 {
		return raw, nil
	}
	data.Message = msg
	data.ReasoningRedacted += n
	return json.Marshal(data)
}

// dropScrubbedSignedReasoning removes the signed reasoning from an assistant
// message whose reasoning the scrubber would change, and records how many
// blocks went in the payload's reasoning_redacted.
//
// A signed thinking block is only good as produced. Anthropic's thinking
// documentation: "Pass every `thinking` block back to the API complete and
// unmodified, alongside the `tool_use` block it accompanied", and "Modified
// thinking blocks are rejected with a 400 error". Rewriting the text and keeping
// the signature, as the generic walk does, builds exactly that block and fails
// every later request in the session. Keeping the text instead would persist
// the secret the scrubber exists to remove. So the block goes, and the history
// replays without it.
//
// The whole signed run goes, not the one block: "Within the latest assistant
// message, the sequence of consecutive `thinking` blocks must match what the
// model generated in the original request: you can't rearrange, edit, or
// partially drop them", and a kept block whose neighbour was removed leaves a
// gap ("What fails is a gap"). The documented recovery is to send the history
// without its thinking blocks, each turn's other blocks left in place, and this
// adapter only ever sends adaptive thinking, where "no assistant turn needs to
// start with" a thinking block. Reasoning without a signature (a provider that
// signs nothing) has no such constraint and is still scrubbed in place by the
// generic walk, as is a signed block the scrubber leaves unchanged.
func dropScrubbedSignedReasoning(raw json.RawMessage, scrub func(string) string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return raw, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return nil, err
	}
	message, content := messageBlocks(payload)
	if message == nil || content == nil {
		// Not a message the projection can read; the typed decode that follows
		// refuses it with a better error than this walk could give.
		return raw, nil
	}

	type head struct {
		Kind      string `json:"kind"`
		Text      string `json:"text"`
		Signature string `json:"signature"`
	}
	signed := make([]bool, len(content))
	changed, run := false, 0
	for i, block := range content {
		var h head
		if json.Unmarshal(block, &h) != nil || h.Kind != string(llm.KindReasoning) || h.Signature == "" {
			continue
		}
		signed[i] = true
		run++
		if scrub(h.Text) != h.Text {
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}

	kept := make([]json.RawMessage, 0, len(content)-run)
	for i, block := range content {
		if !signed[i] {
			kept = append(kept, block)
		}
	}
	var err error
	if message["content"], err = json.Marshal(kept); err != nil {
		return nil, err
	}
	if payload["message"], err = json.Marshal(message); err != nil {
		return nil, err
	}
	if payload["reasoning_redacted"], err = json.Marshal(run); err != nil {
		return nil, err
	}
	return json.Marshal(payload)
}

// messageBlocks splits a message payload into the message object and its
// content blocks. Both are nil when the payload does not hold them.
func messageBlocks(payload map[string]json.RawMessage) (message map[string]json.RawMessage, content []json.RawMessage) {
	if json.Unmarshal(payload["message"], &message) != nil || json.Unmarshal(message["content"], &content) != nil {
		return nil, nil
	}
	return message, content
}

// Walk encoded values rather than replacing JSON bytes: quoted, escaped and
// Unicode text receives exactly the same scrubber input as ordinary text.
//
// free is true inside a tool call's arguments, whose keys are the model's own
// and not part of the record's schema; see renameKeys.
func rewritePayload(raw json.RawMessage, scrub func(string) string, public bool, path []string, free bool, depth int) (json.RawMessage, error) {
	if depth > 128 {
		return nil, errors.New("session: payload nesting exceeds 128")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("session: empty JSON value")
	}
	switch raw[0] {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		var blockKind string
		if isContentPath(path) {
			if err := json.Unmarshal(obj["kind"], &blockKind); err != nil {
				return nil, err
			}
		}
		for key, value := range obj {
			if opaqueKey(blockKind, path, key) {
				// Image data stays in the public projection; a signature and
				// replay state are private and leave it.
				if public && blockKind != "image" {
					delete(obj, key)
				}
				continue
			}
			// The path keeps the key as recorded: what a position means does
			// not change with what the scrubber makes of its name.
			next := append(append([]string(nil), path...), key)
			rewritten, err := rewritePayload(value, scrub, public, next, free || (key == "arguments" && (len(path) == 0 || blockKind == "tool_call")), depth+1)
			if err != nil {
				return nil, err
			}
			obj[key] = rewritten
		}
		if scrub != nil {
			renameKeys(obj, scrub, func(key string) bool {
				return opaqueKey(blockKind, path, key) || (!free && schemaKeys[key])
			})
		}
		return json.Marshal(obj)
	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, err
		}
		for i, value := range values {
			v, err := rewritePayload(value, scrub, public, append(append([]string(nil), path...), "[]"), free, depth+1)
			if err != nil {
				return nil, err
			}
			values[i] = v
		}
		return json.Marshal(values)
	case '"':
		if scrub == nil {
			return raw, nil
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		return json.Marshal(scrub(value))
	default:
		if !json.Valid(raw) {
			return nil, errors.New("session: invalid JSON value")
		}
		return raw, nil
	}
}

// opaqueKey reports whether the walk leaves this key's value alone: image
// bytes, a reasoning signature, and an adapter's replay state are continuation
// data the scrubber must not touch. Their names are not renamed either.
func opaqueKey(blockKind string, path []string, key string) bool {
	return (blockKind == "image" && key == "data") ||
		(blockKind == "reasoning" && key == "signature") ||
		(len(path) == 2 && path[0] == "message" && path[1] == "provenance" && key == "replay_state")
}

// schemaKeys is every key the session record's own readers look up, in one
// place: the fields of each event payload and of the llm types a message is
// built from, read off their json tags, and the wire keys of llm's codec, which
// its unexported block type owns. A scrubber is told about secret values, and a
// secret that happens to equal one of these names (an API key that is the word
// "arguments") must not rename the field and break the record without an error,
// so these names are never rewritten outside a tool call's free-form arguments.
// TestSchemaKeysCoverEveryRecordField fails when a payload gains a field this
// does not hold.
var schemaKeys = collectSchemaKeys(
	[]string{"kind", "role", "content", "provenance"},
	SystemPromptData{}, NullResponseData{}, VerifyReportData{}, PromptAssembledData{}, StepEndData{},
	MessageData{}, ToolCallData{}, ToolResultData{}, DenialData{}, QualificationData{}, GrantData{},
	DropData{}, CompactionData{}, MalformedCallData{}, DecodingData{}, OverflowData{},
	llm.TextBlock{}, llm.ReasoningBlock{}, llm.ImageBlock{}, llm.ToolCallBlock{}, llm.ToolResultBlock{},
	llm.AssistantProvenance{}, llm.Drop{}, llm.Usage{},
)

func collectSchemaKeys(wire []string, payloads ...any) map[string]bool {
	keys := make(map[string]bool)
	for _, key := range wire {
		keys[key] = true
	}
	for _, payload := range payloads {
		t := reflect.TypeOf(payload)
		for i := 0; i < t.NumField(); i++ {
			if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
				keys[name] = true
			}
		}
	}
	return keys
}

// renameKeys runs the scrubber over the object's key names and moves each entry
// whose name changed. A secret can sit in a key as easily as in a value: a map
// keyed by token, a URL that carries one. The scrubber is the one the values
// pass through, so a rewritten name flips the same flag a rewritten value does.
// Keys for which keep reports true are never renamed: the opaque ones, and the
// record's schema keys (see schemaKeys), which is also what keeps an unarmed
// scrubber, whose Clean returns one notice for any text, from renaming every
// field of the record.
//
// Two keys can scrub to the same name. Neither entry is dropped: every key the
// scrubber leaves alone keeps its name, and the rest take the scrubbed name in
// byte order of their original keys, a later one getting "#2", "#3", ... after
// it. The suffix is not secret-shaped, so scrubbing the result again changes
// nothing. Each name counts its own suffixes forward, so n colliding keys cost
// n, not n squared.
func renameKeys(obj map[string]json.RawMessage, scrub func(string) string, keep func(string) bool) {
	type move struct {
		key, name string
		value     json.RawMessage
	}
	var moves []move
	for key := range obj {
		if keep(key) {
			continue
		}
		if name := scrub(key); name != key {
			moves = append(moves, move{key: key, name: name})
		}
	}
	if len(moves) == 0 {
		return
	}
	sort.Slice(moves, func(i, j int) bool { return moves[i].key < moves[j].key })
	// Every renamed entry leaves before any arrives: one key's new name can be
	// another's old one.
	for i := range moves {
		moves[i].value = obj[moves[i].key]
		delete(obj, moves[i].key)
	}
	next := make(map[string]int)
	for _, m := range moves {
		candidate := m.name
		if _, taken := obj[candidate]; taken {
			n := max(next[m.name], 2)
			for {
				candidate = fmt.Sprintf("%s#%d", m.name, n)
				n++
				if _, taken := obj[candidate]; !taken {
					break
				}
			}
			next[m.name] = n
		}
		obj[candidate] = m.value
	}
}

// Only canonical content blocks are opaque; an arbitrary tool argument object
// claiming kind:image must still have its textual fields scrubbed.
func isContentPath(path []string) bool {
	if len(path) > 0 && path[0] == "message" {
		path = path[1:]
	}
	if len(path) < 2 || len(path)%2 != 0 {
		return false
	}
	for i := 0; i < len(path); i += 2 {
		if path[i] != "content" || path[i+1] != "[]" {
			return false
		}
	}
	return true
}
