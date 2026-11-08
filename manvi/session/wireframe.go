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

// WhyOpened, WhyToolsAdded and WhyCarried say why a frame was recorded.
// WhyCarried is the store's restatement of the frame at the front of a file whose
// older turns retention dropped: it is the state of the dropped frames, not a
// change, so it is never a tools boundary. It restates the frame only. What the
// dropped notices had told the model goes with them: a resumed conversation's
// model never saw them, so the frame's own prompt and every declared tool are
// what it holds, and the next request says the rest again (a Loop whose prompt
// or offered tools differ from the frame's appends a notice). Carrying the
// notices' state instead would leave the model on the frame's prompt while the
// log said a later one was delivered.
//
// It is also the record of the trim itself. Retention removes the oldest turns,
// so every signed block kept in the file was bound to messages the file no
// longer holds. The carried frame's Through says up to which event that is: the
// last event the file held when it was trimmed. Blocks in later events were
// produced by the resumed conversation under the trimmed history and are valid.
// The frame is written at the front, where a second trim drops it and writes a
// new one with a later Through, so the latest record is always the one read.
const (
	WhyOpened     = "opened"
	WhyToolsAdded = "tools_added"
	WhyCarried    = "carried"
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
	// WhyToolsAdded frame brought in. Through is set only on a WhyCarried frame:
	// the sequence number of the last event in the file when retention trimmed it.
	FrameData struct {
		System  string           `json:"system"`
		Tools   []llm.ToolSchema `json:"tools,omitempty"`
		Why     string           `json:"why"`
		Added   []string         `json:"added,omitempty"`
		Through int              `json:"through,omitempty"`
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
// through is the latest event whose signed blocks a retention trim made stale
// (see WhyCarried); zero when the log was never trimmed.
//
// A notice counts as delivered only once an assistant message answered it. One
// that a user message (or tool result) follows instead rode a request that
// failed: the projection drops it (see projection.appendMessage), so the model
// never held what it said, and the state returns to what it was before it.
// awaiting is that notice's predecessor state.
type wireState struct {
	WireState
	boundaries int
	through    int
	awaiting   *WireState
	// stale is set only when the state is recomputed for a log that has been
	// edited (Log.Wire): the sequence numbers of the messages whose signed
	// reasoning StripStale removes. A reply that strip empties does not answer a
	// notice, because the request no longer carries it.
	stale map[int]bool
}

// fold applies one event. Types that do not touch the wire state are ignored,
// and cost nothing unless a notice is awaiting its answer.
func (w *wireState) fold(seq int, t Type, raw json.RawMessage) error {
	switch t {
	case UserMessage, ToolResult:
		if w.awaiting != nil {
			w.WireState, w.awaiting = *w.awaiting, nil
		}
	case AssistantMessage:
		if w.awaiting == nil {
			return nil
		}
		var data MessageData
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("session: decoding %s: %w", t, err)
		}
		if data.Message.HasSendableContent() && (!w.stale[seq] || !emptiedByStrip(data.Message)) {
			w.awaiting = nil
		}
	case WireFrame:
		var data FrameData
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("session: decoding %s: %w", t, err)
		}
		if data.Why == WhyToolsAdded {
			w.boundaries++
		}
		if data.Why == WhyCarried {
			w.through = max(w.through, data.Through)
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
		if w.awaiting == nil {
			before := w.WireState
			before.Told = slices.Clone(before.Told)
			w.awaiting = &before
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

// wireCarry is the frame to write at the front of a file whose events before it
// were dropped by retention: the frame those events declared. Without it a trimmed and reloaded conversation has
// no frame and opens a new one from the current prompt and tools, which the
// signed blocks that survive were not produced under. ok is false when the
// dropped events held no frame. The event takes the position of the last dropped
// one, so sequence numbers still only increase.
func wireCarry(dropped []Event, through int) (Event, bool, error) {
	var frame *FrameData
	for _, e := range dropped {
		if e.Type != WireFrame {
			continue
		}
		var data FrameData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return Event{}, false, fmt.Errorf("session: decoding %s: %w", e.Type, err)
		}
		frame = &data
	}
	if frame == nil {
		return Event{}, false, nil
	}
	frame.Why, frame.Added = WhyCarried, nil
	frame.Through = through
	raw, err := json.Marshal(frame)
	if err != nil {
		return Event{}, false, fmt.Errorf("session: encoding carried %s: %w", WireFrame, err)
	}
	last := dropped[len(dropped)-1]
	return Event{Seq: last.Seq, Type: WireFrame, At: last.At, Turn: last.Turn, Data: raw}, true, nil
}

// assumeTrimmedThrough covers a file whose trim left no record of its own: one
// written before the carried frame had a Through, or one with no frame at all (a
// log of a provider that binds nothing, later replayed to Anthropic). The
// header's trimmed_turns says retention ran, but not which events came before
// it, so every event the file held is treated as stale: reasoning is lost, which
// is the documented recovery, and a block is never sent behind a prefix that is
// gone. Nothing is appended: Load returns exactly the events the file holds.
//
// A trim is detected by either of two durable facts. The header's trimmed_turns
// counts whole turns, but a trim that drops only the events ahead of the first
// turn/start (a frame-less log's opening user message, say) counts none, and
// those events can be model-visible messages. The sequence numbers see it: a log
// numbers its events from 1 and never renumbers, so a file whose first event is
// later than 1 lost events from the front, whatever the header says.
func (l *Log) assumeTrimmedThrough(droppedTurns int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.wire.through != 0 || len(l.events) == 0 {
		return
	}
	if droppedTurns > 0 || l.events[0].Seq > 1 {
		l.wire.through = l.events[len(l.events)-1].Seq
	}
}

// Scrubbed is text as the log would hold it: what a payload string becomes on
// its way in. A value the Loop holds in the clear (the system prompt) is
// compared with what the log recorded of an earlier one through this, so a log
// that scrubs does not look changed on every turn.
func (l *Log) Scrubbed(text string) string {
	l.mu.RLock()
	scrub := l.scrub
	l.mu.RUnlock()
	if scrub == nil {
		return text
	}
	return scrub(text)
}

// Wire returns what the conversation has told its provider so far.
func (l *Log) Wire() WireState {
	if w, ok := l.strippedWire(); ok {
		return w
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return cloneWire(l.wire.WireState)
}

// strippedWire is the wire state of a log that has been edited, read the way
// the request is: a notice whose only answer is a reply StripStale empties is
// not delivered, because StripStale drops it from the request. The same
// staleSpans decides which replies those are, so the state and the request
// cannot disagree. The replay costs a pass over the log, which StripStale
// already pays on an edited log, and is kept until the next event. ok is false
// for a log that was never edited, whose incremental state is already right.
func (l *Log) strippedWire() (WireState, bool) {
	edited, floor := false, 0
	if err := l.withProjection(func(p *projection) {
		floor = l.wire.through
		edited = len(p.compacted)+len(p.bySeq) > 0 || l.wire.boundaries > 0 || floor > 0
	}); err != nil || !edited {
		return WireState{}, false
	}
	l.mu.RLock()
	if l.strippedAt == l.seq && l.stripped != nil {
		out := cloneWire(*l.stripped)
		l.mu.RUnlock()
		return out, true
	}
	events, seq := append([]Event(nil), l.events...), l.seq
	l.mu.RUnlock()

	sp, err := staleSpans(events, floor)
	if err != nil {
		return WireState{}, false
	}
	w := wireState{stale: make(map[int]bool, len(sp.seqs))}
	for i, seqNo := range sp.seqs {
		if sp.stale[i] {
			w.stale[seqNo] = true
		}
	}
	for _, e := range events {
		if err := w.fold(e.Seq, e.Type, e.Data); err != nil {
			return WireState{}, false
		}
	}
	l.mu.Lock()
	state := w.WireState
	l.stripped, l.strippedAt = &state, seq
	l.mu.Unlock()
	return cloneWire(w.WireState), true
}

// cloneWire copies the parts of a WireState a caller could otherwise alias.
func cloneWire(in WireState) WireState {
	out := in
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
