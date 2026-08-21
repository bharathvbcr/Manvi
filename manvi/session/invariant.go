package session

import (
	"encoding/json"
	"fmt"

	"manvi/llm"
)

// InvariantError reports a model request that is not reconstructable from the
// log. It names the first divergence rather than reporting a bare mismatch,
// because "the request does not match the log" is not actionable and "message 3
// is in the request but not in the log" is.
type InvariantError struct {
	MessageIndex int
	Detail       string
	// Injected is the offending message when the request has one the log does
	// not, which is the case that matters: content reached the model that the
	// evidence trail does not describe.
	Injected *llm.Message
}

func (e *InvariantError) Error() string {
	return fmt.Sprintf("session: model-visible-means-logged violated at message %d: %s",
		e.MessageIndex, e.Detail)
}

// AssertModelVisible checks that every message about to be sent is present in
// the log's projection, in order.
//
// The comparison is on model-visible content only. Provenance is deliberately
// excluded: PrepareHistory legitimately strips non-portable replay state on the
// way out, and that transformation is itself logged as provenance/dropped. What
// must never differ is what the model reads.
//
// Call this immediately before dispatching a request. The cost is one
// projection and a structural comparison; the thing it catches is a prompt
// section, a compaction summary, or a steering message that reached the model
// without leaving a record.
func AssertModelVisible(log *Log, request []llm.Message) error {
	derived, err := log.DeriveMessages()
	if err != nil {
		return fmt.Errorf("session: cannot verify invariant: %w", err)
	}

	// The request must be a subsequence of the projection, not an index-for-
	// index copy of it. PrepareHistory legitimately omits a message the target
	// cannot carry — an assistant turn whose only content was reasoning has
	// nothing left to send once reasoning is stripped, and sending it hollow is
	// a wire error — so the projection can hold messages the request does not.
	//
	// Advancing through the projection to find each sent message keeps the
	// check strict in the direction that matters: an omission is tolerated,
	// while content the model saw that nothing recorded still finds no match
	// and is reported. Comparing positionally instead would turn the correct
	// omission into a spurious violation on every turn that had to drop one.
	at := 0
	for i, sent := range request {
		matched := false
		var lastErr error
		for at < len(derived) {
			if err := sameVisibleContent(derived[at], sent); err == nil {
				at++
				matched = true
				break
			} else {
				lastErr = err
			}
			at++
		}
		if !matched {
			injected := sent
			detail := fmt.Sprintf("message %d (%s) was sent but no logged message matches it", i, sent.Role)
			if lastErr != nil {
				detail += ": " + lastErr.Error()
			}
			return &InvariantError{
				MessageIndex: i,
				Detail:       detail,
				Injected:     &injected,
			}
		}
	}

	// The log holding more than the request is normal: PrepareHistory may drop
	// a message a target provider cannot carry, and compaction removes history
	// deliberately. Only the reverse — content the model saw that nothing
	// recorded — is a violation.
	return nil
}

// sameVisibleContent compares what a model would read, ignoring provenance.
func sameVisibleContent(logged, sent llm.Message) error {
	if logged.Role != sent.Role {
		return fmt.Errorf("role differs: logged %q, sent %q", logged.Role, sent.Role)
	}

	loggedVisible := visibleBlocks(logged)
	sentVisible := visibleBlocks(sent)

	if len(loggedVisible) != len(sentVisible) {
		return fmt.Errorf("logged %d visible block(s), sent %d",
			len(loggedVisible), len(sentVisible))
	}
	for i := range sentVisible {
		if loggedVisible[i] != sentVisible[i] {
			// No exemption for compaction. Compaction is a logged event that
			// the projection applies, so a compacted result derives compacted
			// on both sides of this comparison. A difference here means
			// something rewrote history on its way to the model without
			// recording it, which is exactly what this check exists to catch.
			return fmt.Errorf("block %d differs: logged %s, sent %s",
				i, truncate(loggedVisible[i]), truncate(sentVisible[i]))
		}
	}
	return nil
}

// visibleBlocks renders a message's content into comparable strings.
//
// Reasoning blocks are excluded on purpose. PrepareHistory strips them when
// handing history to a provider that cannot replay them, and that removal is
// recorded separately — treating it as an invariant violation would make the
// correct behaviour fail the check.
func visibleBlocks(msg llm.Message) []string {
	var out []string
	for _, block := range msg.Content {
		switch b := block.(type) {
		case llm.TextBlock:
			out = append(out, "text:"+b.Text)
		case llm.ReasoningBlock:
			// see doc comment
		case llm.ImageBlock:
			out = append(out, fmt.Sprintf("image:%s:%d", b.MediaType, len(b.Data)))
		case llm.ToolCallBlock:
			out = append(out, fmt.Sprintf("tool_call:%s:%s:%s", b.ID, b.Name, canonical(b.Arguments)))
		case llm.ToolResultBlock:
			text := ""
			for _, inner := range b.Content {
				if t, ok := inner.(llm.TextBlock); ok {
					text += t.Text
				}
			}
			out = append(out, fmt.Sprintf("tool_result:%s:%v:%s", b.ToolCallID, b.IsError, text))
		default:
			out = append(out, fmt.Sprintf("unknown:%s", block.Kind()))
		}
	}
	return out
}

// canonical re-encodes JSON so key order cannot make identical arguments
// compare as different.
func canonical(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return string(raw)
	}
	return string(encoded)
}

func truncate(s string) string {
	const limit = 80
	if len(s) <= limit {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q…", s[:limit])
}
