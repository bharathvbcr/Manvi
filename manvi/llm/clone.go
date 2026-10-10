package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// CloneMessages returns an owned snapshot, including nested image bytes,
// tool arguments and provider-private continuation state. Unknown block types
// are rejected by the same codec used for persisted conversations.
//
// The result is exactly what a JSON round trip through that codec produces —
// that was the original implementation, and it stays the fallback — but the
// common case is copied structurally. The session log clones the whole history
// on every step, so a round trip made each step cost time and garbage in
// proportion to everything the turn had seen so far.
func CloneMessages(messages []Message) ([]Message, error) {
	if messages == nil {
		return nil, nil
	}
	copied := make([]Message, len(messages))
	for i, m := range messages {
		cloned, err := cloneMessage(m)
		if err != nil {
			// Including a block the codec rejects: its error is the one this
			// function has always reported, so let the codec report it.
			return cloneViaCodec(messages)
		}
		copied[i] = cloned
	}
	return copied, nil
}

// errNeedsCodec marks a value the structural copy cannot reproduce exactly —
// a string that is not valid UTF-8, which the encoder rewrites — so the whole
// slice goes through the codec instead.
var errNeedsCodec = errors.New("llm: value needs the codec")

func cloneViaCodec(messages []Message) ([]Message, error) {
	raw, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	var copied []Message
	if err := json.Unmarshal(raw, &copied); err != nil {
		return nil, err
	}
	return copied, nil
}

func cloneMessage(m Message) (Message, error) {
	if err := validStrings(string(m.Role)); err != nil {
		return Message{}, err
	}
	content, err := cloneBlocks(m.Content)
	if err != nil {
		return Message{}, err
	}
	out := Message{Role: m.Role, Content: content}
	if m.Provenance != nil {
		p := *m.Provenance
		if err := validStrings(p.Provider, p.Model); err != nil {
			return Message{}, err
		}
		if p.ReplayState, err = cloneRaw(p.ReplayState); err != nil {
			return Message{}, err
		}
		out.Provenance = &p
	}
	return out, nil
}

// cloneBlocks never returns nil: the codec decodes an absent or empty content
// list as an empty one.
func cloneBlocks(blocks []ContentBlock) ([]ContentBlock, error) {
	out := make([]ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		cloned, err := cloneBlock(block)
		if err != nil {
			return nil, err
		}
		out = append(out, cloned)
	}
	return out, nil
}

func cloneBlock(block ContentBlock) (ContentBlock, error) {
	switch b := block.(type) {
	case TextBlock:
		return b, validStrings(b.Text)
	case ReasoningBlock:
		return b, validStrings(b.Text, b.Signature)
	case ImageBlock:
		if err := validStrings(b.MediaType); err != nil {
			return nil, err
		}
		if len(b.Data) == 0 {
			b.Data = nil // omitted on the wire, so absent after a round trip
		} else {
			b.Data = append([]byte(nil), b.Data...)
		}
		return b, nil
	case ToolCallBlock:
		if err := validStrings(string(b.ID), b.Name); err != nil {
			return nil, err
		}
		args, err := cloneRaw(b.Arguments)
		if err != nil {
			return nil, err
		}
		b.Arguments = args
		return b, nil
	case ToolResultBlock:
		if err := validStrings(string(b.ToolCallID)); err != nil {
			return nil, err
		}
		nested, err := cloneBlocks(b.Content)
		if err != nil {
			return nil, err
		}
		b.Content = nested
		return b, nil
	default:
		return nil, fmt.Errorf("llm: cannot encode content block of type %T", block)
	}
}

// cloneRaw reproduces what the codec does to embedded JSON: an empty value is
// omitted, and anything else is validated and compacted by the encoder.
// Marshalling a RawMessage is that step alone, not a reflective walk.
func cloneRaw(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	return json.Marshal(raw)
}

func validStrings(values ...string) error {
	for _, v := range values {
		if !utf8.ValidString(v) {
			return errNeedsCodec
		}
	}
	return nil
}
