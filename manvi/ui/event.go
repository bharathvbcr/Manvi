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
)

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

	// Session fields.
	TaskID  string `json:"task_id,omitempty"`
	Posture string `json:"posture,omitempty"`
	Model   string `json:"model,omitempty"`

	// Usage.
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
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

// MultiSink fans an event out to several sinks, so a run can render to a
// terminal and write a JSON transcript at once without either knowing.
type MultiSink []Sink

// Emit delivers to every sink.
func (m MultiSink) Emit(e Event) {
	for _, sink := range m {
		if sink != nil {
			sink.Emit(e)
		}
	}
}

// Discard drops events, for tests and for non-interactive callers.
var Discard Sink = SinkFunc(func(Event) {})
