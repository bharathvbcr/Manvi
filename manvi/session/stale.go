package session

import (
	"encoding/json"
	"fmt"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// Signed reasoning that a client-side edit left behind.
//
// A provider that binds each thinking block to the conversation before it
// (llm.BindsThinkingToHistory) rejects a block once an earlier message has been
// edited. A ToolResultCompacted record is such an edit: the projection replays
// the shortened text in place of the result, so every thinking block produced
// while the full text was in the prefix now sits behind a prefix that no longer
// exists.
//
// The rule is: every signed block from the first edited position onward goes,
// including blocks recorded after the edit, and every block before it stays. A
// block before the edited result was signed against a prefix that never
// contained it, so it is valid. Blocks after it are not all stale (one recorded
// after the compaction was signed against whatever history its request carried)
// but each block records the one before it, and the documented removals that
// keep later blocks valid are only a leading run, oldest first
// (shared/model-migration.md, Breaking change 3). Removing the stale blocks and
// keeping later ones would be a removal from the middle, which invalidates what
// is kept. So the cut is a suffix: nothing signed follows a block that was
// removed.

// StripStaleReasoning removes the signed reasoning that the log's compactions
// invalidated from the assistant messages of msgs, and returns the history with
// how many blocks went. Text and tool calls stay: only thinking and
// redacted_thinking blocks are touched. The log itself is not changed, so the
// full record is still there, and nothing here depends on the model: a model
// switch is not an edit, and the provider drops what a different model cannot
// read.
//
// msgs must be the log's projection, as the loop hands it over. When it is not
// (a pre-step listener removed or reordered messages, so positions no longer
// line up with the log) the answer is not known, and the safe reading is that
// every signed block may be stale: they all go. That loses reasoning, which is
// the documented recovery for a history that was edited; sending a block the
// provider will reject loses the whole request.
//
// A log with no compaction in it returns msgs untouched and costs one read of
// the warm projection.
func (l *Log) StripStaleReasoning(msgs []llm.Message) ([]llm.Message, int, error) {
	edited := false
	if err := l.withProjection(func(p *projection) {
		edited = len(p.compacted)+len(p.bySeq) > 0 || l.wire.boundaries > 0
	}); err != nil {
		return nil, 0, err
	}
	if !edited {
		return msgs, 0, nil
	}
	stale, err := staleMessages(l.snapshot())
	if err != nil {
		return nil, 0, err
	}
	aligned := len(stale) == len(msgs)

	out := make([]llm.Message, len(msgs))
	total := 0
	for i, msg := range msgs {
		out[i] = msg
		if msg.Role != llm.RoleAssistant || (aligned && !stale[i]) {
			continue
		}
		stripped, n := llm.DropSignedReasoning(msg)
		out[i] = stripped
		total += n
	}
	return out, total, nil
}

// staleMessages reports, for each message of the projection of events, whether
// its signed reasoning predates an edit that sits before it. It rebuilds the
// projection's grouping (consecutive tool results are one user message, an
// assistant message with no content is not a message) so positions agree with
// DeriveMessages, and reads each compaction record the way the projection does:
// a record that names its result applies to that result, and one that names
// only a call id applies to every result with the id.
func staleMessages(events []Event) ([]bool, error) {
	type compaction struct {
		seq int
		ref CompactionData
	}
	var (
		messageSeq []int           // recording sequence of each projected message
		resultAt   = map[int]int{} // result event seq -> index of the message holding it
		resultsBy  = map[llm.CallID][]int{}
		edits      []compaction
		pending    []int // result seqs awaiting the user message that carries them
		leading    int   // messages before the latest tool-set boundary
		notices    int   // trailing messageSeq entries that are notices
	)
	// push records a projected message. A user-role message takes the
	// unanswered notices before it off, as appendMessage does.
	push := func(seq int, user bool) {
		if user {
			messageSeq = messageSeq[:len(messageSeq)-notices]
			notices = 0
		}
		messageSeq = append(messageSeq, seq)
	}
	flush := func() {
		if len(pending) == 0 {
			return
		}
		push(pending[len(pending)-1], true)
		for _, seq := range pending {
			resultAt[seq] = len(messageSeq) - 1
		}
		pending = nil
	}
	for _, event := range events {
		switch event.Type {
		case UserMessage, AssistantMessage:
			var data MessageData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return nil, fmt.Errorf("session: event %d: %w", event.Seq, err)
			}
			if event.Type == AssistantMessage && len(data.Message.Content) == 0 {
				continue
			}
			flush()
			push(event.Seq, event.Type == UserMessage)
			if event.Type == AssistantMessage {
				notices = 0
			}
		case ToolResult:
			var data ToolResultData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return nil, fmt.Errorf("session: event %d: %w", event.Seq, err)
			}
			pending = append(pending, event.Seq)
			resultsBy[data.ToolCallID] = append(resultsBy[data.ToolCallID], event.Seq)
		case WireNotice:
			// A message of its own in the projection, so it holds a position.
			flush()
			push(event.Seq, false)
			notices++
		case WireFrame:
			var data FrameData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return nil, fmt.Errorf("session: event %d: %w", event.Seq, err)
			}
			if data.Why == WhyToolsAdded {
				// Every message before the frame was signed against a tool set
				// that no longer exists.
				flush()
				leading = len(messageSeq)
			}
		case ToolResultCompacted:
			var data CompactionData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return nil, fmt.Errorf("session: event %d: %w", event.Seq, err)
			}
			edits = append(edits, compaction{seq: event.Seq, ref: data})
		}
	}
	flush()

	stale := make([]bool, len(messageSeq))
	for i := 0; i < leading && i < len(stale); i++ {
		stale[i] = true
	}
	for _, edit := range edits {
		var edited []int
		if edit.ref.ResultSeq > 0 {
			edited = []int{edit.ref.ResultSeq}
		} else {
			edited = resultsBy[edit.ref.ToolCallID]
		}
		// The earliest result the record changes is where the history first
		// differs from the one earlier blocks were signed against.
		first := -1
		for _, seq := range edited {
			if seq >= edit.seq {
				continue
			}
			if at, ok := resultAt[seq]; ok && (first < 0 || at < first) {
				first = at
			}
		}
		if first < 0 {
			continue
		}
		// Everything from the edited position on, recorded before the edit or
		// after it. See the rule at the top of this file.
		for i := first + 1; i < len(messageSeq); i++ {
			stale[i] = true
		}
	}
	return stale, nil
}
