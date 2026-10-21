// Package ui is the harness's terminal face.
//
// The structure follows a lesson worth stealing from the open-source coding
// harnesses: design the wire before the chrome. The terminal is not privileged
// here. It is one consumer of a typed event stream, and the headless
// newline-delimited-JSON mode is another consuming exactly the same events. So
// anything the terminal can show, a CI job or an editor plugin can consume,
// without a second code path that drifts.
//
// The second lesson is that permissions belong on that wire rather than in a
// modal the terminal owns privately. A blocked write emits an EventApproval
// like any other event; the terminal renders it as a prompt, the headless face
// prints it and reads a decision, and an autonomous run answers it from the
// grant ledger. All three go through the same seam, which is what makes "human
// or agent may clear this" one mechanism instead of three.
package ui

import (
	"encoding/json"
	"time"
)

// Kind classifies an event on the wire.
type Kind string

const (
	KindSessionStart Kind = "session.start"
	KindTurnStart    Kind = "turn.start"
	KindText         Kind = "assistant.text"
	KindReasoning    Kind = "assistant.reasoning"
	KindToolStart    Kind = "tool.start"
	KindToolResult   Kind = "tool.result"
	KindApproval     Kind = "approval.request"
	KindApprovalDone Kind = "approval.decided"
	KindPolicy       Kind = "policy.decision"
	KindLease        Kind = "lease.change"
	KindUsage        Kind = "turn.usage"
	KindTurnEnd      Kind = "turn.end"
	KindReport       Kind = "run.report"
	KindError        Kind = "error"
	KindNotice       Kind = "notice"
	// KindHarnessMessage is a model-visible message this harness wrote itself —
	// a verifier's verdict handed back at the terminal checkpoint.
	//
	// It is a separate kind rather than a flag on KindTurnStart because the two
	// must not render alike. The only model-visible role available for such a
	// message is `user`, so without this a face shows the harness's own words
	// in the position the operator's words occupy, and a transcript — or a
	// resumed session, or an evidence report — says the person asked for
	// something they never said.
	KindHarnessMessage Kind = "harness.message"
	// KindStepUsage is what one step spent, projected from the step's own
	// session record. It is not KindUsage: turn.usage is one event per turn
	// and consumers sum it, so a per-step event under that kind would count
	// every token twice.
	KindStepUsage Kind = "step.usage"
	// KindVerifyReport is the end-of-turn check's structured verdict, emitted
	// for every verdict including a pass. It is what a headless caller reads
	// instead of parsing the notice outcomeNotices writes for a bad one.
	KindVerifyReport Kind = "verify.report"
	// KindDiagnostic is a record of the harness itself: how the prompt was
	// assembled, what was compacted, which compensations an adapter made. Type
	// names the session record it came from and Data carries its fields.
	KindDiagnostic Kind = "diagnostic"
)

// WireOnly reports whether a kind exists for the NDJSON face and not the
// terminal. A terminal already says what these carry — outcomeNotices
// summarises a bad verdict and a degraded run in prose, and turn.usage prints
// the cost — so rendering them as well would say it twice. Every terminal face
// consults this rather than keeping its own list.
func (k Kind) WireOnly() bool {
	switch k {
	case KindStepUsage, KindVerifyReport, KindDiagnostic:
		return true
	}
	return false
}

// Event is one thing that happened, in terms both faces understand.
//
// Text fields carry untrusted content — model output, tool results, task
// titles from the store — and are sanitized at the moment of rendering rather
// than here. Sanitizing on the way in would corrupt the JSON face's fidelity;
// a consumer that is not a terminal has no reason to receive escaped text.
type Event struct {
	Kind Kind      `json:"kind"`
	At   time.Time `json:"at"`

	// Agent names the sub-agent an event came from, empty for the parent turn.
	//
	// A child runs its own loop against its own log, and that log was never
	// observed or saved — so every gate refusal, grant, and adapter
	// compensation a child produced was discarded when the function returned.
	// A four-way fan-out whose every write the gate refused left no record at
	// all, and the parent consumed the child's prose as its only account of
	// itself. Attribution is what makes forwarding them safe: two agents'
	// evidence in one stream is only readable if each line says whose it is.
	Agent string `json:"agent,omitempty"`

	// Text is the human-readable body.
	Text string `json:"text,omitempty"`
	// Detail is a secondary line: a path, a model name, a duration.
	Detail string `json:"detail,omitempty"`

	// Tool fields.
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	// Policy fields. Rule is set whenever a rule fired, including on a success
	// the posture or a grant allowed — a qualified pass is not a clean one, and
	// the face must be able to say which it was.
	Rule      string   `json:"rule,omitempty"`
	Severity  string   `json:"severity,omitempty"`
	Path      string   `json:"path,omitempty"`
	Grantable bool     `json:"grantable,omitempty"`
	GrantID   string   `json:"grant_id,omitempty"`
	GrantedBy string   `json:"granted_by,omitempty"`
	Demoted   string   `json:"demoted,omitempty"`
	Degraded  []string `json:"degraded,omitempty"`
	// Weakened names safety settings moved off their defaults. Kept apart from
	// Degraded because they are different facts: a degraded check is one that
	// could not run, a weakened setting is one deliberately relaxed. Merging
	// them would report a policy decision as an infrastructure failure, and
	// the operator's response to each is different.
	Weakened   []string `json:"weakened,omitempty"`
	ApprovalID string   `json:"approval_id,omitempty"`

	// EncodeError marks a line the JSON face could not write whole.
	//
	// Set only by JSONSink, and only on a line that stands in for an event
	// some of whose fields would not marshal. It names them, so the transcript
	// carries a hole a reader can see rather than one that looks like an event
	// that never happened. Nothing produces it on the way in, and the terminal
	// face never sees it.
	EncodeError string `json:"encode_error,omitempty"`

	// Session fields.
	TaskID  string `json:"task_id,omitempty"`
	Posture string `json:"posture,omitempty"`
	Model   string `json:"model,omitempty"`

	// Usage.
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	// CostNanoUSD is what the priced part of a usage event cost, in billionths
	// of a dollar. UnpricedSteps counts the steps no price table covered; when
	// it is not zero the cost is a lower bound, never the bill.
	CostNanoUSD   int64 `json:"cost_nano_usd,omitempty"`
	UnpricedSteps int   `json:"unpriced_steps,omitempty"`
	// ByModel splits a usage event by the model that spent it — the turn's own
	// agent and every sub-agent it dispatched — so a turn whose children ran on
	// other models can still be priced line by line.
	ByModel []ModelUsage `json:"by_model,omitempty"`
	// A step.usage event also carries the rest of the step's usage. Its
	// UnpricedSteps is 1 when no price table covered the step and 0 when one
	// did, so CostNanoUSD is the step's charge only when that is 0.
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`

	// Verify report fields. Degraded, above, is the checks that could not run.
	// Bounce is the attempt within the turn, and absent means the first.
	Verdict  string   `json:"verdict,omitempty"`
	Source   string   `json:"source,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Findings []string `json:"findings,omitempty"`
	Bounce   int      `json:"bounce,omitempty"`

	// Diagnostic fields. Type is the session record's name ("system/prompt_assembled",
	// "context/overflow", ...) and Data is that record's own payload, so the
	// wire carries exactly what the log holds and cannot drift from it.
	Type string          `json:"type,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ModelUsage is one model's share of a usage event.
type ModelUsage struct {
	// Model is "provider/model".
	Model            string `json:"model"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	ReasoningTokens  int    `json:"reasoning_tokens,omitempty"`
	CacheReadTokens  int    `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int    `json:"cache_write_tokens,omitempty"`
	CostNanoUSD      int64  `json:"cost_nano_usd,omitempty"`
	UnpricedSteps    int    `json:"unpriced_steps,omitempty"`
}

// Qualified reports whether an outcome was reached by something other than the
// rules passing.
func (e Event) Qualified() bool {
	return e.GrantID != "" || e.Demoted != "" || len(e.Degraded) > 0 || len(e.Weakened) > 0
}

// Sink consumes events. The terminal renderer and the JSON writer are both
// sinks, and the loop knows about neither.
type Sink interface {
	Emit(Event)
}

// SinkFunc adapts a function to a Sink.
type SinkFunc func(Event)

// Emit calls f.
func (f SinkFunc) Emit(e Event) { f(e) }

// Discard drops events, for tests and for non-interactive callers.
var Discard Sink = SinkFunc(func(Event) {})
