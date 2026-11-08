package session

import (
	"encoding/json"
	"fmt"
	"slices"

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
// keep the rest valid are a contiguous window of the thinking blocks: a run
// dropped from the front (oldest first), a run dropped from the back, or both
// (shared/preserved-thinking-migration.md, "Remove thinking only as a
// contiguous run"; shared/model-migration.md, Breaking change 3). Removing the
// stale blocks and keeping later ones would be a removal from the middle, which
// invalidates what is kept. So an edit's cut is a suffix, the run from the back:
// nothing signed follows a block that was removed.
//
// A retention trim is the other kind of edit, and its cut is a prefix. Store.Save
// drops whole turns off the front of a log, so every block kept in the file was
// signed behind messages the file no longer holds. The carried frame it writes
// at the front names the last event of the file (FrameData.Through), and every
// signed block recorded at or before it goes. They are the oldest blocks of the
// history, a run dropped from the front, which is a removal the docs name as
// keeping later blocks valid
// (shared/model-migration.md, Breaking change 3, "What keeps later blocks
// valid"; shared/preserved-thinking-migration/causes.md, rows rolling_truncation
// and tail_kept, which offer exactly this as "a deterministic, recorded strip").
// Blocks after Through were produced by the resumed conversation under the
// trimmed history, and Through is the end of everything it makes stale, so
// nothing signed follows a removed block there either. A tools_added frame is cut
// the same way and for the same reason.

// The reasons a DropData record gives for signed reasoning removed by
// StripStale.
const (
	DropReasonRetentionTrim = "retention_trim"
	DropReasonHistoryEdit   = "history_edit"
)

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
	out, drops, err := l.StripStale(msgs)
	if err != nil {
		return nil, 0, err
	}
	total := 0
	for _, d := range drops {
		total += d.Blocks
	}
	return out, total, nil
}

// StaleDrop is the signed reasoning one message lost: its position in the
// history StripStale was given, how many blocks went, and why.
type StaleDrop struct {
	MessageIndex int
	Blocks       int
	// Reason is DropReasonRetentionTrim when retention removed the turns the
	// block was signed behind, and DropReasonHistoryEdit for any other edit
	// (a compaction, a tool set that grew, a history that no longer lines up).
	Reason string
	// Suffix is true when the message is stale only because it follows a
	// compaction's first edit (the run from the back), false when it sits in
	// the run from the front (a tool-set boundary or a trim). RecordStale
	// dedupes the two shapes differently.
	Suffix bool
}

// StripStale is StripStaleReasoning with the drops itemised, so a caller can
// record what was removed and why in the same shape the log keeps for other
// reasoning drops (DropData).
func (l *Log) StripStale(msgs []llm.Message) ([]llm.Message, []StaleDrop, error) {
	edited, floor := false, 0
	if err := l.withProjection(func(p *projection) {
		floor = l.wire.through
		edited = len(p.compacted)+len(p.bySeq) > 0 || l.wire.boundaries > 0 || floor > 0
	}); err != nil {
		return nil, nil, err
	}
	if !edited {
		return msgs, nil, nil
	}
	spans, err := staleSpans(l.snapshot(), floor)
	if err != nil {
		return nil, nil, err
	}
	stale, trimmed := spans.stale, spans.trimmed
	aligned := len(stale) == len(msgs)

	out := make([]llm.Message, len(msgs))
	var drops []StaleDrop
	emptied := false
	for i, msg := range msgs {
		out[i] = msg
		if msg.Role != llm.RoleAssistant || (aligned && !stale[i]) {
			continue
		}
		stripped, n := llm.DropSignedReasoning(msg)
		out[i] = stripped
		if n == 0 {
			continue
		}
		emptied = emptied || emptiedByStrip(msg)
		reason := DropReasonHistoryEdit
		if aligned && i < trimmed {
			reason = DropReasonRetentionTrim
		}
		drops = append(drops, StaleDrop{MessageIndex: i, Blocks: n, Reason: reason, Suffix: aligned && i >= spans.prefix})
	}
	if emptied {
		out = withoutNoticesLeftUnanswered(out)
	}
	return out, drops, nil
}

// emptiedByStrip reports whether stripping msg's signed reasoning leaves nothing
// the encoder can send. It is the one rule StripStale (which drops the notice
// such a reply answered from the request) and the log's wire state (which then
// counts that notice as undelivered) both apply, so what the log says the model
// holds is what the request carried.
func emptiedByStrip(msg llm.Message) bool {
	stripped, n := llm.DropSignedReasoning(msg)
	return n > 0 && msg.HasSendableContent() && !stripped.HasSendableContent()
}

// withoutNoticesLeftUnanswered drops each notice that a user message follows
// once the assistant messages the strip emptied are set aside. The reply that
// answered it held only signed reasoning, which the strip removed, so the
// encoder omits it and the wire would carry user, system, user, a 400 on this
// request and on every later one (shared/prompt-caching.md, "Mid-conversation
// system messages").
//
// That removes a message every later block was signed behind (shared/
// preserved-thinking-migration.md: deleting a copy already sent is an edit),
// which is safe here only because of where it happens. A reply is emptied by
// the strip only inside the stale region, and every signed block after that
// region was produced by a request that already carried this same strip, so it
// was signed behind the sequence without the notice. The result is a function
// of the log alone, so it is the same on every request and after a resume.
func withoutNoticesLeftUnanswered(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for i, msg := range msgs {
		if msg.Role == llm.RoleSystem {
			next := i + 1
			for next < len(msgs) && msgs[next].Role == llm.RoleAssistant && !msgs[next].HasSendableContent() {
				next++
			}
			if next < len(msgs) && msgs[next].Role == llm.RoleUser {
				continue
			}
		}
		out = append(out, msg)
	}
	return out
}

// RecordStale logs the signed reasoning StripStale removed, one provenance
// record per reason, so the log and --json say that reasoning was removed and why
// (a retention trim, or another edit of the history). The same strip repeats on
// every request, and on every Loop that drives the conversation, until the
// history changes, so a reason whose latest record already names the same
// messages is not recorded again. The log is the memory: a Loop built for the
// next turn finds what the last one wrote.
func (l *Log) RecordStale(drops []StaleDrop, provider, model string) error {
	byReason := map[string][]llm.Drop{}
	var order []string
	suffix := map[int]bool{}
	for _, d := range drops {
		if d.Suffix {
			suffix[d.MessageIndex] = true
		}
		if _, seen := byReason[d.Reason]; !seen {
			order = append(order, d.Reason)
		}
		byReason[d.Reason] = append(byReason[d.Reason], llm.Drop{
			MessageIndex: d.MessageIndex,
			FromProvider: provider, FromModel: model, ToProvider: provider, ToModel: model,
			Blocks: []string{"reasoning"},
		})
	}
	if len(order) == 0 {
		return nil
	}
	events := l.snapshot()
	for _, reason := range order {
		recorded, err := latestDropIndexes(events, reason)
		if err != nil {
			return err
		}
		if alreadyRecorded(reason, recorded, dropIndexes(byReason[reason]), suffix) {
			continue
		}
		if _, err := l.Append(ProvenanceDropped, DropData{Drops: byReason[reason], Reason: reason}); err != nil {
			return err
		}
	}
	return nil
}

// alreadyRecorded reports whether the latest record for reason already says what
// current names. A retention trim is a fixed set of messages, so the record must
// match exactly.
//
// A history edit follows the shape of its cut. A compaction strips everything
// from its first edited position on (the run from the back), so each assistant
// turn after it adds one more index and a list that only extends the recorded
// one with such indexes is the same fact, "from here on": it is not recorded
// again, which keeps the log linear in the turns after a compaction rather than
// quadratic. A tool-set boundary strips the run from the front, so a second one
// strips messages the first left alone: an index that sits in the front run is a
// change however late in the list it falls, and is recorded. A boundary that
// moves earlier, or a message that joins before the recorded end, differs from
// the record and is written. suffix names the indexes that are in the back run.
func alreadyRecorded(reason string, recorded, current []int, suffix map[int]bool) bool {
	if reason != DropReasonHistoryEdit || len(recorded) == 0 {
		return slices.Equal(recorded, current)
	}
	if len(current) < len(recorded) || !slices.Equal(recorded, current[:len(recorded)]) {
		return false
	}
	for _, i := range current[len(recorded):] {
		if !suffix[i] {
			return false
		}
	}
	return true
}

func dropIndexes(drops []llm.Drop) []int {
	out := make([]int, len(drops))
	for i, d := range drops {
		out[i] = d.MessageIndex
	}
	return out
}

// latestDropIndexes is the message positions of the newest provenance/dropped
// record that names reason, nil when there is none.
func latestDropIndexes(events []Event, reason string) ([]int, error) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != ProvenanceDropped {
			continue
		}
		var data DropData
		if err := json.Unmarshal(events[i].Data, &data); err != nil {
			return nil, fmt.Errorf("session: event %d: %w", events[i].Seq, err)
		}
		if data.Reason == reason {
			return dropIndexes(data.Drops), nil
		}
	}
	return nil, nil
}

// staleMessages reports, for each message of the projection of events, whether
// its signed reasoning predates an edit that sits before it. It rebuilds the
// projection's grouping (consecutive tool results are one user message, an
// assistant message with no content is not a message) so positions agree with
// DeriveMessages, and reads each compaction record the way the projection does:
// a record that names its result applies to that result, and one that names
// only a call id applies to every result with the id.
func staleMessages(events []Event) ([]bool, error) {
	spans, err := staleSpans(events, 0)
	return spans.stale, err
}

// spans is what staleSpans knows about the projection of a log: which messages
// are stale, the sequence number each was recorded at, how many leading messages
// are stale because of a tool-set boundary or a trim (prefix), and how many of
// those sit before the latest retention trim (trimmed).
type spans struct {
	stale   []bool
	seqs    []int
	prefix  int
	trimmed int
}

// staleSpans is staleMessages plus the shape of the cut: where the stale prefix
// ends, and how much of it is stale because of a retention trim rather than any
// other edit.
func staleSpans(events []Event, floor int) (spans, error) {
	type compaction struct {
		seq int
		ref CompactionData
	}
	// projected is one message of the projection: the sequence it was recorded
	// at, and whether it is a notice or an assistant message with nothing the
	// encoder can send, the two kinds a user message's arrival acts on.
	type projected struct {
		seq            int
		notice, hollow bool
	}
	var (
		messages  []projected     // each projected message, in order
		resultAt  = map[int]int{} // result event seq -> index of the message holding it
		resultsBy = map[llm.CallID][]int{}
		edits     []compaction
		pending   []int   // result seqs awaiting the user message that carries them
		leading   int     // messages before the latest tool-set or trim boundary
		through   = floor // latest event a retention trim made stale
	)
	// push records a projected message. A user-role message takes the
	// unanswered notices before it off, by the rule appendMessage applies.
	push := func(m projected, user bool) {
		if user {
			messages = withoutUnansweredNotices(messages,
				func(p projected) bool { return p.notice },
				func(p projected) bool { return p.hollow })
		}
		messages = append(messages, m)
	}
	flush := func() {
		if len(pending) == 0 {
			return
		}
		push(projected{seq: pending[len(pending)-1]}, true)
		for _, seq := range pending {
			resultAt[seq] = len(messages) - 1
		}
		pending = nil
	}
	for _, event := range events {
		switch event.Type {
		case UserMessage, AssistantMessage:
			var data MessageData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return spans{}, fmt.Errorf("session: event %d: %w", event.Seq, err)
			}
			if event.Type == AssistantMessage && len(data.Message.Content) == 0 {
				continue
			}
			flush()
			push(projected{seq: event.Seq, hollow: event.Type == AssistantMessage && !data.Message.HasSendableContent()},
				event.Type == UserMessage)
		case ToolResult:
			var data ToolResultData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return spans{}, fmt.Errorf("session: event %d: %w", event.Seq, err)
			}
			pending = append(pending, event.Seq)
			resultsBy[data.ToolCallID] = append(resultsBy[data.ToolCallID], event.Seq)
		case WireNotice:
			// A message of its own in the projection, so it holds a position.
			flush()
			push(projected{seq: event.Seq, notice: true}, false)
		case WireFrame:
			var data FrameData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return spans{}, fmt.Errorf("session: event %d: %w", event.Seq, err)
			}
			if data.Why == WhyToolsAdded {
				// Every message before the frame was signed against a tool set
				// that no longer exists.
				flush()
				leading = len(messages)
			}
			through = max(through, data.Through)
		case ToolResultCompacted:
			var data CompactionData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return spans{}, fmt.Errorf("session: event %d: %w", event.Seq, err)
			}
			edits = append(edits, compaction{seq: event.Seq, ref: data})
		}
	}
	flush()

	// Messages recorded at or before the trim's Through were signed behind turns
	// retention removed. The recording sequence only increases, so they are a
	// leading run of the projection.
	trimmed := 0
	for trimmed < len(messages) && messages[trimmed].seq <= through {
		trimmed++
	}
	leading = max(leading, trimmed)
	stale := make([]bool, len(messages))
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
		for i := first + 1; i < len(messages); i++ {
			stale[i] = true
		}
	}
	seqs := make([]int, len(messages))
	for i, m := range messages {
		seqs[i] = m.seq
	}
	return spans{stale: stale, seqs: seqs, prefix: leading, trimmed: trimmed}, nil
}
