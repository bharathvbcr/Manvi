package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// ToolSchema is a tool offered to the model.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Request is one model call, in neutral terms.
type Request struct {
	Model string `json:"model"`
	// System is the assembled system prompt.
	System string `json:"system,omitempty"`
	// Messages is the conversation, already passed through PrepareHistory.
	Messages []Message    `json:"messages"`
	Tools    []ToolSchema `json:"tools,omitempty"`
	// MaxTokens bounds the response. Zero means the adapter's default, which
	// every adapter must define rather than sending an unbounded request.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Effort selects a reasoning tier when the model supports one.
	Effort string `json:"effort,omitempty"`
	// Temperature is a pointer so "unset" is distinguishable from zero.
	Temperature *float64 `json:"temperature,omitempty"`
	// TopP bounds nucleus sampling probability mass.
	TopP *float64 `json:"top_p,omitempty"`
	// TopK caps the candidate set to the k most likely tokens.
	//
	// Open-weight models ship a recommended value alongside the weights —
	// Qwen3's generation_config declares top_k 20 — and it is the main sampling
	// lever those models are tuned around. A harness that cannot send it cannot
	// run them the way they were meant to run.
	TopK *int `json:"top_k,omitempty"`
	// MinP sets minimum token probability relative to the most likely token.
	MinP *float64 `json:"min_p,omitempty"`
	// RepetitionPenalty penalizes repeating token sequences (used by local engines).
	RepetitionPenalty *float64 `json:"repetition_penalty,omitempty"`
	// PresencePenalty and FrequencyPenalty are the OpenAI-vocabulary penalties.
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	// Seed makes a local run reproducible. Hosted providers mostly treat it as
	// best-effort; a local server honours it, which is what makes a failing
	// turn investigable rather than merely re-runnable.
	Seed *int `json:"seed,omitempty"`
	// Stop sequences that terminate generation early.
	Stop []string `json:"stop,omitempty"`
}

// Capability describes what a model can actually do, so an impossible request
// fails at assembly time instead of arriving as a 400 halfway through a turn.
type Capability struct {
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	ContextWindow     int    `json:"context_window"`
	MaxOutputTokens   int    `json:"max_output_tokens"`
	SupportsTools     bool   `json:"supports_tools"`
	SupportsStreaming bool   `json:"supports_streaming"`
	SupportsImages    bool   `json:"supports_images"`
	SupportsReasoning bool   `json:"supports_reasoning"`
	// EffortLevels are the reasoning tiers this model accepts, ordered from
	// least reasoning to most.
	//
	// The order is part of the contract, not a presentation detail: the agent
	// loop raises a stuck turn's tier by one position in this list, so an
	// adapter that listed its levels in any other order would buy *less*
	// thinking for a turn that had already been shown to need more. Every
	// adapter's list is checked against that in llm's own tests.
	EffortLevels []string `json:"effort_levels,omitempty"`
}

// Validate reports why a request cannot be served by this model. A nil error
// means the shape is serviceable; it says nothing about whether the content
// will satisfy the model.
func (c Capability) Validate(req Request) error {
	if len(req.Tools) > 0 && !c.SupportsTools {
		return fmt.Errorf("model %s/%s does not support tools", c.Provider, c.Model)
	}
	if req.Effort != "" {
		if !c.SupportsReasoning {
			return fmt.Errorf("model %s/%s does not support reasoning effort", c.Provider, c.Model)
		}
		if len(c.EffortLevels) > 0 && !slices.Contains(c.EffortLevels, req.Effort) {
			return fmt.Errorf("model %s/%s supports effort %v, not %q",
				c.Provider, c.Model, c.EffortLevels, req.Effort)
		}
	}
	if c.MaxOutputTokens > 0 && req.MaxTokens > c.MaxOutputTokens {
		return fmt.Errorf("model %s/%s caps output at %d tokens, requested %d",
			c.Provider, c.Model, c.MaxOutputTokens, req.MaxTokens)
	}
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if block.Kind() == KindImage && !c.SupportsImages {
				return fmt.Errorf("model %s/%s does not accept images", c.Provider, c.Model)
			}
		}
	}
	return nil
}

// ChunkKind classifies a streaming event.
type ChunkKind string

const (
	ChunkText          ChunkKind = "text"
	ChunkReasoning     ChunkKind = "reasoning"
	ChunkToolCallStart ChunkKind = "tool_call_start"
	ChunkToolCallDelta ChunkKind = "tool_call_delta"
	ChunkDone          ChunkKind = "done"
)

// Chunk is one streaming event in neutral terms.
type Chunk struct {
	Kind ChunkKind `json:"kind"`
	// BlockIndex groups chunks belonging to the same content block.
	BlockIndex int    `json:"block_index"`
	Text       string `json:"text,omitempty"`
	// ToolCall fields, for the tool-call chunk kinds.
	ToolCallID   CallID `json:"tool_call_id,omitempty"`
	ToolName     string `json:"tool_name,omitempty"`
	ArgumentsRaw string `json:"arguments_raw,omitempty"`
}

// StopReason is why generation ended.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	StopOther     StopReason = "other"
)

// Usage is token accounting, recorded per call.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
	// Throughput, when the server reports it. Zero means it did not.
	//
	// These are the operational signals for a locally served model, where the
	// cost of a step is dominated by whether the prompt prefix was reused. A
	// harness that does not carry them cannot notice that it has started
	// re-prefilling the whole conversation every step.
	OutputTokensPerSecond float64 `json:"output_tokens_per_second,omitempty"`
	PromptTokensPerSecond float64 `json:"prompt_tokens_per_second,omitempty"`
}

// CacheReuse reports the fraction of the prompt served from the server's prefix
// cache, or -1 when the server did not say.
func (u Usage) CacheReuse() float64 {
	if u.InputTokens <= 0 || u.CacheReadTokens <= 0 {
		return -1
	}
	return float64(u.CacheReadTokens) / float64(u.InputTokens)
}

// MalformedCall is a tool call an adapter could not reconstruct from the wire.
//
// It is data rather than an error because the turn should survive it. A local
// server truncates a response far more readily than a hosted one, and a call
// cut off mid-arguments is a recoverable mistake the model can be told about —
// treating it as a transport failure discards every completed step of the turn
// along with it.
type MalformedCall struct {
	ID     CallID `json:"id"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason"`
}

// DecodingReport records compensations an adapter had to make to read a
// response. An empty report is the healthy case.
//
// None of this changes the message; all of it changes what an operator should
// do about their server, which is why it travels with the response rather than
// being logged and forgotten inside an adapter.
type DecodingReport struct {
	// FallbackFormat names the text shape a tool call had to be recovered
	// from, meaning the server did not parse tool calls for the model it is
	// serving.
	FallbackFormat string `json:"fallback_format,omitempty"`
	// ReasoningReclassified reports that a prefilled thinking tag was detected
	// and the settled message corrected.
	ReasoningReclassified bool `json:"reasoning_reclassified,omitempty"`
	// PrefillDisproved reports that the server delivered reasoning on its own
	// channel while the adapter had been told to assume a prefilled thinking
	// tag. The declaration was wrong for this server and was dropped for the
	// response; left in place it deletes answers, so it is surfaced rather
	// than compensated for in silence.
	PrefillDisproved bool `json:"prefill_disproved,omitempty"`
}

// Clean reports whether the response decoded without compensating for anything.
func (d DecodingReport) Clean() bool {
	return d.FallbackFormat == "" && !d.ReasoningReclassified && !d.PrefillDisproved
}

// Response is the settled result of a stream.
type Response struct {
	Message    Message    `json:"message"`
	StopReason StopReason `json:"stop_reason"`
	Usage      Usage      `json:"usage"`
	// MaxTokensApplied is the output bound this request actually carried, as
	// the adapter resolved it.
	//
	// It is not Capability.MaxOutputTokens. That is the model's ceiling, and
	// the two are different numbers on every provider but one: a request with
	// MaxTokens 0 means "the adapter's default", so anthropic sends 8192 while
	// declaring a 128000 ceiling, and xai and gemini declare no ceiling at all
	// while sending 8192 or nothing. A caller checking whether a response ran
	// to its budget has to compare against the number that was sent, and only
	// the adapter knows it. Zero means the request was genuinely unbounded.
	MaxTokensApplied int `json:"max_tokens_applied,omitempty"`
	// Malformed carries tool calls the adapter could not reconstruct.
	Malformed []MalformedCall `json:"malformed,omitempty"`
	// Decoding reports how the response had to be read.
	Decoding DecodingReport `json:"decoding,omitempty"`
}

// Stream yields chunks until it is exhausted, then settles into a Response.
type Stream interface {
	// Next returns the next chunk. It returns io.EOF when the stream is done.
	Next() (Chunk, error)
	// Response is valid only after Next has returned io.EOF.
	Response() (Response, error)
	// Close releases the stream. Safe to call more than once.
	Close() error
}

// Provider is one model backend behind the seam.
type Provider interface {
	// Name is the adapter's stable identifier, e.g. "anthropic" or "xai". It is
	// what PrepareHistory compares against, so it must match what an adapter
	// writes into AssistantProvenance.Provider.
	Name() string
	// Capability describes a model, or reports that this adapter does not serve
	// it. An adapter must not guess: an unknown model is a false return, not a
	// permissive default, so an unsupported request fails at assembly.
	Capability(model string) (Capability, bool)
	// Stream begins a model call.
	Stream(ctx context.Context, req Request) (Stream, error)
}

// Registry resolves providers by name. It is the cx.llm service.
type Registry struct {
	providers map[string]Provider
	order     []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}}
}

// Register adds a provider. A duplicate name is an error rather than a silent
// replacement — two adapters answering to one name would make provenance
// ambiguous, and provenance is what decides whether replay state is portable.
func (r *Registry) Register(p Provider) error {
	if _, dup := r.providers[p.Name()]; dup {
		return fmt.Errorf("llm: provider %q is already registered", p.Name())
	}
	r.providers[p.Name()] = p
	r.order = append(r.order, p.Name())
	return nil
}

// Get returns a provider by name.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// Names lists registered providers in registration order.
func (r *Registry) Names() []string {
	return append([]string(nil), r.order...)
}

// Resolve returns the provider and validated capability for a call, or an error
// naming exactly what is wrong. This is the assembly-time check that keeps an
// impossible request from becoming a mid-turn 400.
func (r *Registry) Resolve(providerName string, req Request) (Provider, Capability, error) {
	p, ok := r.Get(providerName)
	if !ok {
		return nil, Capability{}, fmt.Errorf("llm: no provider named %q (have %v)", providerName, r.order)
	}
	cap, ok := p.Capability(req.Model)
	if !ok {
		return nil, Capability{}, fmt.Errorf("llm: provider %q does not serve model %q", providerName, req.Model)
	}
	if err := cap.Validate(req); err != nil {
		return nil, Capability{}, err
	}
	return p, cap, nil
}
