// Package session is the append-only event log and the projection derived from
// it. It is the source of truth for what the model saw.
//
// One invariant carries the whole design:
//
//	MODEL-VISIBLE MEANS LOGGED.
//	Anything that reaches a model request must be reconstructable from the log.
//
// It is asserted at runtime, not documented as a convention, because the
// failure it prevents is invisible. A prompt section injected straight into a
// request without a corresponding event produces a perfectly good response and
// an evidence trail that quietly does not describe what happened. That is the
// difference between a harness you can audit and one you can only watch — and
// it is what lets a task's evidence chain be a projection of this log rather
// than a second thing to maintain.
package session

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"manvi/llm"
)

// Type is a session event's kind.
type Type string

const (
	TurnStart Type = "turn/start"
	TurnEnd   Type = "turn/end"
	StepStart Type = "step/start"
	StepEnd   Type = "step/end"

	UserMessage      Type = "user/message"
	SystemPrompt     Type = "system/prompt"
	AssistantChunk   Type = "assistant/chunk"
	AssistantMessage Type = "assistant/message"

	ToolCall   Type = "tool/call"
	ToolResult Type = "tool/result"

	// PolicyDenied and GrantApplied make the gate's decisions part of the same
	// durable record as the conversation, so an evidence report reads one log.
	PolicyDenied Type = "policy/denied"
	// PolicyQualified records a call the gate allowed but did not wave through
	// cleanly — a posture or mode demoted the rule that would have blocked it,
	// an executor widened its own scope, or a check could not run.
	//
	// It exists because the alternative was recording those as PolicyDenied.
	// devcouncil.annotate carries the would-have-blocked rule onto successful
	// results so the log can say why an allow was reached, and a reader keying
	// on the rule alone turned every such allow into a refusal: in yolo posture
	// nothing is refused by construction, yet each qualified write produced a
	// denial event for a write that plainly landed. A trail that cannot tell a
	// refusal from an allow is worse than one that says nothing.
	PolicyQualified Type = "policy/qualified"
	GrantApplied    Type = "grant/applied"
	// ProvenanceDropped records history the target provider could not carry.
	ProvenanceDropped Type = "provenance/dropped"
	// ToolResultCompacted records that a tool result was shortened to fit the
	// context budget. The projection applies it, so the compacted form is what
	// the log says the model saw — rather than the loop rewriting history on
	// its way out and the log describing a request that was never sent.
	//
	// It is durable and one-way for a reason beyond bookkeeping. A local
	// server's prefix cache is keyed on an unchanged token prefix, so
	// recomputing compaction every step moves the divergence point earlier
	// every step and the cache never warms. Recorded once, the prefix settles.
	ToolResultCompacted Type = "context/tool_result_compacted"
	// ContextOverflow records that history still exceeds the budget after
	// everything compactable was compacted. The turn continues — the server's
	// own refusal says more than a pre-emptive one — but a request that is
	// going to be truncated must not be indistinguishable from one that fits.
	ContextOverflow Type = "context/overflow"
	// MalformedToolCall records a tool call the adapter could not reconstruct.
	// It is not a policy denial and not a tool result: nothing ran, and the
	// evidence trail must not imply that something did.
	MalformedToolCall Type = "tool/malformed"
	// DecodingCompensated records that an adapter had to work around how a
	// server framed its response — recovering a tool call from plain text, or
	// correcting a prefilled reasoning tag. Both mean the server is not
	// configured for the model it serves, and both are invisible otherwise.
	DecodingCompensated Type = "llm/decoding_compensated"
	// NullResponseRetried records a response the server completed having
	// generated nothing — no text, no tool call, no output tokens — which the
	// loop asked for again rather than letting it end the turn.
	//
	// It is durable because it is the difference between "the model had nothing
	// to say" and "the provider produced nothing and we asked again", and a
	// turn that quietly re-requested its way past four of them is a turn whose
	// cost and latency need an explanation.
	NullResponseRetried Type = "llm/null_response_retried"
)

// Event is one durable record.
type Event struct {
	Seq  int             `json:"seq"`
	Type Type            `json:"type"`
	At   time.Time       `json:"at"`
	Turn int             `json:"turn"`
	Step int             `json:"step"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Payload shapes for the events the projection reads.
type (
	// SystemPromptData is the assembled system prompt for a step.
	SystemPromptData struct {
		Text string `json:"text"`
	}
	// NullResponseData records one empty completion and which attempt it was.
	NullResponseData struct {
		Step    int `json:"step"`
		Attempt int `json:"attempt"`
		Of      int `json:"of"`
	}
	// MessageData carries a whole message.
	MessageData struct {
		Message llm.Message `json:"message"`
	}
	// ToolCallData is a tool invocation as the model asked for it.
	ToolCallData struct {
		ID        llm.CallID      `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	// ToolResultData is the single model-facing outcome of a tool call.
	ToolResultData struct {
		ToolCallID llm.CallID `json:"tool_call_id"`
		Text       string     `json:"text"`
		IsError    bool       `json:"is_error,omitempty"`
	}
	// DenialData records a blocked tool call.
	DenialData struct {
		ToolCallID llm.CallID `json:"tool_call_id"`
		Tool       string     `json:"tool"`
		Rule       string     `json:"rule"`
		Severity   string     `json:"severity"`
		Reason     string     `json:"reason"`
	}
	// QualificationData records a call that was allowed, and what qualified it.
	//
	// Demoted, Widened and Degraded are the reason this is not DenialData with
	// a flag: DenialData has nowhere to put them, so a resumed session could
	// see the rule but never learn what let the call through.
	QualificationData struct {
		ToolCallID llm.CallID `json:"tool_call_id"`
		Tool       string     `json:"tool"`
		Rule       string     `json:"rule"`
		Severity   string     `json:"severity"`
		Reason     string     `json:"reason"`
		Demoted    string     `json:"demoted,omitempty"`
		Widened    string     `json:"widened,omitempty"`
		Degraded   []string   `json:"degraded,omitempty"`
	}
	// GrantData records an override that cleared a block.
	GrantData struct {
		GrantID   string `json:"grant_id"`
		GrantedBy string `json:"granted_by"`
		Rule      string `json:"rule"`
		Target    string `json:"target"`
		Reason    string `json:"reason"`
	}
	// DropData records non-portable provider state removed from history.
	DropData struct {
		Drops []llm.Drop `json:"drops"`
	}
	// CompactionData records one tool result shortened to fit the budget.
	//
	// The original is not deleted: the ToolResult event that carries it stays
	// in the log, and this event only changes what the projection replays. So
	// an evidence report can still show what the tool actually returned while
	// the model's history carries the short form.
	CompactionData struct {
		ToolCallID llm.CallID `json:"tool_call_id"`
		Text       string     `json:"text"`
		// FromBytes and ToBytes make the saving auditable without diffing the
		// two events.
		FromBytes int `json:"from_bytes"`
		ToBytes   int `json:"to_bytes"`
	}
	// MalformedCallData records an unreconstructable tool call.
	MalformedCallData struct {
		ToolCallID llm.CallID `json:"tool_call_id"`
		Tool       string     `json:"tool,omitempty"`
		Reason     string     `json:"reason"`
	}
	// DecodingData records adapter-level compensation for a server's framing.
	DecodingData struct {
		FallbackFormat        string `json:"fallback_format,omitempty"`
		ReasoningReclassified bool   `json:"reasoning_reclassified,omitempty"`
		PrefillDisproved      bool   `json:"prefill_disproved,omitempty"`
	}
	// OverflowData records history that could not be made to fit.
	OverflowData struct {
		EstimatedTokens int `json:"estimated_tokens"`
		Threshold       int `json:"threshold"`
		ContextWindow   int `json:"context_window"`
	}
)

// Log is an append-only event log.
type Log struct {
	mu     sync.RWMutex
	events []Event
	turn   int
	step   int
	// seq is the last sequence number handed out. It is held rather than
	// derived from len(events) because a restored log may begin partway
	// through a session — retention drops whole turns off the front — and a
	// sequence number that restarted at 1 would make two different events in
	// one session's history carry the same id.
	seq int
	// observers are notified after each append. They exist so a live face — a
	// terminal, an editor plugin — can render a turn as it happens without
	// keeping its own copy of the conversation.
	//
	// Rendering from the log rather than from a parallel stream is what keeps
	// the two from disagreeing: the log is already the only thing the model's
	// history is projected from, so a UI fed by it cannot show a turn that
	// differs from the one the model saw.
	observers []func(Event)
}

// NewLog returns an empty log.
func NewLog() *Log { return &Log{} }

// RestoreLog rebuilds a log from previously recorded events, so a later turn
// can be appended to the same history.
//
// It validates rather than trusts. A log that is accepted here becomes the
// thing DeriveMessages projects, so a malformed one would not fail at load —
// it would fail as a request that does not say what the harness thinks it
// says, or not fail at all and resume from a history missing whatever could
// not be read. Sequence numbers must be positive and strictly increasing, and
// every event must carry a type; anything else is refused by name.
//
// Observers are not notified for restored events. They are registered by a
// face that is about to render the *new* turn, and replaying a whole prior
// session into it would print history the caller did not ask for.
func RestoreLog(events []Event) (*Log, error) {
	l := &Log{}
	last := 0
	for i, event := range events {
		if event.Type == "" {
			return nil, fmt.Errorf("session: restored event %d has no type", i)
		}
		if event.Seq <= last {
			return nil, fmt.Errorf(
				"session: restored event %d has sequence %d, which does not follow %d",
				i, event.Seq, last)
		}
		last = event.Seq
		if event.Turn < l.turn {
			return nil, fmt.Errorf(
				"session: restored event %d (seq %d) belongs to turn %d, which precedes turn %d",
				i, event.Seq, event.Turn, l.turn)
		}
		l.turn = event.Turn
		l.step = event.Step
	}
	l.events = append([]Event(nil), events...)
	l.seq = last

	// The projection is exercised now rather than at the first request. A log
	// that cannot be projected is not a session that resumes with a shorter
	// history; it is a session that cannot be resumed, and the difference has
	// to be visible at the point the operator can still choose another one.
	if _, err := l.DeriveMessages(); err != nil {
		return nil, err
	}
	return l, nil
}

// Observe registers a callback invoked after every append.
//
// The callback runs outside the log's lock, and it must not call back into the
// log: an observer that appended would deadlock on a re-entrant write, and one
// that read would be reading a log that has not finished being written.
func (l *Log) Observe(fn func(Event)) {
	if fn == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.observers = append(l.observers, fn)
}

// Append records an event and returns it with its assigned sequence number.
func (l *Log) Append(t Type, payload any) (Event, error) {
	var raw json.RawMessage
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return Event{}, fmt.Errorf("session: encoding %s: %w", t, err)
		}
		raw = encoded
	}

	l.mu.Lock()
	switch t {
	case TurnStart:
		l.turn++
		l.step = 0
	case StepStart:
		l.step++
	}

	l.seq++
	event := Event{
		Seq:  l.seq,
		Type: t,
		At:   time.Now().UTC(),
		Turn: l.turn,
		Step: l.step,
		Data: raw,
	}
	l.events = append(l.events, event)
	observers := l.observers
	l.mu.Unlock()

	// Notified outside the lock, and only after the event is in the log. An
	// observer that blocked or panicked while the lock was held would take the
	// log — and with it the turn — down. The lock is not reacquired: there is
	// no shared state left to touch, and a deferred unlock here would mean
	// releasing a lock this function no longer holds.
	for _, fn := range observers {
		fn(event)
	}
	return event, nil
}

// Events returns a copy of the log.
func (l *Log) Events() []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]Event(nil), l.events...)
}

// Len is the number of recorded events.
func (l *Log) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.events)
}

// DeriveMessages projects the log into the message history a model request is
// built from. This is the *only* sanctioned way to build that history — which
// is what makes the invariant enforceable rather than aspirational.
//
// Tool results are folded into a user message following the assistant message
// that requested them, which is the shape every current provider expects.
// CompactedCalls returns the tool results this log records as already
// compacted, so a planner can leave them alone.
//
// The ledger has to live on the log rather than on the caller. A Loop is built
// per turn — cmd/manvi/tui.go builds a new one on every submission, and
// `run --resume` builds one against a log restored from disk — while the log
// spans the whole session. A ledger held on the Loop is therefore empty at the
// start of turn two, so results compacted during turn one arrive at the
// planner looking untouched and are compacted a second time.
//
// That is not merely wasted work. Compacting already-compacted text produces
// *different bytes*, so the prompt prefix diverges at the first tool result and
// the server's KV cache is invalidated from there on — the whole cost this
// design exists to avoid, paid again at every turn boundary. It also makes the
// elision notice lie: the second pass counts lines omitted from the already
// elided text, so a result that dropped 64 lines reports "[1 line(s) omitted]".
//
// Deriving it from the events rather than caching it keeps one source of truth
// and makes it survive a restore, which an in-memory map cannot.
func (l *Log) CompactedCalls() (map[llm.CallID]struct{}, error) {
	l.mu.RLock()
	events := append([]Event(nil), l.events...)
	l.mu.RUnlock()

	out := make(map[llm.CallID]struct{})
	for _, event := range events {
		if event.Type != ToolResultCompacted {
			continue
		}
		var data CompactionData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return nil, fmt.Errorf("session: event %d: %w", event.Seq, err)
		}
		out[data.ToolCallID] = struct{}{}
	}
	return out, nil
}

func (l *Log) DeriveMessages() ([]llm.Message, error) {
	l.mu.RLock()
	events := append([]Event(nil), l.events...)
	l.mu.RUnlock()

	// Compactions are collected first because a tool result is replayed in its
	// compacted form regardless of where the compaction event landed relative
	// to it. Applying them in one pass also means the projection is a pure
	// function of the log: two derivations of the same log are byte-identical,
	// which is what the prefix cache and the invariant assertion both rely on.
	compacted := make(map[llm.CallID]string)
	for _, event := range events {
		if event.Type != ToolResultCompacted {
			continue
		}
		var data CompactionData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return nil, fmt.Errorf("session: event %d: %w", event.Seq, err)
		}
		compacted[data.ToolCallID] = data.Text
	}

	var out []llm.Message
	var pendingResults []llm.ContentBlock

	flushResults := func() {
		if len(pendingResults) == 0 {
			return
		}
		out = append(out, llm.Message{Role: llm.RoleUser, Content: pendingResults})
		pendingResults = nil
	}

	for _, event := range events {
		switch event.Type {
		case UserMessage, AssistantMessage:
			var data MessageData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return nil, fmt.Errorf("session: event %d: %w", event.Seq, err)
			}
			// An assistant message with no content is recorded for its usage
			// but contributes nothing to history, matching the upstream rule.
			if event.Type == AssistantMessage && len(data.Message.Content) == 0 {
				continue
			}
			flushResults()
			out = append(out, data.Message)

		case ToolResult:
			var data ToolResultData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return nil, fmt.Errorf("session: event %d: %w", event.Seq, err)
			}
			text := data.Text
			if short, ok := compacted[data.ToolCallID]; ok {
				text = short
			}
			pendingResults = append(pendingResults, llm.ToolResultBlock{
				ToolCallID: data.ToolCallID,
				Content:    []llm.ContentBlock{llm.TextBlock{Text: text}},
				IsError:    data.IsError,
			})
		}
	}
	flushResults()
	return out, nil
}

// SystemPrompt returns the most recently logged system prompt.
func (l *Log) SystemPrompt() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for i := len(l.events) - 1; i >= 0; i-- {
		if l.events[i].Type != SystemPrompt {
			continue
		}
		var data SystemPromptData
		if json.Unmarshal(l.events[i].Data, &data) == nil {
			return data.Text
		}
	}
	return ""
}
