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
// Which blocks that is has an exact answer, and it is narrower than "everything
// after the edit". A block is stale when it comes after the edited result and
// was recorded before the compaction was. A block before the result was signed
// against a prefix that never contained it, and a block recorded after the
// compaction was signed against the shortened history, because the request that
// produced it was built from this same projection. Both stay valid, so stripping
// them would throw away reasoning for nothing.

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
		edited = len(p.compacted)+len(p.bySeq) > 0
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
		pendingAt  int
	)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		for _, seq := range pending {
			resultAt[seq] = pendingAt
		}
		messageSeq = append(messageSeq, pending[len(pending)-1])
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
			messageSeq = append(messageSeq, event.Seq)
		case ToolResult:
			var data ToolResultData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return nil, fmt.Errorf("session: event %d: %w", event.Seq, err)
			}
			if len(pending) == 0 {
				pendingAt = len(messageSeq)
			}
			pending = append(pending, event.Seq)
			resultsBy[data.ToolCallID] = append(resultsBy[data.ToolCallID], event.Seq)
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
		for i := first + 1; i < len(messageSeq); i++ {
			if messageSeq[i] < edit.seq {
				stale[i] = true
			}
		}
	}
	return stale, nil
}
