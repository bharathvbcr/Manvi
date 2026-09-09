package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// The Interactions stateless contract requires every returned step, including
// thought steps, in its original order. See the official function-calling guide
// (Stateless function calling), checked 2026-09-09. Unknown step fields remain
// opaque; thought signatures are never moved to a neighboring function call.
//
// Indexed SSE frames assemble a step. A complete step.stop or interaction.steps
// snapshot, when supplied, is authoritative. Old unindexed journal fixtures
// remain readable through the legacy call-signature path.
type continuation struct {
	order    []*retainedStep
	indexed  map[int]*retainedStep
	complete []json.RawMessage
	bytes    int
}

type retainedStep struct {
	fields  map[string]json.RawMessage
	kind    string
	text    strings.Builder
	thought strings.Builder
	call    *callAccumulator
	final   bool
}

func inputContent(block llm.ContentBlock) (wireContent, error) {
	switch b := block.(type) {
	case llm.TextBlock:
		return wireContent{Type: DeltaText, Text: b.Text}, nil
	case llm.ImageBlock:
		if len(b.Data) == 0 {
			return wireContent{}, errors.New("image has no data")
		}
		switch b.MediaType {
		case "image/png", "image/jpeg", "image/webp", "image/gif", "image/heic", "image/heif", "image/bmp", "image/tiff":
		default:
			return wireContent{}, fmt.Errorf("unsupported image media type %q", b.MediaType)
		}
		return wireContent{Type: "image", MIMEType: b.MediaType, Data: b.Data}, nil
	default:
		return wireContent{}, fmt.Errorf("function result content must be text or image, received %T", block)
	}
}

func (c *continuation) observe(kind string, raw []byte, ev wireEvent, calls map[int]*callAccumulator) error {
	var envelope struct {
		Index       *int                       `json:"index"`
		Step        map[string]json.RawMessage `json:"step"`
		Interaction *struct {
			Steps []json.RawMessage `json:"steps"`
		} `json:"interaction"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if envelope.Interaction != nil && len(envelope.Interaction.Steps) > 0 {
		c.complete = envelope.Interaction.Steps
		for _, step := range c.complete {
			c.bytes += len(step)
		}
	}
	if envelope.Index == nil {
		return nil
	}
	index := *envelope.Index
	if index < 0 {
		return errors.New("gemini: negative continuation step index")
	}
	if c.indexed == nil {
		c.indexed = make(map[int]*retainedStep)
	}
	if kind == EventStepStart && ev.Step != nil {
		s := &retainedStep{fields: envelope.Step, kind: ev.Step.Type, call: calls[index]}
		c.indexed[index] = s
		c.order = append(c.order, s)
		c.bytes += transport.RetainedAccumulatorBytes
		for k, v := range s.fields {
			c.bytes += len(k) + len(v)
		}
		return nil
	}
	s := c.indexed[index]
	if s == nil {
		return nil
	} // legacy streams can omit step.start
	if kind == EventStepStop {
		if len(envelope.Step) > 0 {
			s.fields = envelope.Step
			s.final = true
			for k, v := range s.fields {
				c.bytes += len(k) + len(v)
			}
		}
		return nil
	}
	if kind != EventStepDelta {
		return nil
	}
	if ev.Delta == nil {
		// The documented nested form carries content fragments inside step.
		if ev.Step != nil {
			for _, part := range ev.Step.Content {
				if part.Type == DeltaText {
					s.text.WriteString(part.Text)
					c.bytes += len(part.Text)
				} else if part.Type == DeltaThought {
					v := part.Thought
					if v == "" {
						v = part.Text
					}
					s.thought.WriteString(v)
					c.bytes += len(v)
				}
			}
		}
		return nil
	}
	d := ev.Delta
	switch d.Type {
	case DeltaText:
		s.text.WriteString(d.Text)
		c.bytes += len(d.Text)
	case DeltaThought:
		v := d.Thought
		if v == "" {
			v = d.Text
		}
		s.thought.WriteString(v)
		c.bytes += len(v)
	case DeltaThoughtSignature:
		encoded, err := json.Marshal(d.Signature)
		if err != nil {
			return err
		}
		s.fields["signature"] = encoded
		c.bytes += len(encoded)
	case DeltaArguments, DeltaArgumentsDelta:
		// The decoder owns argument framing; this retains its exact accumulator.
		s.call = calls[index]
	default:
		return fmt.Errorf("gemini: cannot preserve continuation delta %q", d.Type)
	}
	return nil
}

func (c *continuation) steps() ([]json.RawMessage, error) {
	if len(c.complete) > 0 {
		return c.complete, nil
	}
	var out []json.RawMessage
	for _, s := range c.order {
		fields := make(map[string]json.RawMessage, len(s.fields))
		for k, v := range s.fields {
			fields[k] = v
		}
		if !s.final {
			if s.call != nil && s.kind == TypeFunctionCall {
				args := s.call.args.String()
				if args == "" {
					args = "{}"
				}
				if !isJSONObject(json.RawMessage(args)) {
					return nil, errors.New("gemini: continuation contains invalid function arguments")
				}
				fields["arguments"] = json.RawMessage(args)
			}
			for _, part := range []struct{ field, text string }{{"content", s.text.String()}, {"summary", s.thought.String()}} {
				if part.text == "" {
					continue
				}
				var content []json.RawMessage
				if len(fields[part.field]) > 0 {
					if err := json.Unmarshal(fields[part.field], &content); err != nil {
						return nil, err
					}
				}
				block, err := json.Marshal(wireContent{Type: DeltaText, Text: part.text})
				if err != nil {
					return nil, err
				}
				content = append(content, block)
				fields[part.field], err = json.Marshal(content)
				if err != nil {
					return nil, err
				}
			}
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// Replaying opaque fields must not override a caller's changed tool arguments
// or prose. That would send a request different from the neutral journal.
func validateContinuation(msg llm.Message, steps []json.RawMessage) error {
	var calls []llm.ToolCallBlock
	var text strings.Builder
	for _, raw := range steps {
		var step struct {
			Type, ID, Name string
			Arguments      json.RawMessage
			Content        []wireContent
		}
		if err := json.Unmarshal(raw, &step); err != nil {
			return fmt.Errorf("gemini: invalid continuation step: %w", err)
		}
		if step.Type == "" {
			return errors.New("gemini: continuation step has no type")
		}
		if step.Type == TypeFunctionCall {
			calls = append(calls, llm.ToolCallBlock{ID: llm.CallID(step.ID), Name: step.Name, Arguments: step.Arguments})
		}
		if step.Type == InputModelOutput {
			for _, block := range step.Content {
				if block.Type == DeltaText {
					text.WriteString(block.Text)
				}
			}
		}
	}
	expected := msg.ToolCalls()
	if len(calls) != len(expected) || text.String() != msg.Text() {
		return errors.New("gemini: private continuation differs from journaled assistant content; use parameter references for sensitive data or start a new history")
	}
	for i, call := range calls {
		if call.ID != expected[i].ID || call.Name != expected[i].Name || !equalArguments(call.Arguments, expected[i].Arguments) {
			return errors.New("gemini: private continuation differs from journaled tool call; use parameter references for sensitive data or start a new history")
		}
	}
	return nil
}

func equalArguments(a, b json.RawMessage) bool {
	var left, right map[string]json.RawMessage
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	l, err := json.Marshal(left)
	if err != nil {
		return false
	}
	r, err := json.Marshal(right)
	return err == nil && bytes.Equal(l, r)
}
