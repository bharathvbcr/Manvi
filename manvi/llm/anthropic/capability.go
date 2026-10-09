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

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
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
var catalogue = llm.Catalogue{
	"claude-fable-5-1": {
		Provider: Name, Model: "claude-fable-5-1",
		ContextWindow: 1_000_000, MaxOutputTokens: 128_000,
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
		SupportsReasoning: true,
		EffortLevels:      []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax},
	},
	"claude-opus-5-5": {
		Provider: Name, Model: "claude-opus-5-5",
		ContextWindow: 1_000_000, MaxOutputTokens: 128_000,
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
		SupportsReasoning: true,
		EffortLevels:      []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax},
	},
	"claude-sonnet-5-5": {
		Provider: Name, Model: "claude-sonnet-5-5",
		ContextWindow: 1_000_000, MaxOutputTokens: 128_000,
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
		SupportsReasoning: true,
		EffortLevels:      []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax},
	},
	"claude-haiku-5-5": {
		Provider: Name, Model: "claude-haiku-5-5",
		// Unlike Haiku 4.5, the 5.5 generation has the family's 1M/128K limits
		// and reasons; its price steps up above a 100K-token prompt (see prices).
		ContextWindow: 1_000_000, MaxOutputTokens: 128_000,
		SupportsTools: true, SupportsStreaming: true, SupportsImages: true,
		SupportsReasoning: true,
		EffortLevels:      []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax},
	},
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

// prices are first-party API rates in nano-USD per token ($1/MTok = 1000),
// transcribed from https://platform.claude.com/docs/en/about-claude/pricing.
// Cache writes are the 5-minute TTL rate, 1.25x input, which is the only TTL
// this adapter requests. Cache reads are 0.025x input on Fable 5.1, 0.05x on
// Opus 5.5 and Sonnet 5.5, and 0.1x everywhere else, Haiku 5.5's two tiers
// included. Haiku 5.5 is the one model priced by prompt length; the rest bill
// the whole 1M window at one rate. Thinking is billed as output and
// Anthropic's output_tokens already includes it.
var prices = map[string]budget.Prices{
	"claude-fable-5-1":  flat(10_000, 50_000, 250),
	"claude-fable-5":    flat(10_000, 50_000, 1_000),
	"claude-opus-5-5":   flat(4_000, 20_000, 200),
	"claude-opus-5":     flat(5_000, 25_000, 500),
	"claude-opus-4-8":   flat(5_000, 25_000, 500),
	"claude-sonnet-5-5": flat(2_000, 10_000, 100),
	"claude-sonnet-5":   flat(2_000, 10_000, 200),
	"claude-haiku-5-5": {
		Rates:             budget.Rates{Input: 100, Output: 500, CacheWrite: 125, CacheRead: 10},
		LongPromptAbove:   100_000,
		LongRates:         budget.Rates{Input: 500, Output: 2_500, CacheWrite: 625, CacheRead: 50},
		ReasoningInOutput: true,
	},
	"claude-haiku-4-5": flat(1_000, 5_000, 100),
}

func flat(input, output, cacheRead int64) budget.Prices {
	return budget.Prices{
		Rates:             budget.Rates{Input: input, Output: output, CacheWrite: input * 5 / 4, CacheRead: cacheRead},
		ReasoningInOutput: true,
	}
}

// PriceOf returns what a model costs, for budget metering.
func PriceOf(model string) (budget.Prices, bool) {
	p, ok := prices[model]
	return p, ok
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
	// and silently: on Opus 4.8 and 4.7, omitting `thinking` meant no thinking;
	// on Opus 5 it means adaptive. Since max_tokens caps thinking *plus*
	// response text, a request carried over unchanged can now truncate
	// mid-answer with nothing in the request looking wrong.
	ThinkingDefault ThinkingMode = ""
)

// alwaysThinking lists models where reasoning cannot be turned off at all.
//
// Sonnet 5.5 belongs here too: it rejects {type: "disabled"} and turns thinking
// off only through a separate "between_tools" mode this adapter does not send.
var alwaysThinking = map[string]bool{
	"claude-fable-5":    true,
	"claude-fable-5-1":  true,
	"claude-opus-5-5":   true,
	"claude-sonnet-5-5": true,
}

// Capability returns the descriptor for a model, or false when this adapter
// does not serve it. An unknown model is never given a permissive default.
func Capability(model string) (llm.Capability, bool) { return catalogue.Lookup(model) }

// Models lists every model this adapter serves.
func Models() []string { return catalogue.Models() }

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
	if err := catalogue.Validate("anthropic", req); err != nil {
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
// implements the general form of this in llm.PrepareHistoryFor; this function is
// the provider-specific statement of it, kept here so the rule has one owner
// per provider rather than being inferred at the call site.
func ReplayableOn(fromModel, toModel string) bool {
	return fromModel == toModel && fromModel != ""
}
