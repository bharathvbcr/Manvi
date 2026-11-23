package llm

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CallFraming says how a wire marks one tool call as finished. It is a fact
// about the provider's protocol, not a preference, and it is the only way the
// three adapters' settlement is allowed to differ.
type CallFraming int

const (
	// FramingRequired wires close every call with an event of its own
	// (anthropic's content_block_stop). Arguments that parse are still a prefix
	// until that event arrives: truncated JSON that happens to be whole, or an
	// unsealed {}, is how a half-received call becomes a runnable one.
	FramingRequired CallFraming = iota
	// FramingOptional wires usually close a call (gemini's step.stop) but a
	// whole JSON object is accepted without it. A call with no argument bytes
	// that was never closed is not: it is a call whose arguments did not arrive.
	FramingOptional
	// FramingNone wires never say a call is finished (the OpenAI-compatible
	// chunk stream). The stream's own terminal frame is the only framing, and
	// the caller has already insisted on it before settling anything.
	FramingNone
)

// CallVerdict is what SettleCall decides about one tool call.
type CallVerdict int

const (
	// CallComplete: the call may be dispatched, with CallSettlement.Arguments.
	CallComplete CallVerdict = iota
	// CallCutByLimit: the model's output limit ended the call. The turn
	// survives; the call is reported to the model as a MalformedCall.
	CallCutByLimit
	// CallUnusable: nothing the model did explains it. The call cannot be
	// dispatched and cannot be put back to the model as its own mistake.
	// Hosted wires refuse the response; a local server, which truncates far
	// more readily, reports it as a MalformedCall. Either way it never runs.
	CallUnusable
)

// CallEvidence is everything a decoder knows about one call when its stream
// ends.
type CallEvidence struct {
	ID   CallID
	Name string
	// Arguments is the accumulated argument bytes exactly as they arrived.
	Arguments string
	// Closed is true when the wire framed this call as finished.
	Closed  bool
	Framing CallFraming
	// IDOptional is true for a wire that correlates a result with its call
	// without an id (gemini sends the function name on every result, and
	// call_id is optional there), so a call that arrived with none is still
	// answerable. Every other wire needs the id, or the tool_result that
	// answers the call references nothing.
	IDOptional bool
	// Stop is the settled stop reason of the whole response.
	Stop StopReason
}

// CallSettlement is the verdict and what to do with it.
type CallSettlement struct {
	Verdict CallVerdict
	// Arguments is a complete JSON object; set only when Verdict is
	// CallComplete.
	Arguments json.RawMessage
	// Reason says why a call was not complete, in words fit for a MalformedCall
	// or an error. It never names a provider: the adapter adds that.
	Reason string
}

// SettleCall is the single rule for what a complete tool call is. A call is
// complete when it can be routed and answered (a name and an id), its
// arguments are a whole JSON object, and the wire's framing, where it has any,
// says the call is over. A call that falls short is cut by the output limit
// when the response ended on one, and unusable otherwise.
//
// The rule lived three times, once per adapter, and the copies had drifted: one
// let a bare JSON string through as arguments, one accepted a call that had no
// id, one dispatched a named call with no argument bytes as {} when the output
// limit had ended the stream. A fix to one of them was a fix to none of the
// others.
func SettleCall(e CallEvidence) CallSettlement {
	if strings.TrimSpace(e.Name) == "" {
		return unusable("arrived with no function name; it cannot be routed, and running a tool chosen some other way would be inventing the call")
	}
	if strings.TrimSpace(string(e.ID)) == "" && !e.IDOptional {
		return unusable("arrived with no id; the result answering it would reference nothing and the next request would be rejected")
	}
	limit := e.Stop == StopMaxTokens
	cut := CallSettlement{Verdict: CallCutByLimit, Reason: fmt.Sprintf(
		"the arguments were cut off by the output limit after %d bytes and are not a complete JSON object", len(e.Arguments))}

	args := strings.TrimSpace(e.Arguments)
	if args == "" {
		switch {
		case e.Closed:
			return complete("{}")
		case limit:
			return cut
		case e.Framing == FramingNone:
			return complete("{}")
		default:
			return unusable("was never closed by the wire's end-of-call frame and carried no arguments; it was not dispatched")
		}
	}
	if args[0] != '{' || !json.Valid([]byte(args)) {
		switch {
		case limit:
			return cut
		case json.Valid([]byte(args)):
			// Whole JSON that is not an object. A tool's arguments are an
			// object by definition, and a bare string reaching the tool reads as
			// the model having sent nonsense rather than as the adapter having
			// failed to unwrap the wire form.
			return unusable(fmt.Sprintf("arrived with arguments that are not a JSON object, so it was not dispatched: %q", args))
		default:
			// Truncation is the common cause and not the only one: two
			// concatenated objects are complete and still unusable.
			return unusable(fmt.Sprintf(
				"arrived with incomplete arguments, so they are not a JSON object and it was not dispatched — "+
					"the stream was cut off mid-call, or the server framed two calls as one: %q", args))
		}
	}
	if !e.Closed && e.Framing == FramingRequired {
		if limit {
			return cut
		}
		return unusable(fmt.Sprintf(
			"was never closed by the wire's end-of-call frame; its arguments may be incomplete and it was not dispatched (%d argument bytes)",
			len(e.Arguments)))
	}
	return complete(args)
}

func complete(args string) CallSettlement {
	return CallSettlement{Verdict: CallComplete, Arguments: json.RawMessage(args)}
}

func unusable(reason string) CallSettlement {
	return CallSettlement{Verdict: CallUnusable, Reason: reason}
}

// maxUnknownKinds bounds how many distinct unrecognised block or step types a
// DecodingReport names. The rest are counted, not listed.
const maxUnknownKinds = 16

// maxUnknownKindLen bounds one reported type name; it came off the wire.
const maxUnknownKindLen = 64

// NoteUnknown records a block or step type the decoder has no reading for. The
// content it carried is not in the settled message, and a message that is
// silently shorter than the stream looks complete; the report is how an
// operator finds out the adapter has fallen behind the wire. Names are kept
// sorted and distinct, and bounded: UnknownKindsMore counts what did not fit.
func (d *DecodingReport) NoteUnknown(kind string) {
	if len(kind) > maxUnknownKindLen {
		kind = kind[:maxUnknownKindLen]
	}
	if kind == "" {
		kind = "(empty)"
	}
	i := sort.SearchStrings(d.UnknownKinds, kind)
	if i < len(d.UnknownKinds) && d.UnknownKinds[i] == kind {
		return
	}
	if len(d.UnknownKinds) >= maxUnknownKinds {
		d.UnknownKindsMore++
		return
	}
	d.UnknownKinds = append(d.UnknownKinds, "")
	copy(d.UnknownKinds[i+1:], d.UnknownKinds[i:])
	d.UnknownKinds[i] = kind
}
