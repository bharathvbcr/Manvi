// Package serve exposes the harness's local-LLM and policy planes to a host
// process over stdio, so a host that is not written in Go can use them without
// a cgo boundary or a second implementation.
//
// The wire is NDJSON: one JSON object per line, in both directions. It follows
// the shape the harness already uses to reach dcstore, dcverify and devmap —
// single JSON objects per call — with one addition that those did not need.
// Those are request/response; a chat step also *streams*, so this channel
// carries three kinds of line rather than two:
//
//	{"id":"7","op":"policy.check.file","params":{…}}   host → harness  (request)
//	{"id":"7","event":"delta","data":{…}}              harness → host  (event)
//	{"id":"7","ok":true,"result":{…}}                  harness → host  (response)
//
// Every line carries the id of the request it belongs to, so a host may have
// several calls outstanding. Exactly one response is written per request, and
// it is always the last line for that id — a host that has seen the response
// can free the correlation entry knowing no further event will reference it.
//
// Two properties of this protocol are load-bearing, and both exist because the
// host is a GUI that must stay responsive:
//
//   - Errors are values, not stream terminations. A failed request is a line
//     with ok:false, and the connection stays up. A sidecar that exited on a
//     bad request would take every other in-flight call down with it.
//   - Unknown fields are ignored rather than refused, in both directions. The
//     host and the harness are versioned and shipped separately, so a newer
//     harness must not break an older host by adding a result field.
package serve

import "encoding/json"

// ProtocolVersion is the wire contract's version.
//
// It is major-only on purpose: additive changes (a new op, a new result field)
// do not move it, because the ignore-unknown-fields rule above already makes
// those compatible. It moves when an existing field changes meaning, which is
// the only case a host cannot absorb silently — and the case where continuing
// to talk is worse than refusing to.
const ProtocolVersion = 1

// Request is one call from the host.
type Request struct {
	// ID correlates the response and any events with this call. The host
	// chooses it; the harness only echoes it. An empty ID is refused, because
	// a response the host cannot route is indistinguishable from a hang.
	ID string `json:"id"`
	// Op names the operation. See the Op* constants.
	Op string `json:"op"`
	// Params is the operation's argument object, decoded by the handler.
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the single terminal line for a request.
type Response struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Event is a non-terminal line emitted while a request is in flight.
type Event struct {
	ID    string          `json:"id"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Error is a failure the host can branch on.
//
// Code is the part a host should switch on and Message the part it should
// show. They are separate because the host renders in a GUI: a message that
// has to be pattern-matched to be understood forces the host to depend on
// wording, and wording is the thing most likely to be improved later.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Retryable reports whether reissuing the identical request could
	// plausibly succeed. It is stated rather than inferred from the code so
	// that a host does not have to maintain its own list of which transport
	// failures are transient — that list is exactly the thing that goes stale.
	Retryable bool `json:"retryable,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Error codes. A host may encounter a code it does not know — the set grows —
// so an unrecognised code must be handled as a generic failure rather than as
// a protocol violation.
const (
	// ErrBadRequest is a malformed line, an unknown op, or params that do not
	// decode. It is never retryable: the same bytes fail the same way.
	ErrBadRequest = "E_BAD_REQUEST"
	// ErrUnreachable is a server the harness could not reach.
	ErrUnreachable = "E_UNREACHABLE"
	// ErrNotServed is a model the server answered about and does not serve.
	// Distinct from ErrUnreachable because the remedies differ: one is a
	// process to start, the other a model to pull or a name to correct.
	ErrNotServed = "E_NOT_SERVED"
	// ErrTooLarge is a request line past the wire cap. The session survives
	// it; the remedy is a smaller request — fewer messages, or compaction.
	ErrTooLarge = "E_TOO_LARGE"
	// ErrInternal is a defect in the harness rather than in the request.
	ErrInternal = "E_INTERNAL"
)

// Operations.
const (
	// OpHello negotiates the protocol version and reports what the harness can
	// do. A host should call it first and refuse to proceed on a major
	// mismatch, because the alternative is discovering the mismatch as a
	// mis-decoded policy decision.
	OpHello = "hello"
	// OpPolicyCheckFile evaluates one file write against the write gate.
	OpPolicyCheckFile = "policy.check.file"
	// OpPolicyCheckCommand evaluates one shell command against the command gate.
	OpPolicyCheckCommand = "policy.check.command"
	// OpCapabilityProbe reports a model's discovered dimensions and their
	// provenance.
	OpCapabilityProbe = "capability.probe"
	// OpChatPrepare plans what to shorten before a request goes out.
	OpChatPrepare = "chat.prepare"
	// OpChatSettle reads a finished reply: reasoning, tool calls the server
	// did not parse, and truncation.
	OpChatSettle = "chat.settle"
	// OpChatForget drops a conversation's chat state.
	OpChatForget = "chat.forget"
)

// HelloParams is what the host declares about itself.
type HelloParams struct {
	// Protocol is the version the host was built against.
	Protocol int `json:"protocol"`
	// Host names the calling program, for diagnostics only.
	Host string `json:"host,omitempty"`
}

// HelloResult is what the harness declares back.
type HelloResult struct {
	Protocol int `json:"protocol"`
	// Ops lists the operations this build serves, so a host can degrade
	// gracefully rather than discovering an unimplemented op mid-turn.
	Ops []string `json:"ops"`
	// Posture names how taskless policy decisions are resolved. See Posture.
	Posture string `json:"posture"`
}
