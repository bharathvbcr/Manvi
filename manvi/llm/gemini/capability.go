// Package gemini holds the Google Gemini provider's wire contract and
// capability catalogue.
//
// Transcribed from Google's current API documentation, not from recollection —
// and this is the surface where that mattered most. The API has moved to an
// /v1beta/interactions endpoint whose request and response shapes differ
// substantially from the older generateContent form: input rather than
// contents, a timeline of steps rather than candidates with nested parts, and
// typed SSE events rather than a JSON array. Code written from memory would
// have targeted the old endpoint and failed in a way that looked like an
// authentication or model-name problem.
package gemini

import (
	"fmt"

	"manvi/llm"
)

// Name is the adapter's stable identifier.
const Name = "gemini"

// DefaultBaseURL is the documented API root.
const DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// InteractionsPath is the current content-generation endpoint. The older
// generateContent path is deliberately not referenced: mixing the two shapes is
// the failure this package exists to prevent.
const InteractionsPath = "/interactions"

// StreamQuery is appended to request an SSE stream.
const StreamQuery = "alt=sse"

// APIKeyHeader is the documented authentication header. Note it is a bespoke
// header rather than an Authorization bearer, which is the single easiest
// detail to get wrong when porting an adapter from another provider.
const APIKeyHeader = "x-goog-api-key"

// SSE event types on the interactions stream.
const (
	EventInteractionCreated    = "interaction.created"
	EventInteractionInProgress = "interaction.in_progress"
	// EventInteractionStatusUpdate is the progress frame the live stream
	// actually sends -- observed on every real interaction, and absent from
	// the vocabulary this package was first written against.
	EventInteractionStatusUpdate = "interaction.status_update"
	// EventError is a failure delivered as a frame rather than as a status
	// code. The decoder already acts on the error object it carries; the name
	// is here so an event this adapter handles is not counted as one it has
	// never heard of.
	EventError                 = "error"
	EventInteractionCompleted  = "interaction.completed"
	EventInteractionNeedsInput = "interaction.requires_action"
	EventStepStart             = "step.start"
	EventStepDelta             = "step.delta"
	EventStepStop              = "step.stop"
	EventDone                  = "done"
)

// DoneSentinel terminates the Gemini SSE stream.
const DoneSentinel = "[DONE]"

// Delta payload types carried inside a step.delta event.
//
// These are the values of `delta.type` on the live stream, established from a
// recorded benchmark on 2026-08-19 rather than from the prose documentation,
// which describes a different shape (content blocks nested under `step`). Both
// are decoded -- see applyDelta -- because the documented form is what every
// published example shows, and a wire that has already moved once may move
// again.
const (
	DeltaText      = "text"
	DeltaArguments = "arguments"
	DeltaThought   = "thought"

	// DeltaArgumentsDelta carries a fragment of a tool call's arguments, as a
	// JSON *string* to be concatenated. This is how arguments actually arrive:
	// the step.start frame that opens a function_call carries `"arguments": {}`
	// and nothing else, so an adapter reading only step.start dispatches every
	// tool call with no arguments at all.
	DeltaArgumentsDelta = "arguments_delta"
	// DeltaThoughtSignature carries an opaque signature for a thinking step. It
	// holds no readable text: reasoning content is not delivered on this
	// stream, only the fact that thinking happened and its token count.
	DeltaThoughtSignature = "thought_signature"
)

// Step types, which arrive on step.start and tell a delta at the same index
// what it belongs to.
const (
	StepThought      = "thought"
	StepModelOutput  = "model_output"
	StepFunctionCall = TypeFunctionCall
)

// Wire type discriminators for tools and their results.
const (
	TypeFunction       = "function"
	TypeFunctionCall   = "function_call"
	TypeFunctionResult = "function_result"
)

// StoreInteractions is the value the adapter sends for the request's `store`
// field, and it is false.
//
// It was briefly true, on a measurement that turned out to be wrong, and the
// mistake is recorded here because it is the kind that repeats. Every request
// carrying a tool result was being refused, and a probe appeared to show that
// store=true was what fixed it. It was not: the probe read only the first 200
// bytes of the response, and this endpoint sends `interaction.created` before
// it sends `event: error` — so a refusal that arrived ~300 bytes in was scored
// as a success. Under a full read, store makes no difference at all:
//
//	store=true   user + function_result(with name)   4/4 accepted
//	store=false  user + function_result(with name)   4/4 accepted
//
// What actually refused those requests was the shape of the history, not this
// field. See historyShape in adapter.go for the two rules and the table they
// were established from.
//
// So the original reasoning stands and the original value comes back. The API
// keeps conversation state server-side by default and lets a later request
// resume it with previous_interaction_id; a harness whose central invariant is
// "the session log is the complete record of what the model saw" must not let
// the provider hold part of the conversation. The field is not omitempty on
// purpose: the API's default is true, so an omitted field would opt into the
// opposite silently.
//
// The lesson worth keeping is about the instrument rather than the API: a
// truncated read of a streaming response is not a measurement, and on a wire
// that reports failure mid-stream it will read as a pass.
const StoreInteractions = false

// catalogue is the verified text-model set. Google's published model list
// includes image, video, music, embedding, robotics, and agent models; only the
// text models this chat adapter can actually drive are listed, so naming any
// other in config fails at assembly rather than at request time.
//
// Context windows and output caps are not stated on the model index, and the
// per-model pages were not read, so those fields are left at zero — meaning
// "not stated", which the neutral Capability does not enforce. Inventing the
// numbers would produce a gate that rejects requests the provider accepts.
var catalogue = map[string]llm.Capability{
	"gemini-3.7-flash": {
		Provider: Name, Model: "gemini-3.7-flash",
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
		SupportsReasoning: true,
		EffortLevels:      thinkingLevels,
	},
	"gemini-3.6-flash": {
		Provider: Name, Model: "gemini-3.6-flash",
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
		SupportsReasoning: true,
		EffortLevels:      thinkingLevels,
	},
	"gemini-3.5-flash": {
		Provider: Name, Model: "gemini-3.5-flash",
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
		SupportsReasoning: true,
		EffortLevels:      thinkingLevels,
	},
	"gemini-3.5-flash-lite": {
		Provider: Name, Model: "gemini-3.5-flash-lite",
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
	},
	"gemini-3.1-flash-lite": {
		Provider: Name, Model: "gemini-3.1-flash-lite",
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
	},
	"gemini-2.5-pro": {
		Provider: Name, Model: "gemini-2.5-pro",
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
		SupportsReasoning: true,
		EffortLevels:      thinkingLevels,
	},
	"gemini-2.5-flash": {
		Provider: Name, Model: "gemini-2.5-flash",
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
	},
	"gemini-2.5-flash-lite": {
		Provider: Name, Model: "gemini-2.5-flash-lite",
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
	},
}

// thinkingLevels are the values accepted by generation_config.thinking_level.
// Google documents "low" explicitly and describes the field as offering
// "different thinking configurations"; the harness's own effort vocabulary maps
// onto it in Effort, and an unmapped value is refused rather than passed
// through in the hope that the provider accepts it.
var thinkingLevels = []string{"low", "medium", "high"}

// Capability returns the descriptor for a model, or false when this adapter
// does not serve it.
func Capability(model string) (llm.Capability, bool) {
	c, ok := catalogue[model]
	return c, ok
}

// Models lists every model this adapter serves.
func Models() []string {
	out := make([]string, 0, len(catalogue))
	for model := range catalogue {
		out = append(out, model)
	}
	return out
}

// ValidateRequest checks an assembled request before it is sent.
func ValidateRequest(req llm.Request) error {
	c, ok := Capability(req.Model)
	if !ok {
		return fmt.Errorf("gemini: unknown model %q (serves %v)", req.Model, Models())
	}
	return c.Validate(req)
}

// ReplayableOn reports whether adapter state produced by one model can be sent
// to another.
//
// Same model, yes; anything else, no. What is being replayed is not thought
// *text* — this stream delivers none — but the thought signature that
// authorises sending a function_call back. The live API refuses a replayed call
// without one and accepts it with one, so a harness that cannot carry the
// signature cannot show the model its own actions: it ends up asking the model
// to continue from a list of results for calls it never sees, which produced
// runs of entirely empty responses in a measured benchmark.
//
// A signature is minted by one model for one moment of its own reasoning, so it
// is not portable across models, and a cross-model handoff still strips it and
// reports the loss.
func ReplayableOn(fromModel, toModel string) bool {
	return fromModel != "" && fromModel == toModel
}
