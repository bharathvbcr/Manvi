// Package xai holds the xAI (Grok) provider's wire contract and capability
// catalogue.
//
// Transcribed from xAI's current API documentation, not from recollection.
// The endpoint is OpenAI-compatible, which is convenient and also the main
// trap: "OpenAI-compatible" is a statement about request shape, not about
// which parameters a given model accepts. reasoning_effort in particular is
// documented as supported on one model, so treating it as a family-wide
// parameter produces errors on the others.
package xai

import (
	"fmt"

	"manvi/llm"
)

// Name is the adapter's stable identifier, and therefore the provenance value
// that decides whether adapter state is portable.
const Name = "xai"

// DefaultBaseURL is xAI's documented API root. It is a default rather than a
// constant in the flag catalogue so a proxy or gateway can be pointed at
// without a rebuild.
const DefaultBaseURL = "https://api.x.ai/v1"

// ChatCompletionsPath is the OpenAI-compatible chat endpoint.
const ChatCompletionsPath = "/chat/completions"

// DoneSentinel terminates an OpenAI-compatible SSE stream.
const DoneSentinel = "[DONE]"

// Reasoning effort values, passed as the request's reasoning_effort field.
// "none" disables reasoning; "low" is the documented default.
const (
	EffortNone   = "none"
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
)

// reasoningEfforts is the accepted set for models that support the parameter.
var reasoningEfforts = []string{EffortNone, EffortLow, EffortMedium, EffortHigh}

// catalogue is the verified text-model set. The image and video models xAI also
// publishes are deliberately absent: this seam is a chat-completions adapter,
// and listing a model it cannot drive would let a config name one and fail at
// request time rather than at assembly.
//
// Output caps are not documented per model. Rather than invent a number, the
// descriptor leaves MaxOutputTokens at zero, which the neutral Capability
// treats as "no stated cap" and therefore does not enforce. A fabricated cap
// would be worse than none: it would reject requests the provider accepts.
var catalogue = map[string]llm.Capability{
	"grok-4.6": {
		Provider: Name, Model: "grok-4.6",
		ContextWindow: 500_000,
		SupportsTools: true, SupportsStreaming: true,
	},
	"grok-4.5": {
		Provider: Name, Model: "grok-4.5",
		ContextWindow: 500_000,
		SupportsTools: true, SupportsStreaming: true,
	},
	"grok-4.3": {
		Provider: Name, Model: "grok-4.3",
		ContextWindow: 1_000_000,
		SupportsTools: true, SupportsStreaming: true,
		// The one model documented as supporting reasoning_effort.
		SupportsReasoning: true,
		EffortLevels:      reasoningEfforts,
	},
	"grok-4.20-0309-reasoning": {
		Provider: Name, Model: "grok-4.20-0309-reasoning",
		ContextWindow: 1_000_000,
		SupportsTools: true, SupportsStreaming: true,
		SupportsReasoning: true,
		EffortLevels:      reasoningEfforts,
	},
	"grok-4.20-0309-non-reasoning": {
		Provider: Name, Model: "grok-4.20-0309-non-reasoning",
		ContextWindow: 1_000_000,
		SupportsTools: true, SupportsStreaming: true,
	},
	"grok-build-0.1": {
		Provider: Name, Model: "grok-build-0.1",
		ContextWindow: 256_000,
		SupportsTools: true, SupportsStreaming: true,
	},
}

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
		return fmt.Errorf("xai: unknown model %q (serves %v)", req.Model, Models())
	}
	return c.Validate(req)
}

// ReplayableOn reports whether adapter state produced by one model can be sent
// to another.
//
// xAI publishes no documented mechanism for replaying model-internal reasoning
// state across a turn boundary, so the adapter claims none. Returning false
// unconditionally is the fail-closed answer: the neutral layer will strip
// reasoning blocks and *report* the drop, which is a visible, explainable loss.
// Claiming replayability the provider has not documented would instead produce
// silent quality loss or a rejected request.
func ReplayableOn(fromModel, toModel string) bool { return false }
