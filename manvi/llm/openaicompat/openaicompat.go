// Package openaicompat is the one implementation of the OpenAI-compatible
// chat-completions wire in this harness.
//
// It exists because "OpenAI-compatible" is now the shape several providers
// speak — xAI's hosted API and every local server worth running (Ollama, MLX,
// vLLM, llama.cpp) — and a second transcription of the same streaming,
// tool-call-accumulation and finish-reason logic is a second place for those to
// drift. What genuinely differs between those providers is not the wire: it is
// which models exist, whether a credential is required, and which optional
// fields the server tolerates. Those are the Options below; everything else is
// shared.
//
// The parts that look like over-care are each a real failure this code has to
// survive:
//
//   - tool-call fragments are keyed by the wire's index, never by arrival
//     order, because a server may interleave two calls and appending by
//     position concatenates one call's arguments onto another's;
//   - arguments are validated as JSON before a call is surfaced, because a
//     truncated stream otherwise produces a tool call with syntactically
//     broken arguments that fails much later and much less legibly;
//   - usage is requested explicitly, because a turn with no token accounting
//     cannot be budgeted afterwards.
package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// ChatCompletionsPath is the OpenAI-compatible chat endpoint.
const ChatCompletionsPath = "/chat/completions"

// DoneSentinel terminates an OpenAI-compatible SSE stream.
const DoneSentinel = "[DONE]"

// ModelsPath is the OpenAI-compatible model listing endpoint.
const ModelsPath = "/models"

// Options configure one provider's use of this wire.
//
// Every field that has a safe default has one, so a caller that omits it gets
// working behaviour rather than a zero value that fails at request time.
type Options struct {
	// Name is the provider identifier used in errors and in the assistant
	// message's provenance. Required: provenance decides whether adapter state
	// is portable, and an empty one would make two providers indistinguishable
	// in a session log.
	Name string
	// BaseURL is the API root. Empty falls back to DefaultBaseURL.
	BaseURL string
	// DefaultBaseURL is used when BaseURL is empty.
	DefaultBaseURL string
	// DefaultMaxTokens bounds a request that did not set its own. An unbounded
	// request is not something a harness should send by default: a runaway
	// generation is billed, or on a local server occupies the only GPU, and
	// neither is interruptible after the fact.
	DefaultMaxTokens int
	// Validate checks an assembled request against the provider's catalogue
	// before it is sent. Required — a provider that serves anything should say
	// so with a permissive function rather than by leaving this nil, so the
	// choice is visible at the call site rather than implied by a zero value.
	Validate func(llm.Request) error
	// Header builds the per-request headers, including authorisation. It is a
	// function because a credential must be resolved at request time, not at
	// construction: a harness that caches one cannot pick up a rotated key.
	Header func() (http.Header, error)
	// SendReasoningEffort permits the reasoning_effort field. Servers that do
	// not understand it differ in how they respond — some ignore it, some
	// reject the whole request — so it is opt-in per provider rather than sent
	// whenever a caller happens to set Effort.
	SendReasoningEffort bool
	// StallTimeout abandons a stream that stops producing bytes without
	// ending. It bounds the gap between tokens, not the length of the stream,
	// which is the only bound that works for a local model whose legitimate
	// first-token latency can exceed a hosted model's whole response.
	//
	// Zero means DefaultStallTimeout; negative disables the watchdog.
	StallTimeout time.Duration
	// AssumeReasoningPrefill declares that the server's chat template ends the
	// prompt with an open thinking tag, so generation starts inside it. Off by
	// default: the stream detects it and corrects the settled message, and this
	// only makes the live view right from the first byte as well.
	AssumeReasoningPrefill bool
	// TextToolFallback recovers tool calls a server left in the message text
	// (<tool_call> tags, or a fenced JSON object naming an offered tool) when
	// it delivered none in tool_calls. It is for a local server that is not
	// configured with a tool-call parser for the model it serves, and it is off
	// by default: a hosted provider parses tool calls itself, so anything in
	// its text that looks like a call is the model quoting an example, and
	// running it executes prose. Every recovery is reported in
	// Response.Decoding.FallbackFormat.
	TextToolFallback bool
	// HeaderTimeout bounds the wait for response headers: zero keeps the
	// transport's default for the endpoint (see transport.Client), negative
	// disables the bound. It is a time-to-first-byte limit and never cuts off a
	// stream that is already flowing.
	HeaderTimeout time.Duration

	// stallClock drives the stall watchdog. Unexported because production has
	// exactly one clock and a nil value is it; the seam exists so a test can
	// state the gap between frames instead of sleeping for it, which is the
	// difference between an assertion about this package and an assertion
	// about how busy the machine is. See transport.StallClock and
	// TestASlowButLiveStreamOutlivesTheStallTimeout.
	stallClock transport.StallClock
}

// MaxDecodedResponseBytes bounds how much of one response a stream will hold.
//
// Nothing bounded it before. s.text, s.reasoning and every accumulator's
// arguments grew for as long as the server kept talking, and a server that
// keeps talking also keeps resetting the stall watchdog — so a server ignoring
// its own max_tokens was not a slow turn, it was the harness allocating until
// the machine gave out. Measured: 8,192,000 bytes delivered past any cap and
// every one of them buffered.
//
// 4MiB is the bound because the largest legitimate response is bounded by the
// model's output cap, and even a 128k-token completion at four bytes a token
// is about 512KiB. 4MiB leaves eight times that headroom — a local model
// writing a whole file through a tool call still lands — while stopping a
// runaway in seconds instead of minutes. It counts text, reasoning, all
// tool-call arguments and the per-call bookkeeping together, because a
// per-field cap is evaded by whichever field the runaway happens to be filling.
//
// Exceeding it is an error, never a truncation. Settling 4MiB of a runaway as
// though it were the answer would be exactly the silent corruption the rest of
// this package is written to prevent.
//
// The number is transport.MaxDecodedResponseBytes rather than a fourth copy of
// the same literal: anthropic and gemini bound the same thing, and three
// independent declarations are how they came to disagree about what the bound
// counted. This name stays because callers outside the package use it.
const MaxDecodedResponseBytes = transport.MaxDecodedResponseBytes

// DefaultStallTimeout is generous enough for a cold, large, quantised model to
// finish loading and prefill a long prompt — measured at two minutes for a
// 14.7k-token prompt on a 4-bit 27B — and short enough that a wedged server
// does not consume a thirty-minute turn budget.
const DefaultStallTimeout = 5 * time.Minute

// Adapter speaks the OpenAI-compatible chat-completions API.
type Adapter struct {
	opts   Options
	client *transport.Client
}

// New builds an adapter. It panics on an Options that cannot produce a working
// adapter, because each such case is a programming error at wiring time rather
// than a condition an operator can correct at runtime.
func New(opts Options) *Adapter {
	if opts.Name == "" {
		panic("openaicompat: Name is required; provenance depends on it")
	}
	if opts.Validate == nil {
		panic("openaicompat: Validate is required; a provider that serves any model must say so explicitly")
	}
	if opts.Header == nil {
		panic("openaicompat: Header is required; use an empty header for an unauthenticated server")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = opts.DefaultBaseURL
	}
	if opts.DefaultMaxTokens <= 0 {
		opts.DefaultMaxTokens = 8192
	}
	if opts.StallTimeout == 0 {
		opts.StallTimeout = DefaultStallTimeout
	}
	client := transport.New(opts.Name, opts.BaseURL, opts.Header)
	switch {
	case opts.HeaderTimeout > 0:
		client.ResponseHeaderTimeout = opts.HeaderTimeout
	case opts.HeaderTimeout < 0:
		client.ResponseHeaderTimeout = 0
	}
	return &Adapter{opts: opts, client: client}
}

// Name identifies the adapter.
func (a *Adapter) Name() string { return a.opts.Name }

// BaseURL reports the resolved API root, for diagnostics that must show an
// operator which endpoint was actually used.
func (a *Adapter) BaseURL() string { return a.opts.BaseURL }

// Client exposes the underlying transport so a provider can add endpoints this
// package does not model, such as model discovery.
func (a *Adapter) Client() *transport.Client { return a.client }

type wireRequest struct {
	Model         string        `json:"model"`
	Messages      []wireMessage `json:"messages"`
	Tools         []wireTool    `json:"tools,omitempty"`
	Stream        bool          `json:"stream"`
	StreamOptions *streamOpts   `json:"stream_options,omitempty"`
	// Both spellings of the output cap are sent.
	//
	// max_completion_tokens is the current OpenAI field; max_tokens is what
	// llama.cpp and several other local servers actually read. Sending only the
	// new name means those servers silently ignore the cap, and an unbounded
	// generation on a local box occupies the only GPU until it decides to stop.
	// Servers that know both treat them as the same setting, and the values
	// here are always equal, so there is no ambiguity to resolve.
	MaxCompletionTokens int      `json:"max_completion_tokens,omitempty"`
	MaxTokens           int      `json:"max_tokens,omitempty"`
	ReasoningEffort     string   `json:"reasoning_effort,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	TopP                *float64 `json:"top_p,omitempty"`
	TopK                *int     `json:"top_k,omitempty"`
	MinP                *float64 `json:"min_p,omitempty"`
	RepetitionPenalty   *float64 `json:"repetition_penalty,omitempty"`
	PresencePenalty     *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64 `json:"frequency_penalty,omitempty"`
	Seed                *int     `json:"seed,omitempty"`
	Stop                []string `json:"stop,omitempty"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireCallFunc `json:"function"`
}

type wireCallFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Stream begins a model call.
func (a *Adapter) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	if err := a.opts.Validate(req); err != nil {
		return nil, err
	}
	body, err := a.buildRequest(req)
	if err != nil {
		return nil, err
	}
	// PostStream rather than Post so an error chunk that arrives before any
	// output is retried like the status it stands for; see preflight.
	wait := transport.FirstTokenBudget(a.opts.StallTimeout)
	respBody, err := a.client.PostStream(ctx, ChatCompletionsPath, body, preflight(wait, a.opts.Name))
	if err != nil {
		return nil, err
	}
	return newStream(respBody, req.Model, req.Tools, a.opts, a.appliedMaxTokens(req)), nil
}

// appliedMaxTokens is the bound this adapter will actually send. One owner, so
// the number on the wire and the number reported back cannot drift.
func (a *Adapter) appliedMaxTokens(req llm.Request) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return a.opts.DefaultMaxTokens
}

func (a *Adapter) buildRequest(req llm.Request) (*wireRequest, error) {
	maxTokens := a.appliedMaxTokens(req)
	effort := req.Effort
	if !a.opts.SendReasoningEffort {
		effort = ""
	}
	out := &wireRequest{
		Model:  req.Model,
		Stream: true,
		// Usage is not sent on a stream unless it is asked for, and a turn with
		// no token accounting cannot be budgeted or costed afterwards.
		StreamOptions:       &streamOpts{IncludeUsage: true},
		MaxCompletionTokens: maxTokens,
		MaxTokens:           maxTokens,
		ReasoningEffort:     effort,
		Temperature:         req.Temperature,
		TopP:                req.TopP,
		TopK:                req.TopK,
		MinP:                req.MinP,
		RepetitionPenalty:   req.RepetitionPenalty,
		PresencePenalty:     req.PresencePenalty,
		FrequencyPenalty:    req.FrequencyPenalty,
		Seed:                req.Seed,
		Stop:                req.Stop,
	}
	if req.System != "" {
		out.Messages = append(out.Messages, wireMessage{Role: "system", Content: req.System})
	}
	for i, msg := range req.Messages {
		converted, err := toWireMessages(msg, a.opts.Name)
		if err != nil {
			return nil, fmt.Errorf("%s: message %d: %w", a.opts.Name, i, err)
		}
		out.Messages = append(out.Messages, converted...)
	}
	for _, tool := range req.Tools {
		params := tool.InputSchema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, wireTool{
			Type:     "function",
			Function: wireFunction{Name: tool.Name, Description: tool.Description, Parameters: params},
		})
	}
	return out, nil
}

// toWireMessages converts one neutral message, which may become several.
//
// The shapes genuinely differ here rather than merely being spelled
// differently: a neutral message can carry several tool results, and this API
// requires one message per result, each naming its call id. Flattening them
// into a single message would pair every result with the first call.
func toWireMessages(msg llm.Message, name string) ([]wireMessage, error) {
	var results []wireMessage
	primary := wireMessage{Role: string(msg.Role)}
	var text strings.Builder

	for _, block := range msg.Content {
		switch b := block.(type) {
		case llm.TextBlock:
			text.WriteString(b.Text)
		case llm.ReasoningBlock:
			// Reasoning is not replayable on this wire (see each provider's
			// ReplayableOn), and PrepareHistoryFor strips it before a request is
			// assembled. One arriving here means it was not stripped, and
			// sending it as text would silently change the prompt.
			return nil, errors.New("reasoning blocks are not replayable on this provider and must be stripped by PrepareHistoryFor")
		case llm.ImageBlock:
			return nil, errors.New("this adapter does not send images")
		case llm.ToolCallBlock:
			args := string(b.Arguments)
			if args == "" {
				args = "{}"
			}
			primary.ToolCalls = append(primary.ToolCalls, wireToolCall{
				ID: string(b.ID), Type: "function",
				Function: wireCallFunc{Name: b.Name, Arguments: args},
			})
		case llm.ToolResultBlock:
			var body strings.Builder
			for _, inner := range b.Content {
				if t, ok := inner.(llm.TextBlock); ok {
					body.WriteString(t.Text)
				} else {
					return nil, fmt.Errorf("this adapter cannot send non-text tool result content (%T)", inner)
				}
			}
			results = append(results, wireMessage{
				Role: "tool", ToolCallID: string(b.ToolCallID), Content: body.String(),
			})
		default:
			return nil, fmt.Errorf("cannot send a %s block", block.Kind())
		}
	}

	primary.Content = text.String()
	var out []wireMessage
	if primary.Content != "" || len(primary.ToolCalls) > 0 {
		out = append(out, primary)
	}
	out = append(out, results...)
	if len(out) == 0 {
		return nil, errors.New("message has no sendable content")
	}
	return out, nil
}
