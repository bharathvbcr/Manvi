package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Public returns an owned event suitable for UI and export. Adapter-private
// continuation data is deliberately absent; this is not a resumable journal.
func (e Event) Public() (Event, error) {
	out := cloneEvent(e)
	if (e.Type == UserMessage || e.Type == AssistantMessage) && len(e.Data) > 0 {
		raw, err := rewritePayload(e.Data, nil, true, nil, 0)
		if err != nil {
			return Event{}, err
		}
		out.Data = raw
	}
	return out, nil
}

// PublicEvents omits opaque provider continuation state. Events and Store keep
// the private resumable journal; applications must restrict its retention.
func (l *Log) PublicEvents() ([]Event, error) {
	events := l.Events()
	for i := range events {
		e, err := events[i].Public()
		if err != nil {
			return nil, fmt.Errorf("session: public event %d: %w", events[i].Seq, err)
		}
		events[i] = e
	}
	return events, nil
}

// Walk encoded values rather than replacing JSON bytes: quoted, escaped and
// Unicode text receives exactly the same scrubber input as ordinary text.
func rewritePayload(raw json.RawMessage, scrub func(string) string, public bool, path []string, depth int) (json.RawMessage, error) {
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
			if blockKind == "image" && key == "data" {
				continue
			}
			if blockKind == "reasoning" && key == "signature" {
				if public {
					delete(obj, key)
				}
				continue
			}
			if len(path) == 2 && path[0] == "message" && path[1] == "provenance" && key == "replay_state" {
				if public {
					delete(obj, key)
				}
				continue
			}
			next := append(append([]string(nil), path...), key)
			rewritten, err := rewritePayload(value, scrub, public, next, depth+1)
			if err != nil {
				return nil, err
			}
			obj[key] = rewritten
		}
		return json.Marshal(obj)
	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, err
		}
		for i, value := range values {
			v, err := rewritePayload(value, scrub, public, append(append([]string(nil), path...), "[]"), depth+1)
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
