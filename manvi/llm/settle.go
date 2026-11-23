package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
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
	// KeepTurn is set on a CallUnusable call that the output limit ended the
	// response around: the turn is already cut short, so it survives and the
	// call is reported like a cut one, but the reason is not "cut off" because
	// the bytes are all there. See Reportable.
	KeepTurn bool
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
		case e.Framing != FramingNone:
			return unusable("was never closed by the wire's end-of-call frame and carried no arguments; it was not dispatched")
		case e.Stop == StopEndTurn || e.Stop == StopToolUse:
			// A zero-argument call on a wire that frames nothing: the stream
			// ended the way a stream that carried a call ends.
			return complete("{}")
		default:
			// A content filter, a refusal, or no stop reason at all: nothing says
			// the arguments are really absent rather than withheld.
			return unusable(fmt.Sprintf(
				"arrived with no arguments and the response ended on stop reason %q rather than a call; it was not dispatched", e.Stop))
		}
	}
	whole := json.Valid([]byte(args))
	if whole && args[0] != '{' {
		// Whole JSON that is not an object: all the bytes are there, so this is
		// never a cut. A tool's arguments are an object by definition, and a
		// bare string reaching the tool reads as the model having sent nonsense
		// rather than as the adapter having failed to unwrap the wire form.
		u := unusable(fmt.Sprintf("arrived with arguments that are not a JSON object, so it was not dispatched: %s", echoArgs(args)))
		u.KeepTurn = limit
		return u
	}
	if !whole {
		if limit {
			return cut
		}
		// Truncation is the common cause and not the only one: two
		// concatenated objects are complete and still unusable.
		return unusable(fmt.Sprintf(
			"arrived with incomplete arguments, so they are not a JSON object and it was not dispatched — "+
				"the stream was cut off mid-call, or the server framed two calls as one: %s", echoArgs(args)))
	}
	if !e.Closed && e.Framing == FramingRequired {
		if limit {
			return CallSettlement{Verdict: CallCutByLimit, Reason: fmt.Sprintf(
				"the output limit ended the response before the wire closed this call (%d argument bytes arrived); it was not dispatched",
				len(e.Arguments))}
		}
		return unusable(fmt.Sprintf(
			"was never closed by the wire's end-of-call frame; its arguments may be incomplete and it was not dispatched (%d argument bytes)",
			len(e.Arguments)))
	}
	return complete(args)
}

// echoArgs quotes a bounded prefix of wire arguments for a reason that reaches
// the model and the logs.
func echoArgs(args string) string {
	return fmt.Sprintf("%q", TruncateForMessage(args))
}

// messageLimit bounds a fragment of wire text quoted back to the model.
const messageLimit = 120

// TruncateForMessage bounds a fragment of untrusted wire text quoted back to
// the model or a log.
//
// The cut is moved back to a rune boundary. Slicing at a byte offset split
// multi-byte UTF-8 down the middle and put the orphaned continuation bytes into
// MalformedCall.Reason, which is both quoted back to the model and written to
// the session log: the record was corrupted quietly, because json.Marshal
// substitutes U+FFFD rather than refusing.
func TruncateForMessage(s string) string {
	if len(s) <= messageLimit {
		return s
	}
	cut := messageLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// RefuseDuplicateIDs is the response-level half of the rule. Two calls in one
// response that would both dispatch under the same non-empty id leave the
// results answering them ambiguous, and the next request is rejected; neither
// dispatches, and both become CallUnusable so both are reported. ids and
// settled are parallel, one entry per call in stream order. Calls that were
// not going to dispatch anyway (cut, unusable) do not count: a whole call and
// a cut call may legitimately share an id.
func RefuseDuplicateIDs(ids []CallID, settled []CallSettlement) {
	count := make(map[CallID]int, len(ids))
	for i, id := range ids {
		if id != "" && settled[i].Verdict == CallComplete {
			count[id]++
		}
	}
	for i, id := range ids {
		if id != "" && settled[i].Verdict == CallComplete && count[id] > 1 {
			settled[i] = unusable(fmt.Sprintf(
				"shares id %q with another call in the same response, so the results answering them would be ambiguous; neither was dispatched",
				TruncateForMessage(string(id))))
		}
	}
}

// Reportable is true for a call that must not dispatch but does not end the
// turn: it is told to the model as a MalformedCall. Every other CallUnusable
// call is a refusal.
func (c CallSettlement) Reportable() bool {
	return c.Verdict == CallCutByLimit || (c.Verdict == CallUnusable && c.KeepTurn)
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

// maxUnknownOverflowTracked bounds the memory spent telling distinct overflow
// names from repeats. Past it the count may include repeats.
const maxUnknownOverflowTracked = 1 << 16

// boundKind makes a wire-chosen type name safe to log and display: control
// characters and invalid UTF-8 are replaced, and a name past the bound is cut
// on a rune boundary with a hash of the whole name as its suffix, so two long
// names that differ only after the cut stay two names. llm cannot use
// ui.Sanitize (ui imports llm), and unlike it this removes newlines and tabs
// too: a name is one token.
func boundKind(kind string) string {
	var b strings.Builder
	b.Grow(len(kind))
	for i := 0; i < len(kind); {
		r, size := utf8.DecodeRuneInString(kind[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteRune('�')
		case unicode.IsControl(r):
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	name := b.String()
	if len(name) <= maxUnknownKindLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "#" + hex.EncodeToString(sum[:4])
	cut := maxUnknownKindLen - len(suffix)
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut] + suffix
}

// NoteUnknown records a block or step type the decoder has no reading for. The
// content it carried is not in the settled message, and a message that is
// silently shorter than the stream looks complete; the report is how an
// operator finds out the adapter has fallen behind the wire. Names are
// sanitised, kept sorted and distinct, and bounded: UnknownKindsMore counts the
// distinct names that did not fit.
func (d *DecodingReport) NoteUnknown(kind string) {
	name := boundKind(kind)
	i := sort.SearchStrings(d.UnknownKinds, name)
	if i < len(d.UnknownKinds) && d.UnknownKinds[i] == name {
		return
	}
	if len(d.UnknownKinds) < maxUnknownKinds {
		d.UnknownKinds = append(d.UnknownKinds, "")
		copy(d.UnknownKinds[i+1:], d.UnknownKinds[i:])
		d.UnknownKinds[i] = name
		return
	}
	if _, seen := d.unknownOverflow[name]; seen {
		return
	}
	if len(d.unknownOverflow) < maxUnknownOverflowTracked {
		if d.unknownOverflow == nil {
			d.unknownOverflow = make(map[string]struct{})
		}
		d.unknownOverflow[name] = struct{}{}
	}
	d.UnknownKindsMore++
}
