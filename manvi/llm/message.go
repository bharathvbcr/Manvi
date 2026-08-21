// Package llm is the provider seam's definition: the provider-neutral message
// vocabulary every adapter maps to and from, and the one mechanism that keeps
// that neutrality from destroying provider-private state.
//
// The hard problem in a multi-provider harness is not writing two HTTP clients.
// It is that provider-private state has to survive a round trip through a
// neutral log. Anthropic requires thinking blocks to be echoed back unchanged
// on the same model; other providers drop them or reject them. A naive neutral
// log silently discards that state and you get quality loss that is very hard
// to trace, or a 400.
//
// The fix is AssistantProvenance.ReplayState: opaque adapter-private bytes,
// handed to an adapter only when it owns both the provider that produced the
// history and the provider about to receive it. Otherwise it is dropped — and
// the drop is *reported*, so cross-provider handoff is explicitly lossy rather
// than accidentally lossy.
package llm

import (
	"encoding/json"
	"fmt"
)

// BlockKind identifies a content block's type on the wire.
type BlockKind string

const (
	KindText       BlockKind = "text"
	KindReasoning  BlockKind = "reasoning"
	KindImage      BlockKind = "image"
	KindToolCall   BlockKind = "tool_call"
	KindToolResult BlockKind = "tool_result"
)

// ContentBlock is one piece of a message. Every adapter maps to and from these.
type ContentBlock interface {
	Kind() BlockKind
}

// TextBlock is visible assistant or user text.
type TextBlock struct {
	Text string `json:"text"`
}

func (TextBlock) Kind() BlockKind { return KindText }

// ReasoningBlock is model thinking, distinct from visible text.
//
// Signature carries whatever the producing provider needs in order to accept
// the block back. It is opaque here on purpose: the neutral layer must not
// interpret it, only carry it or drop it.
type ReasoningBlock struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
	// Redacted marks reasoning the provider returned in encrypted form.
	Redacted bool `json:"redacted,omitempty"`
}

func (ReasoningBlock) Kind() BlockKind { return KindReasoning }

// ImageBlock is image input.
type ImageBlock struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
}

func (ImageBlock) Kind() BlockKind { return KindImage }

// CallID identifies a tool call so its result can be paired with it.
type CallID string

// ToolCallBlock is the model asking for a tool.
type ToolCallBlock struct {
	ID        CallID          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (ToolCallBlock) Kind() BlockKind { return KindToolCall }

// ToolResultBlock is the harness answering one.
type ToolResultBlock struct {
	ToolCallID CallID         `json:"tool_call_id"`
	Content    []ContentBlock `json:"content"`
	IsError    bool           `json:"is_error,omitempty"`
}

func (ToolResultBlock) Kind() BlockKind { return KindToolResult }

// Role is who a message is from.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// AssistantProvenance records which provider produced an assistant message and
// carries that adapter's lossless replay state.
type AssistantProvenance struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// ReplayState is opaque adapter-private data needed to replay the response
	// losslessly. Handed to an adapter ONLY when it owns both the historical and
	// the target provider. Otherwise: dropped, and the drop is reported.
	ReplayState json.RawMessage `json:"replay_state,omitempty"`
}

// Message is one turn of conversation.
type Message struct {
	Role       Role                 `json:"role"`
	Content    []ContentBlock       `json:"content"`
	Provenance *AssistantProvenance `json:"provenance,omitempty"`
}

// Text returns the concatenated visible text of a message, ignoring reasoning
// and tool blocks.
func (m Message) Text() string {
	out := ""
	for _, block := range m.Content {
		if t, ok := block.(TextBlock); ok {
			out += t.Text
		}
	}
	return out
}

// ToolCalls returns the tool calls in a message.
func (m Message) ToolCalls() []ToolCallBlock {
	var out []ToolCallBlock
	for _, block := range m.Content {
		if call, ok := block.(ToolCallBlock); ok {
			out = append(out, call)
		}
	}
	return out
}

// Drop records provider-private state that was removed from history because the
// target adapter does not own it.
type Drop struct {
	MessageIndex int    `json:"message_index"`
	FromProvider string `json:"from_provider"`
	FromModel    string `json:"from_model"`
	ToProvider   string `json:"to_provider"`
	// ToModel is the model the history was being prepared for.
	//
	// Carried so a reader can tell the two reasons a drop happens apart. A
	// drop where the from and to pair are identical is not a handoff at all:
	// it is a provider that cannot replay its own reasoning, which for a
	// stateless adapter is permanent and expected and happens on every step of
	// every turn. A drop across a differing pair is a real crossing, and is the
	// one an operator can do something about. Reporting both as "lost state in
	// handoff", and marking both degraded, made the degraded signal fire on
	// 100% of steps against a reasoning model — which is how a signal that is
	// supposed to mean "look at this" comes to mean nothing.
	ToModel string `json:"to_model,omitempty"`
	// Blocks names the block kinds removed, e.g. "reasoning".
	Blocks []string `json:"blocks,omitempty"`
	// ReplayState is true when opaque adapter state was discarded.
	ReplayState bool `json:"replay_state"`
	// Emptied reports that removing the non-portable blocks left the message
	// with no content at all, so the message itself was omitted from the
	// history rather than sent as a hollow one no wire accepts.
	Emptied bool `json:"emptied,omitempty"`
}

// SameTarget reports whether this drop happened while preparing history for
// the very model that produced it. Nothing crossed; the adapter simply has no
// documented way to replay its own model-internal state.
func (d Drop) SameTarget() bool {
	// An unnamed target is not evidence of sameness. A Drop whose provenance
	// nobody recorded has to fall to the louder reading — reporting a real
	// crossing as a routine property of one model is the failure that matters
	// here, and the reverse is only noise.
	if d.ToProvider == "" || d.ToModel == "" || d.FromProvider == "" || d.FromModel == "" {
		return false
	}
	return d.FromProvider == d.ToProvider && d.FromModel == d.ToModel
}

func (d Drop) String() string {
	if d.SameTarget() {
		return fmt.Sprintf("message %d: %s/%s cannot replay its own %v",
			d.MessageIndex, d.FromProvider, d.FromModel, d.Blocks)
	}
	if d.Emptied {
		return fmt.Sprintf("message %d: %s/%s left no content portable to %s; the message was omitted (blocks=%v replay_state=%v)",
			d.MessageIndex, d.FromProvider, d.FromModel, d.ToProvider, d.Blocks, d.ReplayState)
	}
	return fmt.Sprintf("message %d: %s/%s state not portable to %s (blocks=%v replay_state=%v)",
		d.MessageIndex, d.FromProvider, d.FromModel, d.ToProvider, d.Blocks, d.ReplayState)
}

// ReasoningReplayer is implemented by an adapter that can say whether reasoning
// one of its models produced may be sent back on a later request.
//
// It exists because "same provider and same model" is not the same question as
// "replayable". A hosted API that signs its thinking blocks can replay them; an
// OpenAI-compatible wire has no mechanism to carry them at all, so replaying
// reasoning to the very model that produced it is still wrong — and the wire
// adapter rejects such a message outright, which turns a successful step into a
// failed turn on the step after it.
//
// An adapter that does not implement this is treated as replay-on-same-model,
// which is the behaviour that predates the interface.
type ReasoningReplayer interface {
	ReplayableOn(fromModel, toModel string) bool
}

// PrepareHistory adapts a message history for a target provider and model.
//
// An assistant message keeps its ReplayState and its reasoning blocks only when
// the target adapter owns the provider that produced it, and only when that
// adapter says the reasoning is replayable. Same provider but a different model
// still drops reasoning: providers tie thinking state to a specific model, and
// replaying it onto a sibling is the failure this function exists to prevent.
//
// Returns the adapted history and every drop, so the caller can log what was
// lost rather than discovering it as a quality regression.
func PrepareHistory(messages []Message, targetProvider, targetModel string) ([]Message, []Drop) {
	return prepareHistory(messages, targetProvider, targetModel, nil)
}

// PrepareHistoryFor is PrepareHistory with the target adapter available, so an
// adapter that implements ReasoningReplayer is asked rather than assumed.
func PrepareHistoryFor(messages []Message, target Provider, targetModel string) ([]Message, []Drop) {
	if target == nil {
		// A nil provider is a programming error, not a history transformation.
		// Returning nil here silently discarded every message — the turn then
		// went out with no context at all and nothing said why. Hand the
		// messages back untouched and let the caller fail on the nil provider
		// it actually has.
		return messages, nil
	}
	replayer, _ := target.(ReasoningReplayer)
	return prepareHistory(messages, target.Name(), targetModel, replayer)
}

func prepareHistory(messages []Message, targetProvider, targetModel string, replayer ReasoningReplayer) ([]Message, []Drop) {
	out := make([]Message, 0, len(messages))
	var drops []Drop

	for i, msg := range messages {
		if msg.Role != RoleAssistant || msg.Provenance == nil {
			out = append(out, msg)
			continue
		}

		sameProvider := msg.Provenance.Provider == targetProvider
		sameModel := sameProvider && msg.Provenance.Model == targetModel
		replayable := sameModel
		if sameProvider && replayer != nil {
			replayable = replayer.ReplayableOn(msg.Provenance.Model, targetModel)
		}
		if replayable {
			out = append(out, msg)
			continue
		}

		drop := Drop{
			MessageIndex: i,
			FromProvider: msg.Provenance.Provider,
			FromModel:    msg.Provenance.Model,
			ToProvider:   targetProvider,
			ToModel:      targetModel,
		}

		// Reasoning is only replayable on the exact model that produced it.
		filtered := make([]ContentBlock, 0, len(msg.Content))
		for _, block := range msg.Content {
			if block.Kind() == KindReasoning {
				drop.Blocks = append(drop.Blocks, string(KindReasoning))
				continue
			}
			filtered = append(filtered, block)
		}
		if len(msg.Provenance.ReplayState) > 0 {
			drop.ReplayState = true
		}

		// Stripping reasoning can empty the message, and an assistant message
		// with no content is not something any wire can carry: the
		// OpenAI-compatible encoder refuses it and the turn dies with "message
		// has no sendable content".
		//
		// It is reachable, not theoretical. A local server that is cut off
		// inside a <think> block yields a message whose only block is
		// reasoning, and llm/local reports reasoning as never replayable — so
		// the step that ran out of room poisons every later request in the
		// session, including after a --resume, because the message is in the
		// log for good.
		//
		// Such a message carries nothing the target can read, so it is dropped
		// rather than sent hollow. session.AssertModelVisible allows for this:
		// it aligns the request against the projection as a subsequence, so an
		// omission is fine and only content the model saw that nothing recorded
		// is a violation.
		if len(filtered) == 0 {
			drop.Emptied = true
			drops = append(drops, drop)
			continue
		}

		kept := msg
		kept.Content = filtered
		// The provenance itself is preserved so the log still records who
		// produced the message; only the non-portable payload goes.
		kept.Provenance = &AssistantProvenance{
			Provider: msg.Provenance.Provider,
			Model:    msg.Provenance.Model,
		}
		out = append(out, kept)

		if len(drop.Blocks) > 0 || drop.ReplayState {
			drops = append(drops, drop)
		}
	}
	return out, drops
}
