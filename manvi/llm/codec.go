package llm

import (
	"encoding/json"
	"fmt"
)

// ContentBlock is an interface, so encoding/json cannot decode into it without
// help. Every block is tagged with its kind on the wire and dispatched on the
// way back.
//
// An unknown kind is an error rather than a skipped block. The session log is
// the record of what the model saw; silently dropping a block a newer version
// wrote would make an old binary's replay quietly differ from the original run,
// which is exactly the class of divergence the log exists to rule out.

type wireBlock struct {
	Kind BlockKind `json:"kind"`

	// text / reasoning
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
	Redacted  bool   `json:"redacted,omitempty"`

	// image
	MediaType string `json:"media_type,omitempty"`
	Data      []byte `json:"data,omitempty"`

	// tool call
	ID        CallID          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`

	// tool result
	ToolCallID CallID      `json:"tool_call_id,omitempty"`
	Content    []wireBlock `json:"content,omitempty"`
	IsError    bool        `json:"is_error,omitempty"`
}

func toWire(block ContentBlock) (wireBlock, error) {
	switch b := block.(type) {
	case TextBlock:
		return wireBlock{Kind: KindText, Text: b.Text}, nil
	case ReasoningBlock:
		return wireBlock{Kind: KindReasoning, Text: b.Text, Signature: b.Signature, Redacted: b.Redacted}, nil
	case ImageBlock:
		return wireBlock{Kind: KindImage, MediaType: b.MediaType, Data: b.Data}, nil
	case ToolCallBlock:
		return wireBlock{Kind: KindToolCall, ID: b.ID, Name: b.Name, Arguments: b.Arguments}, nil
	case ToolResultBlock:
		nested := make([]wireBlock, 0, len(b.Content))
		for _, inner := range b.Content {
			encoded, err := toWire(inner)
			if err != nil {
				return wireBlock{}, err
			}
			nested = append(nested, encoded)
		}
		return wireBlock{Kind: KindToolResult, ToolCallID: b.ToolCallID, Content: nested, IsError: b.IsError}, nil
	default:
		return wireBlock{}, fmt.Errorf("llm: cannot encode content block of type %T", block)
	}
}

func fromWire(w wireBlock) (ContentBlock, error) {
	switch w.Kind {
	case KindText:
		return TextBlock{Text: w.Text}, nil
	case KindReasoning:
		return ReasoningBlock{Text: w.Text, Signature: w.Signature, Redacted: w.Redacted}, nil
	case KindImage:
		return ImageBlock{MediaType: w.MediaType, Data: w.Data}, nil
	case KindToolCall:
		return ToolCallBlock{ID: w.ID, Name: w.Name, Arguments: w.Arguments}, nil
	case KindToolResult:
		nested := make([]ContentBlock, 0, len(w.Content))
		for _, inner := range w.Content {
			decoded, err := fromWire(inner)
			if err != nil {
				return nil, err
			}
			nested = append(nested, decoded)
		}
		return ToolResultBlock{ToolCallID: w.ToolCallID, Content: nested, IsError: w.IsError}, nil
	case "":
		return nil, fmt.Errorf("llm: content block has no kind")
	default:
		return nil, fmt.Errorf("llm: unknown content block kind %q", w.Kind)
	}
}

type wireMessage struct {
	Role       Role                 `json:"role"`
	Content    []wireBlock          `json:"content"`
	Provenance *AssistantProvenance `json:"provenance,omitempty"`
}

// MarshalJSON encodes a message with kind-tagged content blocks.
func (m Message) MarshalJSON() ([]byte, error) {
	blocks := make([]wireBlock, 0, len(m.Content))
	for _, block := range m.Content {
		encoded, err := toWire(block)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, encoded)
	}
	return json.Marshal(wireMessage{Role: m.Role, Content: blocks, Provenance: m.Provenance})
}

// UnmarshalJSON decodes a message, dispatching each block on its kind.
func (m *Message) UnmarshalJSON(data []byte) error {
	var wire wireMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	blocks := make([]ContentBlock, 0, len(wire.Content))
	for _, w := range wire.Content {
		decoded, err := fromWire(w)
		if err != nil {
			return err
		}
		blocks = append(blocks, decoded)
	}
	m.Role = wire.Role
	m.Content = blocks
	m.Provenance = wire.Provenance
	return nil
}
