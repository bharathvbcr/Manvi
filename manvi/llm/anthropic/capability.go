// Package anthropic holds the Anthropic provider's capability catalogue and
// the request rules that would otherwise surface as a 400 mid-turn.
//
// Everything here is transcribed from Anthropic's current API documentation,
// not from recollection. That distinction matters more than usual: this surface
// changed materially in 2025–2026 — sampling parameters were removed, manual
// thinking budgets were removed, thinking became on-by-default on Opus 5 — and
// a plausible-looking constant written from memory produces a 400 halfway
// through a turn rather than a compile error.
//
// The transport is deliberately not here. This package answers "is this request
// serviceable, and what does this model do", which is knowledge the harness
// needs whether it speaks HTTP itself or through an SDK.
package anthropic

import (
	"fmt"

	"manvi/llm"
)

// Name is the adapter's stable identifier. It is what lands in
// AssistantProvenance.Provider, and therefore what decides whether reasoning
// state is portable — so it must not drift.
const Name = "anthropic"

// Effort levels, passed as output_config.effort. Not every model accepts every
// level, so the per-model capability is authoritative rather than this list.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
	EffortXHigh  = "xhigh"
	EffortMax    = "max"
)

// catalogue is the verified model set. Context windows and output caps are the
// documented figures; an unlisted model is unknown rather than assumed, so a
// typo in a config fails at assembly instead of at the API.
var catalogue = map[string]llm.Capability{
	"claude-opus-5": {
		Provider: Name, Model: "claude-opus-5",
		ContextWindow: 1_000_000, MaxOutputTokens: 128_000,
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
		SupportsReasoning: true,
		EffortLevels:      []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax},
	},
	"claude-opus-4-8": {
		Provider: Name, Model: "claude-opus-4-8",
		ContextWindow: 1_000_000, MaxOutputTokens: 128_000,
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
		SupportsReasoning: true,
		EffortLevels:      []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax},
	},
	"claude-sonnet-5": {
		Provider: Name, Model: "claude-sonnet-5",
		ContextWindow: 1_000_000, MaxOutputTokens: 128_000,
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
		SupportsReasoning: true,
		EffortLevels:      []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax},
	},
	"claude-fable-5": {
		Provider: Name, Model: "claude-fable-5",
		ContextWindow: 1_000_000, MaxOutputTokens: 128_000,
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
		SupportsReasoning: true,
		EffortLevels:      []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax},
	},
	"claude-haiku-4-5": {
		Provider: Name, Model: "claude-haiku-4-5",
		// Haiku is the outlier on both axes — 200K context and a 64K output
		// cap, where the rest of the family is 1M/128K. The fan-out tier is
		// exactly where a wrong constant does damage, because a searcher is
		// handed a narrow question and a large context by default.
		ContextWindow: 200_000, MaxOutputTokens: 64_000,
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
		SupportsReasoning: false,
	},
}

// ThinkingMode is how a request configures reasoning.
type ThinkingMode string

const (
	// ThinkingAdaptive lets the model decide when and how much to think.
	ThinkingAdaptive ThinkingMode = "adaptive"
	// ThinkingDisabled turns reasoning off. Availability is model- and
	// effort-dependent — see ValidateThinking.
	ThinkingDisabled ThinkingMode = "disabled"
	// ThinkingDefault omits the parameter. What that means differs by model,
	// which is the trap ThinkingDefaultRuns exists to make explicit.
	ThinkingDefault ThinkingMode = ""
)

// thinkingOnByDefault lists models that think when the parameter is omitted.
//
// This is the silent one. On Opus 4.8 and 4.7, omitting `thinking` meant no
// thinking; on Opus 5 it means adaptive. Since max_tokens caps thinking *plus*
// response text, a request carried over unchanged can now truncate mid-answer
// with nothing in the request looking wrong.
var thinkingOnByDefault = map[string]bool{
	"claude-opus-5":   true,
	"claude-fable-5":  true,
	"claude-sonnet-5": true,
}

// alwaysThinking lists models where reasoning cannot be turned off at all.
var alwaysThinking = map[string]bool{
	"claude-fable-5": true,
}

// Capability returns the descriptor for a model, or false when this adapter
// does not serve it. An unknown model is never given a permissive default.
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

// ThinkingDefaultRuns reports whether omitting the thinking parameter results
// in reasoning for this model. Callers size max_tokens from this.
func ThinkingDefaultRuns(model string) bool { return thinkingOnByDefault[model] }

// ValidateThinking reports why a thinking configuration is not serviceable.
//
// Two rules, both of which return a 400 from the API rather than degrading:
//
//   - Reasoning cannot be disabled at all on models where it is always on.
//   - Where it can be disabled, that is only permitted at effort "high" or
//     lower; pairing disabled thinking with "xhigh" or "max" is rejected.
//
// The check is per request, so a conversation that raises effort on a later
// turn while still disabling thinking fails on that turn even though earlier
// turns succeeded. That is why this lives on the request path and not on
// construction.
func ValidateThinking(model string, mode ThinkingMode, effort string) error {
	if mode != ThinkingDisabled {
		return nil
	}
	if alwaysThinking[model] {
		return fmt.Errorf("anthropic: %s always reasons; omit the thinking parameter rather than disabling it", model)
	}
	if effort == EffortXHigh || effort == EffortMax {
		return fmt.Errorf(
			"anthropic: %s rejects disabled thinking at effort %q (permitted at %q or lower)",
			model, effort, EffortHigh)
	}
	return nil
}

// RemovedParameters names request fields that current models reject outright.
// They are listed rather than silently dropped: a harness that quietly discards
// a temperature the caller set is a harness whose output nobody can explain.
var RemovedParameters = []string{"temperature", "top_p", "top_k"}

// ValidateRequest checks an assembled request against everything this adapter
// knows will fail, before it is sent.
func ValidateRequest(req llm.Request, mode ThinkingMode) error {
	c, ok := Capability(req.Model)
	if !ok {
		return fmt.Errorf("anthropic: unknown model %q (serves %v)", req.Model, Models())
	}
	if err := c.Validate(req); err != nil {
		return err
	}
	if req.Temperature != nil {
		return fmt.Errorf(
			"anthropic: %v are not accepted by current models; steer with prompting instead",
			RemovedParameters)
	}
	if err := ValidateThinking(req.Model, mode, req.Effort); err != nil {
		return err
	}
	// A last-assistant-turn prefill is rejected by every current model. It is
	// worth catching here because the replacement is structural (structured
	// output, or a system-prompt instruction) rather than a parameter tweak.
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == llm.RoleAssistant {
		return fmt.Errorf("anthropic: %s rejects a trailing assistant message (prefill); "+
			"use a structured output format or a system-prompt instruction instead", req.Model)
	}
	return nil
}

// ReplayableOn reports whether reasoning produced by (fromModel) can be sent
// back to (toModel) intact.
//
// Anthropic's rule is exact: thinking blocks must be echoed back unchanged on
// the same model, and other models drop them. The harness's neutral layer
// implements the general form of this in llm.PrepareHistory; this function is
// the provider-specific statement of it, kept here so the rule has one owner
// per provider rather than being inferred at the call site.
func ReplayableOn(fromModel, toModel string) bool {
	return fromModel == toModel && fromModel != ""
}
