package session

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// The wire frame of a conversation.
//
// A provider that binds each thinking block to the conversation before it
// (llm.BindsThinkingToHistory) signs the top-level system prompt and the full
// tool definitions along with the messages, so a request that replays a block
// must carry the same system and the same tools as the request that produced
// it (shared/model-migration.md, "Breaking change 3"). The frame is the record
// of what the conversation sent: the system prompt of its first request and the
// tool set its requests declared. It lives in the log so that it survives a
// resume and is the same for every Loop that drives the conversation.
//
// Two events carry it:
//
//   - WireFrame opens the frame, and again each time the declared tool set
//     grows. A frame that grows is a boundary: the tools sit at the front of
//     the prefix, so every signed block before it is stale. staleMessages marks
//     them, and StripStaleReasoning removes them. They are a leading run, which
//     is the documented removal that keeps later blocks valid.
//   - WireNotice is a mid-conversation system message: a change of instructions
//     or of which tools are available, appended where it became true. The
//     projection replays it as a role:"system" message at the same place in
//     every later request, and it is never rewritten.

// WhyOpened and WhyToolsAdded say why a frame was recorded.
const (
	WhyOpened     = "opened"
	WhyToolsAdded = "tools_added"
)

const (
	// WireFrame records the conversation's declared system prompt and tools.
	WireFrame Type = "wire/frame"
	// WireNotice records a mid-conversation system message.
	WireNotice Type = "wire/notice"
)

type (
	// FrameData is what a conversation declared to its provider: the system
	// prompt of its first request and the tool definitions, sorted by name so
	// the bytes sent are the same on every request. Added names the tools a
	// WhyToolsAdded frame brought in.
	FrameData struct {
		System string           `json:"system"`
		Tools  []llm.ToolSchema `json:"tools,omitempty"`
		Why    string           `json:"why"`
		Added  []string         `json:"added,omitempty"`
	}
	// NoticeData is one mid-conversation system message. Text is exactly what
	// the model was sent, rendered once when the notice was recorded, so a
	// later change to the wording never rewrites history. Prompt is the whole
	// system prompt a notice delivered, and Offered the tool names it told the
	// model were available; the Log reads them back as what the model was last
	// told, so a notice is written only when something differs from that.
	NoticeData struct {
		Text    string   `json:"text"`
		Prompt  string   `json:"prompt,omitempty"`
		Offered []string `json:"offered,omitempty"`
	}
)

// WireState is what a conversation has told its provider so far.
type WireState struct {
	// Frame is the declared system prompt and tool set, nil before the first
	// request of a conversation.
	Frame *FrameData
	// Delivered is the system prompt the model currently holds: the frame's, or
	// the latest one a notice delivered.
	Delivered string
	// Told is the tool names the latest tools notice said were available; nil
	// when none was sent, which means every declared tool.
	Told []string
}

// wireState is the log's running WireState, kept as events are appended so a
// request does not rescan a long log. boundaries counts the frames that grew
// the tool set, which is what makes StripStaleReasoning look at the log.
type wireState struct {
	WireState
	boundaries int
}

// fold applies one WireFrame or WireNotice event. Other types are ignored.
func (w *wireState) fold(t Type, raw json.RawMessage) error {
	switch t {
	case WireFrame:
		var data FrameData
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("session: decoding %s: %w", t, err)
		}
		if data.Why == WhyToolsAdded {
			w.boundaries++
		}
		if w.Frame == nil {
			w.Delivered = data.System
		}
		w.Frame = &data
	case WireNotice:
		var data NoticeData
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("session: decoding %s: %w", t, err)
		}
		if data.Prompt != "" {
			w.Delivered = data.Prompt
		}
		if len(data.Offered) > 0 {
			w.Told = slices.Clone(data.Offered)
		}
	}
	return nil
}

// Wire returns what the conversation has told its provider so far.
func (l *Log) Wire() WireState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := l.wire.WireState
	if out.Frame != nil {
		frame := *out.Frame
		frame.Tools = slices.Clone(frame.Tools)
		frame.Added = slices.Clone(frame.Added)
		out.Frame = &frame
	}
	out.Told = slices.Clone(out.Told)
	return out
}

// NoticeMessage is the message a notice replays as.
func NoticeMessage(data NoticeData) llm.Message {
	return llm.Message{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.TextBlock{Text: data.Text}}}
}
