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
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"manvi/llm"
	"manvi/llm/transport"
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
	return &Adapter{
		opts:   opts,
		client: transport.New(opts.Name, opts.BaseURL, opts.Header),
	}
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
	resp, err := a.client.Post(ctx, ChatCompletionsPath, body)
	if err != nil {
		return nil, err
	}
	return newStream(resp.Body, req.Model, req.Tools, a.opts, a.appliedMaxTokens(req)), nil
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
			// ReplayableOn), and PrepareHistory strips it before a request is
			// assembled. One arriving here means it was not stripped, and
			// sending it as text would silently change the prompt.
			return nil, errors.New("reasoning blocks are not replayable on this provider and must be stripped by PrepareHistory")
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

type stream struct {
	sse   *transport.SSE
	model string
	name  string
	// maxTokensApplied is the output bound this request carried, carried back
	// so the caller can tell a response that ran to its budget from one the
	// server merely labelled "stop". See llm.Response.MaxTokensApplied.
	maxTokensApplied int
	// tools are the schemas the request offered. They are the authority on how
	// to type a parameter recovered from a text-shaped tool call, where the
	// wire carries no types at all.
	tools []llm.ToolSchema

	text      strings.Builder
	reasoning strings.Builder
	// calls are keyed by a slot, which is the wire's index whenever that index
	// is free. It is not always: see resolveAccumulator, where a server that
	// omits the index makes every call claim slot 0.
	calls map[int]*callAccumulator
	// callsByID resolves a fragment by the id the server gave it, which is the
	// only discriminator left when the index is absent.
	callsByID map[string]*callAccumulator
	// openAtIndex is the accumulator a fragment carrying that wire index most
	// recently landed in, so a continuation fragment carrying neither an id nor
	// a usable index still joins the call it belongs to.
	openAtIndex map[int]*callAccumulator
	callOrder   []int
	filter      tagFilter
	pending     []llm.Chunk

	// argBytes counts tool-call argument bytes accumulated across all calls.
	// Text and reasoning can be measured from their builders; arguments are
	// spread over a map of accumulators, so they are tallied as they arrive.
	argBytes int

	// callBytes counts what the per-call bookkeeping itself costs: one charge
	// of transport.RetainedAccumulatorBytes per accumulator opened, plus the
	// id and name each one retains. Tallied as it changes, for the same reason
	// argBytes is and one more: decodedBytes runs once per frame, so walking
	// the accumulators to total this would make the cap that bounds a runaway
	// quadratic in the size of the runaway.
	callBytes int

	stopReason llm.StopReason
	usage      llm.Usage
	done       bool
	failure    error
	// fallbackFormat names the shape a tool call had to be recovered from,
	// when the server did not parse it. Empty is the healthy case.
	fallbackFormat FallbackFormat
	// malformed collects calls that could not be reconstructed, so the loop can
	// tell the model rather than the turn dying on a truncated response.
	malformed []MalformedCall
	// settled caches what Response assembled, so asking twice answers twice
	// with the same value. It used to append to s.malformed on every call: a
	// caller that logged, rendered and then persisted one response reported the
	// same broken call three times, and the count grew with the number of
	// readers rather than with the number of faults. A settled response is a
	// value; reading it must not change it.
	settled *llm.Response
	// blockIndex counts emitted blocks so chunks from one logical block share
	// an index, which is what a renderer groups on.
	nextIndex int
	textIndex int
	reasonIdx int
}

type callAccumulator struct {
	index int
	id    string
	name  string
	args  strings.Builder
	// idFromWire distinguishes an id the server sent from one this package
	// invented. An accumulator is given a synthetic id the moment it is
	// created, so "does it already have an id" cannot answer whether a later
	// fragment's id identifies this call or a different one.
	idFromWire bool
	// started records that the tool-call-start chunk has been emitted, so a
	// call split across many deltas announces itself exactly once.
	started bool
	// block is the chunk block index assigned to this call.
	block int
}

// newStream is the only place a stream is built, so every stream that exists
// carries the provider's stall watchdog and prefill declaration.
//
// It takes the whole Options rather than the two fields it reads: the previous
// shape built an unwatched stream here and then replaced its reader in Stream,
// which allocated the reader's megabyte buffer twice and left this constructor
// producing a stream that would wait on a wedged server forever. A caller that
// used it directly — every test that did — was exercising a stream production
// never builds.
func newStream(body io.ReadCloser, model string, tools []llm.ToolSchema, opts Options, maxTokensApplied int) *stream {
	st := &stream{
		sse:         transport.NewSSEWithStall(body, DoneSentinel, opts.StallTimeout),
		model:       model,
		name:        opts.Name,
		tools:       tools,
		calls:       map[int]*callAccumulator{},
		callsByID:   map[string]*callAccumulator{},
		openAtIndex: map[int]*callAccumulator{},
		textIndex:   -1,
		reasonIdx:   -1,

		maxTokensApplied: maxTokensApplied,
	}
	st.filter.assumePrefill = opts.AssumeReasoningPrefill
	return st
}

// tagFilter separates inline reasoning from visible text.
//
// Two shapes have to work. The ordinary one is a matched pair: the model emits
// <think>…</think> inside the content stream and the reasoning is what lies
// between them.
//
// The other is a *prefilled* opening tag. Qwen3's chat template ends the
// generation prompt with a bare "<think>", so the model begins generating
// inside the block and the only tag it ever emits is the closing one. Read as
// ordinary text that put the whole chain of thought into the answer, left a
// literal "</think>" in the output, and — because it was stored as text rather
// than reasoning — replayed all of it to the model on every later step.
//
// An unmatched closing tag is therefore read as evidence that the block was
// open from the start, and the content before it is *retroactively* reclassified
// as reasoning. Retroactively, rather than by holding text back until the
// question is settled: buffering would delay every visible token on the servers
// that never prefill, which is most of them. The chunks already emitted are
// spent, but the settled message is what gets logged and replayed, and that is
// what this corrects. An operator who wants the live view right from the first
// byte declares the prefill instead.
type tagFilter struct {
	// depth counts open think blocks. One would do if models never nested
	// them, and the previous shape assumed so: an inner </think> closed the
	// outer block, so "<think>outer <think>inner</think> still-thinking
	// </think>ANSWER" put " still-thinking" and the answer on the same side and
	// left the outer closing tag to be dropped as stray framing. The settled
	// message is what gets logged and replayed, so the model's private
	// reasoning became part of its answer and was fed back on every later step.
	//
	// Counting costs the opposite error — a model that writes a literal
	// "<think>" *inside* its reasoning now needs two closing tags — but that
	// direction fails safe: the excess is classified as reasoning, and an
	// unclosed block is flushed as reasoning at end of stream. Leaking
	// reasoning into an answer is the failure that cannot be undone.
	depth   int
	inThink bool
	carry   string
	// resolved records that the prefill question is settled — an opening tag
	// arrived, or an unmatched closing tag was seen.
	resolved bool
	// prefillSuspected records that an unmatched closing tag arrived, which is
	// what a server that prefills the opening tag looks like from here. It is
	// reported, never acted on: acting on it deleted answers.
	prefillSuspected bool
	// assumePrefill starts the filter inside a think block, for a server the
	// operator has declared prefills the tag.
	assumePrefill bool
	// started records that assumePrefill has been applied.
	started bool
	// assumed records that the filter is inside a think block it was told
	// about rather than one it saw opened. Only that state may be cancelled by
	// outOfBandReasoning; a block opened by a real tag is not in question.
	assumed bool
	// visibleTail is the last few bytes handed to the visible channel, kept so
	// framing removal can be checked against everything already emitted rather
	// than only against the run immediately before it.
	//
	// A pairwise check is not enough: "<t</think>hink</think>>" deletes two
	// tags, and neither deletion fuses a marker on its own, but the three
	// surviving runs concatenate to "<think>". The consumer sees one stream, so
	// the comparison has to be against that stream.
	visibleTail string
	// prefillDisproved records that the declared prefill was contradicted by
	// the server itself. Reported, so an operator can correct the setting
	// rather than discovering it as missing answers.
	prefillDisproved bool
	// inToolCall suspends tag interpretation for the body of a <tool_call>
	// block, because those bytes are a payload the model was asked to produce
	// and not prose it is narrating.
	//
	// Without it, a tool argument whose *value* contains "</think>" had the tag
	// deleted from the value on its way past: write(s="please strip </think>
	// markers") reached the tool as "please strip  markers", the file landed on
	// disk with content nobody asked for, and nothing anywhere reported the
	// edit. The same bytes also set prefillSuspected, so the harness told the
	// operator their server might be prefilling <think> on the strength of a
	// closing tag the model had been *instructed* to write.
	//
	// The filter runs over the raw byte stream and extractFallbackToolCalls
	// runs after it, so the stream is the only place that can know the
	// difference — by the time the parser sees the text the bytes are already
	// gone. Hence the flag here rather than a repair downstream.
	//
	// Suspension preserves the current classification rather than forcing
	// text: a block opened inside a think block stays reasoning. That keeps
	// both truncation directions fail-safe. A <tool_call> opened in the answer
	// and never closed leaves the tail as text, so no answer is deleted; one
	// opened inside reasoning and never closed leaves the tail as reasoning, so
	// no private chain of thought leaks into the answer. Nothing is buffered
	// while waiting for the closer — protected bytes are emitted as they
	// arrive — so an unclosed block costs no memory and cannot defeat
	// MaxDecodedResponseBytes.
	inToolCall bool
}

var (
	openTags  = []string{"<think>", "<thought>"}
	closeTags = []string{"</think>", "</thought>"}
	allTags   = []string{"<think>", "<thought>", "</think>", "</thought>"}
)

// The tool-call markers the filter has to recognise. Only <tool_call>…
// </tool_call> is protected, which is exactly what extractTaggedCalls treats as
// a payload — the filter and the parser must agree on where a call begins and
// ends or one of them is protecting bytes the other is not reading.
//
// That is why the inner Qwen spellings, <function=…> and <parameter=…>, are not
// listed. They are not protected in their own right because they are never
// recognised in their own right: parseCallPayload only ever sees them inside a
// <tool_call> body, so protecting the outer block already covers every
// parameter value inside it, and protecting a bare <function= would suspend
// reasoning separation for text that can never become a call.
//
// The fenced-JSON spelling (```json {…} ```) is deliberately left unprotected,
// and it keeps the residual: a fenced call whose argument string contains a
// literal "</think>" still loses it. Protecting fences would cost far more than
// it buys. A fence is ordinary prose in a coding harness — models write code
// blocks constantly, inside their reasoning most of all — whereas a literal
// "<tool_call>" is machinery a model only emits when it means it. Protecting
// every fence would therefore suspend reasoning separation across a large
// fraction of real reasoning, and a fence opened *inside* a think block would
// swallow the closing </think> and turn the entire remaining answer into
// reasoning. Worse, a fence cannot be recognised as a call until its body is
// complete and parsed, which in a stream means buffering the whole fence before
// deciding — unbounded, against a cap this package exists to respect. The
// tagged spellings are decidable from the opening marker alone; the fenced one
// is not.
const (
	toolCallOpen  = "<tool_call>"
	toolCallClose = "</tool_call>"
)

var (
	toolCallCloseTags = []string{toolCallClose}
	// carryTags is what partialSuffix must consider while tag interpretation is
	// live, so a <tool_call> opener split across two SSE frames is held back
	// exactly as a split <think> is. A server that happened to break the frame
	// after "<tool" would otherwise strip the payload anyway, which makes the
	// protection decorative rather than real.
	//
	// It is a separate list from allTags on purpose: allTags is the set of tags
	// that must never survive into content, and <tool_call> must survive —
	// extractTaggedCalls is what reads it.
	carryTags = []string{"<think>", "<thought>", "</think>", "</thought>", toolCallOpen}
)

// classify labels a run of bytes with whichever side of the filter is currently
// open, so protected tool-call payload keeps the classification it was opened
// in instead of being forced onto one side.
// emit records a chunk and keeps visibleTail current. Every visible emission
// goes through here so the tail cannot silently fall behind.
func (f *tagFilter) emit(out []filteredChunk, c filteredChunk) []filteredChunk {
	if c.text != "" {
		tail := f.visibleTail + c.text
		if n := longestCarryTag() - 1; n > 0 && len(tail) > n {
			tail = tail[len(tail)-n:]
		}
		f.visibleTail = tail
	}
	return append(out, c)
}

func longestCarryTag() int {
	longest := 0
	for _, t := range carryTags {
		if len(t) > longest {
			longest = len(t)
		}
	}
	return longest
}

func (f *tagFilter) classify(s string) filteredChunk {
	if f.inThink {
		return filteredChunk{reasoning: s}
	}
	return filteredChunk{text: s}
}

type filteredChunk struct {
	text      string
	reasoning string
}

// findFirst returns the earliest occurrence of any tag, and its length.
func findFirst(data string, tags []string) (int, int) {
	idx, tagLen := -1, 0
	for _, tag := range tags {
		at := strings.Index(data, tag)
		if at < 0 {
			continue
		}
		if idx < 0 || at < idx {
			idx, tagLen = at, len(tag)
		}
	}
	return idx, tagLen
}

// partialSuffix returns how many trailing bytes could be the start of a tag, so
// a tag split across two deltas is not mistaken for content.
func partialSuffix(data string, tags []string) int {
	longest := 0
	for _, tag := range tags {
		for l := len(tag) - 1; l >= 1; l-- {
			if strings.HasSuffix(data, tag[:l]) && l > longest {
				longest = l
			}
		}
	}
	return longest
}

// deltaReasoning picks whichever of the two out-of-band reasoning spellings a
// server used. A server sending both would be sending the same text twice, so
// the first non-empty one wins rather than being concatenated.
func deltaReasoning(reasoningContent, reasoning *string) string {
	if reasoningContent != nil && *reasoningContent != "" {
		return *reasoningContent
	}
	if reasoning != nil && *reasoning != "" {
		return *reasoning
	}
	return ""
}

// outOfBandReasoning tells the filter that the server delivered reasoning on
// its own channel, and reports whether that contradicted a declared prefill.
//
// assumePrefill exists because some servers begin generation inside a thinking
// tag and never send the opening one, so the content stream arrives already
// inside a block. A server that puts reasoning in its own field is doing the
// opposite: it has separated the two itself, and what arrives on the content
// channel is the answer, outside any block.
//
// Believing the declaration over the wire deletes answers. Measured against
// omlx 0.6.2 serving Qwen3.8-27B on 2026-08-19, with
// llm.local.assume_reasoning_prefill true: the server streamed its thinking as
// reasoning_content deltas and the answer as content "\n\nDONE", with no tag
// anywhere. The filter, told it had begun inside a block, classified the whole
// answer as reasoning and flushed it as reasoning at end of stream. Every text
// answer in a four-task benchmark was lost this way — three of the four turns
// also made no edits, because the model could no longer see its own prose in
// the history it was replayed.
//
// The setting is not ignored, and this is not a guess: it is the server
// contradicting the operator, in the one way that cannot be mistaken for
// anything else. The contradiction is reported rather than silently absorbed,
// so the setting gets corrected instead of quietly compensated for forever.
func (f *tagFilter) outOfBandReasoning() bool {
	if !f.assumePrefill {
		return false
	}
	// Never applied yet: disarm it before feed() ever acts on it.
	if !f.started {
		f.assumePrefill = false
		f.prefillDisproved = true
		return true
	}
	// Applied, and still inside the block it invented. Step out. A block that
	// a real tag opened is not touched — f.assumed is false for those.
	if f.assumed {
		f.inThink = false
		f.depth = 0
		f.assumed = false
		f.assumePrefill = false
		f.prefillDisproved = true
		return true
	}
	return false
}

// wouldFuse reports whether deleting framing between these two runs would splice
// a marker into existence.
//
// Deleting framing makes the bytes either side of it adjacent. They were never
// adjacent in the model's output, and their join can form a marker nothing
// downstream can tell from a real one. Found by fuzzing:
//
//	"<t</think>hink>"  ->  visible text "<think>"
//
// which contradicts this file's own invariant that no think tag survives into
// content. The consequential form manufactures a tool call:
//
//	"<tool</think>_call>{\"name\":\"write_file\",…}</tool_call>"
//	 -> "<tool_call>{\"name\":\"write_file\",…}</tool_call>"
//	 -> RecoverFromText returns a write_file call the model never made
//
// extractFallbackToolCalls only recovers names that were offered, so a
// fabricated call always names a real tool.
//
// When this fires the tag is kept as literal text instead of dropped. That is
// the trade this file already made once, in the other direction and for the
// same reason: "Leaking reasoning into a visible answer is a cosmetic fault the
// operator can see and fix; deleting the answer is neither." A stray "</think>"
// in an answer is visible and harmless. A tool call the model never wrote is
// neither.
func wouldFuse(before, after string) bool {
	longest := 0
	for _, t := range carryTags {
		if len(t) > longest {
			longest = len(t)
		}
	}
	if longest < 2 {
		return false
	}
	if len(before) > longest-1 {
		before = before[len(before)-(longest-1):]
	}
	if len(after) > longest-1 {
		after = after[:longest-1]
	}
	if before == "" || after == "" {
		return false
	}
	joined := before + after
	for _, t := range carryTags {
		for i := 0; i+len(t) <= len(joined); i++ {
			if joined[i:i+len(t)] != t {
				continue
			}
			// Only a match that actually spans the seam is fabricated; one
			// wholly inside either side was already there.
			if i < len(before) && i+len(t) > len(before) {
				return true
			}
		}
	}
	return false
}

func (f *tagFilter) feed(raw string) []filteredChunk {
	if !f.started {
		f.started = true
		if f.assumePrefill {
			f.inThink = true
			f.depth = 1
			f.resolved = true
			f.assumed = true
		}
	}

	data := f.carry + raw
	f.carry = ""
	var out []filteredChunk

	for len(data) > 0 {
		if f.inToolCall {
			// Inside a tool-call block nothing is a tag. These bytes are a
			// payload — a path, a patch, a file the model was asked to write —
			// and a "</think>" among them is a character the tool is supposed
			// to receive, not framing to drop and not evidence about the
			// server's chat template. prefillSuspected is unreachable from
			// here, which is the point: it must describe the template, never
			// the file the model happens to be writing.
			//
			// The first "</tool_call>" ends the block, matching
			// extractTaggedCalls byte for byte. A value containing a literal
			// "</tool_call>" ends the call early for the parser too, so filter
			// and parser agree about where the payload stopped rather than
			// disagreeing silently.
			closeIdx := strings.Index(data, toolCallClose)
			if closeIdx >= 0 {
				end := closeIdx + len(toolCallClose)
				out = f.emit(out, f.classify(data[:end]))
				data = data[end:]
				f.inToolCall = false
				continue
			}
			// Only the closing marker can matter now, so only its prefixes are
			// held back — at most len("</tool_call>")-1 bytes. Everything else
			// is emitted as it arrives: a block that never closes must not grow
			// a buffer, which is what keeps this inside MaxDecodedResponseBytes
			// instead of defeating it.
			if partial := partialSuffix(data, toolCallCloseTags); partial > 0 {
				if safe := data[:len(data)-partial]; safe != "" {
					out = f.emit(out, f.classify(safe))
				}
				f.carry = data[len(data)-partial:]
				break
			}
			out = f.emit(out, f.classify(data))
			break
		}

		if !f.inThink {
			toolIdx := strings.Index(data, toolCallOpen)
			closeIdx, closeLen := findFirst(data, closeTags)
			openIdx, openLen := findFirst(data, openTags)

			// A tool-call opener arriving before any think tag suspends tag
			// interpretation for what follows. The marker itself is emitted
			// rather than swallowed as framing, because unlike a think tag it
			// is not framing: extractFallbackToolCalls reads it, and a filter
			// that ate it would leave the parser nothing to recognise.
			//
			// Matching is exact, as it is for the think tags — see
			// TestTagMatchingIsDeliberatelyCaseAndSpaceExact. A loose match
			// here would let prose that merely mentions the marker switch the
			// reasoning filter off for the rest of the response.
			if toolIdx >= 0 && (openIdx < 0 || toolIdx < openIdx) && (closeIdx < 0 || toolIdx < closeIdx) {
				if toolIdx > 0 {
					out = f.emit(out, f.classify(data[:toolIdx]))
				}
				end := toolIdx + len(toolCallOpen)
				out = f.emit(out, f.classify(data[toolIdx:end]))
				data = data[end:]
				f.inToolCall = true
				continue
			}

			if closeIdx >= 0 && (openIdx < 0 || closeIdx < openIdx) {
				if !f.resolved {
					// A closing tag with nothing open *might* mean the opening
					// tag was prefilled into the prompt — or it might just be a
					// model writing about think tags, which in a harness whose
					// own source is full of them is the common case, not the
					// exotic one.
					//
					// This used to resolve that ambiguity by guessing prefill
					// and retroactively moving everything before the tag into
					// reasoning. When the guess was wrong it deleted the answer
					// from the settled message, not merely from the live view:
					// "To strip reasoning you remove everything up to the
					// closing tag </think> in the buffer" came back as " in the
					// buffer." A wrong guess here is unrecoverable and silent.
					//
					// Nothing in the byte stream can tell the two apart, so the
					// stream is no longer asked to. The text stays text, the tag
					// is dropped as framing, and the suspicion is reported
					// instead of acted on — see prefillSuspected, which tells
					// the operator to set AssumeReasoningPrefill if their server
					// really does prefill. Leaking reasoning into a visible
					// answer is a cosmetic fault the operator can see and fix;
					// deleting the answer is neither.
					//
					// Reaching this at all means the tag was outside tool-call
					// markup. One inside a payload is a character the model was
					// asked to write and says nothing about the template, so the
					// protected branch above never gets here.
					f.resolved = true
					f.prefillSuspected = true
				}
				// A stray closing tag once the shape is already known. It is
				// template framing, not content, and emitting it as text is
				// how "</think>" ends up in an answer.
				//
				// The bytes either side of it are about to become adjacent, so
				// a tag-length tail is withheld and re-scanned rather than
				// emitted blind. See seamHold.
				rest := data[closeIdx+closeLen:]
				if wouldFuse(f.visibleTail+data[:closeIdx], rest) {
					// Keep the tag rather than splice a marker into being.
					out = f.emit(out, filteredChunk{text: data[:closeIdx+closeLen]})
					data = rest
					continue
				}
				if closeIdx > 0 {
					out = f.emit(out, filteredChunk{text: data[:closeIdx]})
				}
				data = rest
				continue
			}

			if openIdx >= 0 {
				f.resolved = true
				f.assumed = false
				if openIdx > 0 {
					out = f.emit(out, filteredChunk{text: data[:openIdx]})
				}
				data = data[openIdx+openLen:]
				f.inThink = true
				f.depth = 1
				continue
			}

			if partial := partialSuffix(data, carryTags); partial > 0 {
				if safe := data[:len(data)-partial]; safe != "" {
					out = f.emit(out, filteredChunk{text: safe})
				}
				f.carry = data[len(data)-partial:]
				break
			}

			out = f.emit(out, filteredChunk{text: data})
			break
		}

		toolIdx := strings.Index(data, toolCallOpen)
		closeIdx, closeLen := findFirst(data, closeTags)
		// A nested opening tag deepens the block rather than being ignored.
		// The tag itself is still framing and is dropped, so no think tag ever
		// survives into content, but the matching closing tag now closes the
		// nesting instead of the outer block.
		nestIdx, nestLen := findFirst(data, openTags)

		// A tool-call block opened inside reasoning is protected on the same
		// terms and stays classified as reasoning. A model that thinks about
		// calling a tool writes the whole block out inside its think block, and
		// leaving that unprotected let the payload's own "</think>" close the
		// real block early: the rest of the deliberation, the stray closing
		// tag and the parameter markup all landed in the answer. The considered
		// call is still not dispatched — it is reasoning, and
		// extractFallbackToolCalls only ever sees visible text.
		if toolIdx >= 0 && (nestIdx < 0 || toolIdx < nestIdx) && (closeIdx < 0 || toolIdx < closeIdx) {
			if toolIdx > 0 {
				out = f.emit(out, f.classify(data[:toolIdx]))
			}
			end := toolIdx + len(toolCallOpen)
			out = f.emit(out, f.classify(data[toolIdx:end]))
			data = data[end:]
			f.inToolCall = true
			continue
		}

		if nestIdx >= 0 && (closeIdx < 0 || nestIdx < closeIdx) {
			if nestIdx > 0 {
				out = f.emit(out, filteredChunk{reasoning: data[:nestIdx]})
			}
			data = data[nestIdx+nestLen:]
			f.depth++
			continue
		}
		if closeIdx >= 0 {
			if closeIdx > 0 {
				out = f.emit(out, filteredChunk{reasoning: data[:closeIdx]})
			}
			data = data[closeIdx+closeLen:]
			f.depth--
			if f.depth <= 0 {
				f.depth = 0
				f.inThink = false
				f.assumed = false
			}
			f.resolved = true
			continue
		}
		if partial := partialSuffix(data, carryTags); partial > 0 {
			if safe := data[:len(data)-partial]; safe != "" {
				out = f.emit(out, filteredChunk{reasoning: safe})
			}
			f.carry = data[len(data)-partial:]
			break
		}
		out = f.emit(out, filteredChunk{reasoning: data})
		break
	}
	return out
}

// flush releases whatever is still held when the stream ends. Content inside an
// open block is reasoning even though the model never closed it, which is what
// a response truncated mid-thought looks like.
//
// A stream that ends inside a <tool_call> block is the same shape one step
// along: a response cut off at max_tokens can open the block and never close
// it. The held bytes are released on whichever side the block was opened on and
// are never dropped — they exist nowhere else. Because the marker never closed,
// extractTaggedCalls writes the whole unterminated block back into the text as
// prose and recovers no call from it, which is the rule that has to keep
// holding: half a payload is not a request.
func (f *tagFilter) flush() filteredChunk {
	if f.carry == "" {
		return filteredChunk{}
	}
	pending := f.carry
	f.carry = ""
	return f.classify(pending)
}

type wireChunk struct {
	Choices []struct {
		Delta struct {
			Content *string `json:"content"`
			// Two spellings, one meaning. DeepSeek, vLLM and omlx send
			// "reasoning_content"; ollama sends "reasoning". Reading only the
			// first threw away every reasoning token ollama produced — with
			// llm.local.supports_reasoning on and an effort tier requested and
			// paid for, the thinking never reached the transcript at all — and
			// it left the prefill contradiction in outOfBandReasoning blind on
			// that server, which is the difference between an answer and an
			// empty turn.
			ReasoningContent *string        `json:"reasoning_content"`
			Reasoning        *string        `json:"reasoning"`
			ToolCalls        []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			// CachedTokens is how much of the prompt the server served from its
			// prefix cache. On a local server this is the single most useful
			// number the wire carries: it is the difference between a step that
			// re-prefills the whole conversation and one that does not, and
			// without it a harness cannot tell that it is invalidating the
			// cache on every step.
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	// Timings is not in the OpenAI schema; MLX and llama.cpp both add it, and
	// it is the only throughput signal available to a caller that is not
	// timing the socket itself.
	Timings *struct {
		PredictedPerSecond float64 `json:"predicted_per_second"`
		PromptPerSecond    float64 `json:"prompt_per_second"`
	} `json:"timings"`
	Error *wireError `json:"error"`
}

// wireError is the error field, which is not one shape.
//
// OpenAI documents an object with a message. Local servers do not all agree:
// several send a bare string, and one of them ended a real 10-minute agent turn
// here — the strict struct failed to decode `"error":"..."`, the whole stream
// was abandoned as undecodable, and the work was lost to a difference in how
// the server spelled a message it was only reporting in passing.
//
// So both shapes are accepted, and anything else is kept verbatim rather than
// dropped. An error the harness cannot parse is still an error the operator
// needs to see; discarding it would turn a server-side failure into a silent
// stall.
type wireError struct {
	Message string
	Code    any
}

func (e *wireError) UnmarshalJSON(data []byte) error {
	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		e.Message = asString
		return nil
	}
	var asObject struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	}
	if err := json.Unmarshal(data, &asObject); err == nil {
		e.Message, e.Code = asObject.Message, asObject.Code
		if e.Message == "" {
			// An object with no message field still carries the fault; showing
			// the raw JSON beats reporting an empty error.
			e.Message = string(data)
		}
		return nil
	}
	e.Message = string(data)
	return nil
}

func (s *stream) Next() (llm.Chunk, error) {
	for {
		if len(s.pending) > 0 {
			chunk := s.pending[0]
			s.pending = s.pending[1:]
			return chunk, nil
		}
		if s.failure != nil {
			return llm.Chunk{}, s.failure
		}
		event, err := s.sse.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// A stream that died mid-flight settles as that failure. Without
				// this the reader's error is known only to whoever called Next,
				// and Response answers "called before the stream was exhausted"
				// — which blames the caller for a server that went silent, and
				// sends an operator looking for a bug in the harness.
				s.failure = err
				return llm.Chunk{}, err
			}
			s.done = true
			if fl := s.filter.flush(); fl.text != "" || fl.reasoning != "" {
				if fl.text != "" {
					if s.textIndex < 0 {
						s.textIndex = s.nextIndex
						s.nextIndex++
					}
					s.text.WriteString(fl.text)
					return llm.Chunk{Kind: llm.ChunkText, BlockIndex: s.textIndex, Text: fl.text}, nil
				}
				if fl.reasoning != "" {
					if s.reasonIdx < 0 {
						s.reasonIdx = s.nextIndex
						s.nextIndex++
					}
					s.reasoning.WriteString(fl.reasoning)
					return llm.Chunk{Kind: llm.ChunkReasoning, BlockIndex: s.reasonIdx, Text: fl.reasoning}, nil
				}
			}
			return llm.Chunk{}, err
		}

		var chunk wireChunk
		if err := json.Unmarshal(event.Data, &chunk); err != nil {
			s.failure = fmt.Errorf("%s: undecodable stream chunk: %w", s.name, err)
			return llm.Chunk{}, s.failure
		}
		if chunk.Error != nil {
			s.failure = fmt.Errorf("%s: stream error: %s", s.name, chunk.Error.Message)
			return llm.Chunk{}, s.failure
		}
		if chunk.Usage != nil {
			s.usage.InputTokens = chunk.Usage.PromptTokens
			s.usage.OutputTokens = chunk.Usage.CompletionTokens
			if d := chunk.Usage.PromptTokensDetails; d != nil {
				s.usage.CacheReadTokens = d.CachedTokens
			}
			if d := chunk.Usage.CompletionTokensDetails; d != nil {
				s.usage.ReasoningTokens = d.ReasoningTokens
			}
		}
		if chunk.Timings != nil {
			s.usage.OutputTokensPerSecond = chunk.Timings.PredictedPerSecond
			s.usage.PromptTokensPerSecond = chunk.Timings.PromptPerSecond
		}
		if len(chunk.Choices) == 0 {
			// A usage-only frame, which is what include_usage produces at the
			// end of the stream.
			continue
		}

		choice := chunk.Choices[0]
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.stopReason = mapFinishReason(*choice.FinishReason)
		}

		// Every branch below queues rather than returning, and the loop head
		// drains the queue. One delta may legitimately carry reasoning, text
		// and tool-call fragments together; returning from the first branch
		// that matched dropped the rest of that frame on the floor.
		if reasoning := deltaReasoning(choice.Delta.ReasoningContent, choice.Delta.Reasoning); reasoning != "" {
			// The server is separating reasoning itself, so the content
			// channel is not inside an assumed thinking block. See
			// tagFilter.outOfBandReasoning.
			s.filter.outOfBandReasoning()
			if s.reasonIdx < 0 {
				s.reasonIdx = s.nextIndex
				s.nextIndex++
			}
			s.reasoning.WriteString(reasoning)
			s.pending = append(s.pending, llm.Chunk{
				Kind: llm.ChunkReasoning, BlockIndex: s.reasonIdx,
				Text: reasoning,
			})
		}

		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			filtered := s.filter.feed(*choice.Delta.Content)
			for _, fc := range filtered {
				if fc.reasoning != "" {
					if s.reasonIdx < 0 {
						s.reasonIdx = s.nextIndex
						s.nextIndex++
					}
					s.reasoning.WriteString(fc.reasoning)
					s.pending = append(s.pending, llm.Chunk{
						Kind:       llm.ChunkReasoning,
						BlockIndex: s.reasonIdx,
						Text:       fc.reasoning,
					})
				}
				if fc.text != "" {
					if s.textIndex < 0 {
						s.textIndex = s.nextIndex
						s.nextIndex++
					}
					s.text.WriteString(fc.text)
					s.pending = append(s.pending, llm.Chunk{
						Kind:       llm.ChunkText,
						BlockIndex: s.textIndex,
						Text:       fc.text,
					})
				}
			}
		}

		s.applyToolCalls(choice.Delta.ToolCalls)

		// Checked once per frame rather than per write, and by setting the
		// failure rather than returning it, so the chunks this frame already
		// queued still reach the caller before the refusal does — the loop
		// head drains pending first and reports the failure after.
		if over := s.decodedBytes(); over > MaxDecodedResponseBytes && s.failure == nil {
			s.failure = fmt.Errorf(
				"%s: the response exceeded the %d-byte decode limit (%d bytes and still arriving); "+
					"the server is generating past any max_tokens it was given",
				s.name, MaxDecodedResponseBytes, over)
		}
	}
}

// decodedBytes is everything this stream is holding from the response so far.
//
// "Everything" now includes the per-call bookkeeping, not just the content. It
// counted text, reasoning, argument bytes and the filter's carry, and a stream
// of tool_calls fragments that each carried a fresh id and no argument bytes
// created an accumulator, three map entries and a callOrder slot per fragment
// while adding nothing to the total: 400,000 accumulators and 98 MiB of heap
// with this function returning 0 and the 4MiB cap never firing. The cap is
// documented as counting every field together precisely so the runaway cannot
// pick an uncounted one, and an uncounted one is what this was.
//
// Each open call is charged transport.RetainedAccumulatorBytes for the fixed
// cost of existing, plus the id and name it retains, plus its arguments —
// argBytes and callBytes are those tallies, kept as the stream runs. The same
// rule is applied by the anthropic and gemini decoders.
func (s *stream) decodedBytes() int {
	return s.text.Len() + s.reasoning.Len() + s.argBytes + s.callBytes + len(s.filter.carry)
}

// syntheticCallSeq numbers the tool call ids the harness has to invent.
//
// A local server that streams tool calls without an id leaves the harness to
// supply one, and the obvious construction — the call's position within this
// response — repeats on the very next step, because those counters restart with
// every response. Ids are not decoration: session.Log.DeriveMessages pairs each
// tool result to its call by id and keys the compaction ledger on it, so two
// steps that both produce "call_local_0_0" make the projection replay one
// step's output as the answer to both. Measured before this counter existed:
// twelve tool results across twelve steps projected as twelve copies of step
// three's output, and the model read the wrong answer twelve times.
//
// The sequence is process-wide rather than per-stream because a turn spans many
// streams and a session spans many turns — per-stream uniqueness is precisely
// the scope that was already wrong. Ids only have to be unique within one
// session log, and the log stores whatever is minted here, so a replay of that
// log reproduces them exactly.
var syntheticCallSeq atomic.Uint64

// synthesizeCallID mints an id for a call the server did not identify.
func synthesizeCallID(kind string) string {
	return fmt.Sprintf("call_%s_%d", kind, syntheticCallSeq.Add(1))
}

// applyToolCalls folds a delta's tool-call fragments into the accumulators and
// returns at most one chunk to surface.
//
// Fragments are keyed by the wire's index, not by position: a provider may
// interleave fragments for two calls, and appending by arrival order would
// concatenate one call's arguments onto another's.
// It queues every chunk rather than returning one. A delta may open two calls
// at once — vLLM and llama.cpp both batch fragments that way — and returning a
// single value meant the second call's start chunk overwrote the first's, so a
// renderer, the event bus and the NDJSON sink each saw one call where two were
// made. The settled Response recovered both from the accumulators, which is why
// this stayed invisible: the message was right and only the stream was wrong.
func (s *stream) applyToolCalls(deltas []wireToolCall) {
	for _, delta := range deltas {
		acc := s.resolveAccumulator(delta)
		if delta.ID != "" {
			s.callBytes += len(delta.ID) - len(acc.id)
			acc.id = delta.ID
			acc.idFromWire = true
		} else if acc.id == "" {
			acc.id = synthesizeCallID("local")
			s.callBytes += len(acc.id)
		}
		if delta.Function.Name != "" {
			s.callBytes += len(delta.Function.Name) - len(acc.name)
			acc.name = delta.Function.Name
		}
		if delta.Function.Arguments != "" {
			acc.args.WriteString(delta.Function.Arguments)
			s.argBytes += len(delta.Function.Arguments)
		}

		switch {
		case !acc.started && acc.name != "":
			acc.started = true
			s.pending = append(s.pending, llm.Chunk{
				Kind: llm.ChunkToolCallStart, BlockIndex: acc.block,
				ToolCallID: llm.CallID(acc.id), ToolName: acc.name,
				ArgumentsRaw: delta.Function.Arguments,
			})
		case delta.Function.Arguments != "":
			s.pending = append(s.pending, llm.Chunk{
				Kind: llm.ChunkToolCallDelta, BlockIndex: acc.block,
				ToolCallID: llm.CallID(acc.id), ToolName: acc.name,
				ArgumentsRaw: delta.Function.Arguments,
			})
		}
	}
}

// resolveAccumulator decides which call a tool-call fragment belongs to.
//
// The wire's index is the primary key and stays so. But "index" is an int, so
// a server that omits the field decodes it as 0 on every fragment of every
// call, and keying on that alone folded two calls into one slot: the
// accumulator kept the LAST id and the LAST name while holding BOTH calls'
// arguments, so the concatenated wreckage was reported as malformed under the
// wrong tool's name — a report blaming "beta" for a payload that was mostly
// alpha's. Two calls in, zero calls out, and one misattributed error.
//
// Whether a server in the wild omits the index is unverified. The field is
// optional in the shape this wire copies, and a harness must not corrupt when
// an optional field is absent, so the id discriminates when one is present:
//
//   - a fragment whose id has been seen belongs to that call, whatever index
//     it claims;
//   - a fragment with no id, or with the id already on the call currently open
//     at its index, continues that call — this is the ordinary case, and it is
//     what keeps a server that sends the id only on the first fragment, or
//     only on a later one, from being split apart;
//   - a fragment naming an id the server already used for a *different* call
//     at this index opens a new one.
//
// A server that omits the index *and* the id is indistinguishable from one
// streaming a single call, and nothing here can recover that.
func (s *stream) resolveAccumulator(delta wireToolCall) *callAccumulator {
	// Lazily built rather than required of the constructor. newStream does
	// build them, but a *stream assembled directly — which several tests do,
	// and which is the shape a future caller will copy — would otherwise
	// panic on a nil map the first time a tool call arrived.
	if s.calls == nil {
		s.calls = map[int]*callAccumulator{}
	}
	if s.callsByID == nil {
		s.callsByID = map[string]*callAccumulator{}
	}
	if s.openAtIndex == nil {
		s.openAtIndex = map[int]*callAccumulator{}
	}

	if delta.ID != "" {
		if acc, ok := s.callsByID[delta.ID]; ok {
			s.openAtIndex[delta.Index] = acc
			return acc
		}
	}
	if acc, ok := s.openAtIndex[delta.Index]; ok {
		if delta.ID == "" || !acc.idFromWire || acc.id == delta.ID {
			if delta.ID != "" {
				s.callsByID[delta.ID] = acc
			}
			return acc
		}
	}

	// A new call. It takes the wire's index as its slot when that slot is
	// free, so ordinary streams key exactly as they always did; otherwise it
	// takes the next free one, which keeps sort.Ints(callOrder) in Response
	// ordering calls by arrival.
	slot := delta.Index
	for {
		if _, taken := s.calls[slot]; !taken {
			break
		}
		slot++
	}
	acc := &callAccumulator{index: slot, block: s.nextIndex}
	// Charged the moment it exists, not when it collects anything. An
	// accumulator that never collects a byte still costs a struct, a builder,
	// an entry in each of the three maps above and a slot in callOrder; see
	// transport.RetainedAccumulatorBytes.
	s.callBytes += transport.RetainedAccumulatorBytes
	s.nextIndex++
	s.calls[slot] = acc
	s.callOrder = append(s.callOrder, slot)
	s.openAtIndex[delta.Index] = acc
	if delta.ID != "" {
		s.callsByID[delta.ID] = acc
	}
	return acc
}

func isIdentChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
}

// schemaOffers reports whether name is one of the tools the request offered.
//
// The offered schemas are the only authority on what exists to be called, and
// this is the check that makes the fallback parser safe. That parser runs on
// every assistant message that carried no structured tool_calls — which is
// every ordinary prose answer, not just a misconfigured server — and it reads
// any JSON object with a "name" key as a call. Unchecked, a fenced
// package.json in an explanation becomes an executable call whose fence is
// deleted from the answer, and a model discussing this harness's own tool
// format names a real tool with real arguments nobody asked for.
//
// This replaced a name check that validated only the *shape* of the string and
// was never called from anywhere. Shape was the wrong question: "rm_rf" is a
// well-formed name for a tool that does not exist. A name that was not offered
// is prose, and is left in the text as prose.
func schemaOffers(tools []llm.ToolSchema, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// replaceWordLiteral rewrites a bare Python literal outside string values.
//
// It walks bytes rather than runes, and asks strings.HasPrefix(text[i:], …)
// rather than strings.HasPrefix(string(runes[i:]), …). The difference is not
// stylistic: converting a rune slice back to a string copies it, so the old
// spelling allocated one copy of the remaining payload per input rune. That
// made this quadratic in both time and memory — 16KB of unquoted content cost
// 2.1 seconds and 427MB, doubling the input quadrupled both, and the input is
// model output arriving from the network. It runs in Response(), after the
// stream ended and the stall watchdog was stopped, so nothing bounded it: a
// truncated Python-repr tool-call payload was a denial of service with a
// straight face.
//
// Bytes are safe here because every character this function decides on —
// the quote marks, the backslash, the ASCII word literals, and the identifier
// characters either side of a match — is ASCII, and no byte of a multi-byte
// UTF-8 sequence can be mistaken for one. Bytes it does not decide on are
// copied through untouched, so multi-byte content survives verbatim.
func replaceWordLiteral(text, oldWord, newWord string) string {
	if oldWord == "" || !strings.Contains(text, oldWord) {
		return text
	}
	var sb strings.Builder
	sb.Grow(len(text))
	inString := false
	var quoteChar byte
	escaped := false
	n := len(text)

	for i := 0; i < n; {
		c := text[i]
		if inString {
			sb.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == quoteChar {
				inString = false
			}
			i++
			continue
		}

		if c == '"' || c == '\'' {
			inString = true
			quoteChar = c
			sb.WriteByte(c)
			i++
			continue
		}

		if c == oldWord[0] && strings.HasPrefix(text[i:], oldWord) {
			wordLen := len(oldWord)
			prevIsWord := i > 0 && isIdentChar(rune(text[i-1]))
			nextIsWord := (i+wordLen < n) && isIdentChar(rune(text[i+wordLen]))
			if !prevIsWord && !nextIsWord {
				sb.WriteString(newWord)
				i += wordLen
				continue
			}
		}

		sb.WriteByte(c)
		i++
	}
	return sb.String()
}

func repairJSONLiterals(s string) string {
	repaired := strings.ReplaceAll(s, ",\n}", "\n}")
	repaired = strings.ReplaceAll(repaired, ", }", "}")
	repaired = strings.ReplaceAll(repaired, ",}", "}")
	repaired = strings.ReplaceAll(repaired, ",\n]", "\n]")
	repaired = strings.ReplaceAll(repaired, ", ]", "]")
	repaired = strings.ReplaceAll(repaired, ",]", "]")

	repaired = replaceWordLiteral(repaired, "True", "true")
	repaired = replaceWordLiteral(repaired, "False", "false")
	repaired = replaceWordLiteral(repaired, "None", "null")
	return repaired
}

func sanitizeJSONArguments(raw string) json.RawMessage {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	// Strip markdown code fences: ```json ... ``` or ``` ... ```
	if strings.HasPrefix(trimmed, "```") {
		lines := strings.Split(trimmed, "\n")
		if len(lines) >= 2 && strings.HasPrefix(lines[0], "```") {
			lines = lines[1:]
			if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
				lines = lines[:len(lines)-1]
			}
			trimmed = strings.TrimSpace(strings.Join(lines, "\n"))
			if json.Valid([]byte(trimmed)) {
				return json.RawMessage(trimmed)
			}
		}
	}
	// Extract substring between first '{' and last '}' or '[' and ']'
	startObj := strings.Index(trimmed, "{")
	endObj := strings.LastIndex(trimmed, "}")
	if startObj >= 0 && endObj > startObj {
		candidate := trimmed[startObj : endObj+1]
		if json.Valid([]byte(candidate)) {
			return json.RawMessage(candidate)
		}
		repaired := repairJSONLiterals(candidate)
		if json.Valid([]byte(repaired)) {
			return json.RawMessage(repaired)
		}
	}

	startArr := strings.Index(trimmed, "[")
	endArr := strings.LastIndex(trimmed, "]")
	if startArr >= 0 && endArr > startArr {
		candidate := trimmed[startArr : endArr+1]
		if json.Valid([]byte(candidate)) {
			return json.RawMessage(candidate)
		}
		repaired := repairJSONLiterals(candidate)
		if json.Valid([]byte(repaired)) {
			return json.RawMessage(repaired)
		}
	}

	repairedDirect := repairJSONLiterals(trimmed)
	if json.Valid([]byte(repairedDirect)) {
		return json.RawMessage(repairedDirect)
	}

	return json.RawMessage(raw)
}

// FallbackFormat names how a tool call was recovered from plain text.
type FallbackFormat string

const (
	// FallbackNone means the server parsed the call itself and delivered it in
	// the tool_calls field, which is the healthy case.
	FallbackNone FallbackFormat = ""
	// FallbackHermes is <tool_call>{"name":…,"arguments":{…}}</tool_call>.
	FallbackHermes FallbackFormat = "hermes-json"
	// FallbackQwenXML is the nested-XML form Qwen3's chat template mandates:
	//
	//	<tool_call>
	//	<function=name>
	//	<parameter=key>
	//	value
	//	</parameter>
	//	</function>
	//	</tool_call>
	FallbackQwenXML FallbackFormat = "qwen-xml"
	// FallbackFencedJSON is a markdown-fenced JSON object naming a tool.
	FallbackFencedJSON FallbackFormat = "fenced-json"
)

// extractFallbackToolCalls recovers tool calls a server left in the text.
//
// This path should never run. Every serving stack worth using parses tool calls
// itself and returns them in tool_calls; when this fires it means the server is
// not configured with a parser for the model it is serving, and the harness is
// compensating. That is worth saying out loud — the caller records the format —
// because a silent compensation hides a misconfigured server, and a silent
// *failure* to compensate is worse: the call surfaces as prose, the loop sees no
// tool calls, and a turn that did nothing reports success.
func extractFallbackToolCalls(rawText string, tools []llm.ToolSchema) (string, []llm.ToolCallBlock, FallbackFormat) {
	trimmed := strings.TrimSpace(rawText)
	if trimmed == "" {
		return rawText, nil, FallbackNone
	}

	if cleaned, calls := extractTaggedCalls(trimmed, tools); len(calls) > 0 {
		format := FallbackHermes
		for _, c := range calls {
			if c.xml {
				format = FallbackQwenXML
				break
			}
		}
		blocks := make([]llm.ToolCallBlock, 0, len(calls))
		for _, c := range calls {
			blocks = append(blocks, llm.ToolCallBlock{
				ID:        llm.CallID(synthesizeCallID("fallback")),
				Name:      c.name,
				Arguments: c.args,
			})
		}
		return cleaned, blocks, format
	}

	if cleaned, calls := extractFencedCalls(trimmed, tools); len(calls) > 0 {
		return cleaned, calls, FallbackFencedJSON
	}

	return rawText, nil, FallbackNone
}

type parsedCall struct {
	name string
	args json.RawMessage
	xml  bool
}

// extractTaggedCalls walks every <tool_call>…</tool_call> block, accepting
// either the JSON body or the nested-XML body. Every block is walked rather
// than only the first, because a model that emits two calls in one message
// means both, and pairing the first opening tag with the last closing one — as
// the previous implementation did for fenced blocks — merges two calls into one
// malformed extraction.
func extractTaggedCalls(data string, tools []llm.ToolSchema) (string, []parsedCall) {
	const open, close = "<tool_call>", "</tool_call>"
	if !strings.Contains(data, open) {
		return data, nil
	}

	var calls []parsedCall
	var clean strings.Builder
	rest := data
	for {
		start := strings.Index(rest, open)
		if start < 0 {
			clean.WriteString(rest)
			break
		}
		clean.WriteString(rest[:start])
		body := rest[start+len(open):]
		end := strings.Index(body, close)
		if end < 0 {
			// Unterminated: a truncated response, not a call. Keep the text as
			// it stands rather than inventing a call from half a payload.
			clean.WriteString(open)
			clean.WriteString(body)
			break
		}
		payload := strings.TrimSpace(body[:end])
		rest = body[end+len(close):]

		if parsed, ok := parseCallPayload(payload, tools); ok {
			calls = append(calls, parsed...)
			continue
		}
		// A block that could not be read is left in the text exactly as the
		// model wrote it. parseCallPayload's contract has always said so, but
		// it only held when *nothing* was recovered, because a rejected
		// payload was written nowhere: with one good call beside one bad one,
		// the bad one vanished from the answer as well as from the calls, so a
		// request the model made neither ran nor was visible to anyone.
		clean.WriteString(open)
		clean.WriteString(body[:end])
		clean.WriteString(close)
	}
	if len(calls) == 0 {
		return data, nil
	}
	return strings.TrimSpace(clean.String()), calls
}

// parseCallPayload reads one tool-call body in either supported spelling.
//
// It returns a slice because the XML spelling permits more than one
// <function=…> block inside a single <tool_call>, and a model that writes two
// means two.
func parseCallPayload(payload string, tools []llm.ToolSchema) ([]parsedCall, bool) {
	var found []parsedCall
	if strings.HasPrefix(payload, "<function=") {
		xml, ok := parseXMLCalls(payload, tools)
		if !ok {
			return nil, false
		}
		found = xml
	} else {
		name, args, ok := parseJSONCall(payload)
		if !ok {
			return nil, false
		}
		found = []parsedCall{{name: name, args: args}}
	}
	// Both spellings meet here, so the offered-set check sits here rather than
	// in each parser. Rejecting leaves the block in the text — the caller
	// writes an unrecognised block back verbatim — so a model that asked for a
	// tool it was not given has its request surface as prose instead of
	// vanishing.
	//
	// One unoffered name rejects the whole block rather than the single call.
	// A block is one thing the model asked for; running half of a two-call
	// block is a silent partial execution, and the model is never told which
	// half it got.
	for _, call := range found {
		if !schemaOffers(tools, call.name) {
			return nil, false
		}
	}
	return found, true
}

func parseJSONCall(payload string) (string, json.RawMessage, bool) {
	var body struct {
		Name       string          `json:"name"`
		Tool       string          `json:"tool"`
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		// Models emit Python literals and trailing commas often enough that
		// refusing them means dropping real calls. Repair, then insist the
		// result is valid JSON — a payload that still will not parse is not a
		// call, and inventing one from it would be worse than reporting none.
		if err := json.Unmarshal([]byte(repairJSONLiterals(payload)), &body); err != nil {
			return "", nil, false
		}
	}
	name := body.Name
	if name == "" {
		name = body.Tool
	}
	if name == "" {
		return "", nil, false
	}
	args := body.Arguments
	if len(args) == 0 {
		args = body.Parameters
	}
	if len(args) == 0 {
		return name, json.RawMessage("{}"), true
	}
	object, ok := argumentsAsObject(sanitizeJSONArguments(string(args)))
	if !ok {
		return "", nil, false
	}
	return name, object, true
}

// argumentsAsObject insists that a recovered call's arguments are a JSON
// object, which is the only shape a tool can be invoked with.
//
// The value used to be passed through verbatim, so a model writing
// {"name":"read_file","arguments":"not an object"} produced a tool call whose
// Arguments was a JSON *string*. Nothing here objected; the failure surfaced
// far away, in whichever consumer unmarshalled a ToolCallBlock into a map,
// with nothing left pointing back at the response that caused it. A call that
// cannot be invoked is not a call, and saying so here leaves the model's text
// intact as prose instead.
//
// The one non-object spelling that is still a call is a JSON-encoded object
// *string*: OpenAI's own wire spells tool arguments that way, and models copy
// what they were trained on. That is unwrapped once — once, not repeatedly, so
// a doubly-encoded payload is refused rather than chased.
func argumentsAsObject(args json.RawMessage) (json.RawMessage, bool) {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" {
		return nil, false
	}
	switch trimmed[0] {
	case '{':
		if json.Valid([]byte(trimmed)) {
			return json.RawMessage(trimmed), true
		}
	case '"':
		var inner string
		if json.Unmarshal([]byte(trimmed), &inner) != nil {
			return nil, false
		}
		unwrapped := strings.TrimSpace(string(sanitizeJSONArguments(inner)))
		if strings.HasPrefix(unwrapped, "{") && json.Valid([]byte(unwrapped)) {
			return json.RawMessage(unwrapped), true
		}
	}
	return nil, false
}

// parseXMLCalls reads the nested-XML body Qwen3's template prescribes.
//
// Parameter values are strings on the wire, and are re-typed only when the text
// is unambiguously a JSON scalar or container. Guessing more than that would
// turn a path like "0755" into a number, and a filename like "null" into
// nothing at all.
//
// Every <function=…>…</function> block in the payload is read, and each one is
// bounded by its own closing tag. The previous shape took the first function
// name and then scanned for <parameter=> to the end of the whole payload,
// straight past the </function> that ended its block — so two calls in one
// <tool_call> merged into one. Not merely lossy: the second tool disappeared
// and *its* arguments were attached to the first, which is how a "target"
// written for delete_file arrived as an extra argument on read_file.
//
// A block that is not well formed rejects the entire payload rather than
// contributing what it managed to read. A <tool_call> is one thing the model
// asked for, and half-executing it — running the complete first call while
// dropping a truncated second — is a silent partial execution the model is
// never told about. Refused, the markup stays in the text as prose and the
// loop sees a turn that made no calls, which is recoverable.
func parseXMLCalls(payload string, tools []llm.ToolSchema) ([]parsedCall, bool) {
	const fOpen, fClose = "<function=", "</function>"

	var calls []parsedCall
	rest := payload
	for {
		start := strings.Index(rest, fOpen)
		if start < 0 {
			break
		}
		rest = rest[start+len(fOpen):]
		nameEnd := strings.Index(rest, ">")
		if nameEnd < 0 {
			return nil, false
		}
		name := strings.TrimSpace(strings.TrimSuffix(rest[:nameEnd], "/"))
		if name == "" {
			return nil, false
		}
		rest = rest[nameEnd+1:]

		// The block's own body, so a following block's parameters cannot be
		// read as this one's. A missing closing tag is tolerated for the last
		// block — some templates omit it — but only when the parameters inside
		// it are themselves complete; an unterminated parameter is what a
		// response cut off at max_tokens looks like, and is refused below.
		body := rest
		if closeAt := strings.Index(rest, fClose); closeAt >= 0 {
			body, rest = rest[:closeAt], rest[closeAt+len(fClose):]
		} else {
			rest = ""
		}
		if strings.Contains(body, fOpen) {
			// A function block opening inside another one is not a spelling
			// any template produces, and guessing which of the two the
			// parameters belong to is exactly the merge this rewrite exists
			// to stop.
			return nil, false
		}

		args, complete := xmlParameters(body, paramTypes(tools, name))
		if !complete {
			return nil, false
		}
		calls = append(calls, parsedCall{name: name, args: args, xml: true})
	}
	if len(calls) == 0 {
		return nil, false
	}
	return calls, true
}

// xmlParameters reads the <parameter=key>value</parameter> pairs of one
// function block.
//
// Finding where a value *ends* is the whole difficulty. The grammar has no
// escaping, so a value is free to contain the literal "</parameter>" — and it
// routinely does, because the values are file contents and this harness's own
// fixtures are full of tool-call markup. Taking the first "</parameter>", as
// this used to, truncated such a value silently: write_file received "prefix "
// where the model wrote "prefix </parameter> suffix", and the short file was
// written to disk with no error raised anywhere.
//
// The value therefore ends at the *last* "</parameter>" before the next
// "<parameter=" (or before the end of the block, for the final parameter).
// That is exact for both shapes that matter: successive parameters still close
// at their own tag, and an embedded closing tag stays inside the value it
// belongs to. It is still a heuristic — a value containing a literal
// "<parameter=" cuts short — but that spelling has no plausible source, where
// XML content does.
//
// The second return reports that every parameter it opened was also closed. A
// parameter left open is a response truncated mid-value, and the caller refuses
// the whole call rather than dispatching a tool with the arguments that
// happened to arrive first.
func xmlParameters(body string, types map[string]string) (json.RawMessage, bool) {
	const pOpen, pClose = "<parameter=", "</parameter>"

	args := map[string]json.RawMessage{}
	rest := body
	for {
		start := strings.Index(rest, pOpen)
		if start < 0 {
			break
		}
		rest = rest[start+len(pOpen):]
		keyEnd := strings.Index(rest, ">")
		if keyEnd < 0 {
			return nil, false
		}
		key := strings.TrimSpace(rest[:keyEnd])
		rest = rest[keyEnd+1:]

		region := rest
		if next := strings.Index(rest, pOpen); next >= 0 {
			region = rest[:next]
		}
		end := strings.LastIndex(region, pClose)
		if end < 0 {
			return nil, false
		}
		// The template puts the value on its own lines; the surrounding
		// newlines are framing, not content.
		value := strings.Trim(rest[:end], "\n")
		rest = rest[end+len(pClose):]
		if key == "" {
			continue
		}
		args[key] = typedParam(value, types[key])
	}

	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// paramTypes maps a tool's parameter names to their declared JSON Schema type.
//
// The declared schema is the only authority on what a value means. Guessing
// from the text cannot work: "0755" is a file mode, "1.10" is a version, and
// "null" is a plausible filename, yet each reads as a number or a literal. The
// nested-XML form the template prescribes carries no types at all, so without
// the schema every one of those is a coin flip that produces a wrong value
// which looks like a right one.
func paramTypes(tools []llm.ToolSchema, toolName string) map[string]string {
	for _, t := range tools {
		if t.Name != toolName {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Type any `json:"type"`
			} `json:"properties"`
		}
		if json.Unmarshal(t.InputSchema, &schema) != nil {
			return nil
		}
		out := make(map[string]string, len(schema.Properties))
		for name, prop := range schema.Properties {
			switch v := prop.Type.(type) {
			case string:
				out[name] = v
			case []any:
				// A union such as ["string","null"]: take the first concrete
				// type, which is what a caller would most likely mean.
				for _, entry := range v {
					if str, ok := entry.(string); ok && str != "null" {
						out[name] = str
						break
					}
				}
			}
		}
		return out
	}
	return nil
}

// typedParam encodes one XML parameter value according to its declared type.
//
// An undeclared parameter stays a string. That is the conservative answer: a
// string reaching a schema that wanted a number is a validation error the model
// is told about and can correct, whereas a number reaching a schema that wanted
// a string is accepted and silently wrong.
func typedParam(value, declared string) json.RawMessage {
	trimmed := strings.TrimSpace(value)
	asString := func() json.RawMessage {
		encoded, err := json.Marshal(value)
		if err != nil {
			return json.RawMessage(`""`)
		}
		return encoded
	}

	switch declared {
	case "number", "integer":
		if trimmed == "" {
			return asString()
		}
		var n json.Number
		if json.Unmarshal([]byte(trimmed), &n) == nil {
			return json.RawMessage(trimmed)
		}
		return asString()
	case "boolean":
		switch strings.ToLower(trimmed) {
		case "true":
			return json.RawMessage("true")
		case "false":
			return json.RawMessage("false")
		}
		return asString()
	case "object", "array":
		// json.Valid is not the question. It is true of every scalar, so a
		// parameter declared "object" whose text was "123" came back as the
		// number 123, and one declared "array" whose text was "true" came back
		// as the boolean true — schema-driven coercion emitting precisely the
		// kind the schema said it was not. The opening byte is what separates a
		// container from a scalar, and it has to agree with the declaration
		// before the text is passed through as JSON.
		open := byte('{')
		if declared == "array" {
			open = '['
		}
		if trimmed != "" && trimmed[0] == open && json.Valid([]byte(trimmed)) {
			return json.RawMessage(trimmed)
		}
		return asString()
	default:
		return asString()
	}
}

// extractFencedCalls reads a markdown-fenced JSON object that names a tool.
//
// Each fence is considered on its own, and only a fence naming an offered tool
// becomes a call. An earlier version required a "devcouncil_" prefix here while
// accepting anything in the tagged path, so the same call was recovered or
// dropped depending on how the model happened to wrap it; the offered set is
// now the single authority for both spellings.
func extractFencedCalls(data string, tools []llm.ToolSchema) (string, []llm.ToolCallBlock) {
	var calls []llm.ToolCallBlock
	var clean strings.Builder
	rest := data
	idx := 0
	for {
		start := strings.Index(rest, "```")
		if start < 0 {
			clean.WriteString(rest)
			break
		}
		body := rest[start+3:]
		end := strings.Index(body, "```")
		if end < 0 {
			clean.WriteString(rest)
			break
		}
		inner := strings.TrimSpace(body[:end])
		after := body[end+3:]
		inner = strings.TrimPrefix(inner, "json")
		inner = strings.TrimSpace(inner)

		if name, args, ok := parseJSONCall(inner); ok && schemaOffers(tools, name) {
			clean.WriteString(rest[:start])
			calls = append(calls, llm.ToolCallBlock{
				ID:        llm.CallID(synthesizeCallID("fallback")),
				Name:      name,
				Arguments: args,
			})
			idx++
		} else {
			clean.WriteString(rest[:start+3+end+3])
		}
		rest = after
	}
	if len(calls) == 0 {
		return data, nil
	}
	return strings.TrimSpace(clean.String()), calls
}

func (s *stream) Response() (llm.Response, error) {
	if s.failure != nil {
		return llm.Response{}, s.failure
	}
	if !s.done {
		return llm.Response{}, fmt.Errorf("%s: Response called before the stream was exhausted", s.name)
	}
	if s.settled != nil {
		return *s.settled, nil
	}

	msg := llm.Message{
		Role:       llm.RoleAssistant,
		Provenance: &llm.AssistantProvenance{Provider: s.name, Model: s.model},
	}
	if s.reasoning.Len() > 0 {
		// Carried with no signature: this wire documents no way to replay it,
		// so PrepareHistory will strip it on the next turn and report the drop.
		// Recording it here keeps the session log complete.
		msg.Content = append(msg.Content, llm.ReasoningBlock{Text: s.reasoning.String()})
	}

	var visibleText = s.text.String()
	var fallbackCalls []llm.ToolCallBlock

	// If no wire tool calls were accumulated, inspect the visible text for fallback tool calls
	if len(s.callOrder) == 0 && visibleText != "" {
		cleaned, extracted, format := extractFallbackToolCalls(visibleText, s.tools)
		if len(extracted) > 0 {
			visibleText = cleaned
			fallbackCalls = extracted
			s.fallbackFormat = format
		}
	}

	if visibleText != "" {
		msg.Content = append(msg.Content, llm.TextBlock{Text: visibleText})
	}

	// A call whose arguments did not survive the stream is reported as a
	// malformed call rather than as a failed request.
	//
	// The difference matters most on a local server, which truncates far more
	// readily than a hosted one — mlx-vlm stops at 2048 output tokens by
	// default. Returning an error here ended the whole turn on a response that
	// merely ran out of room, discarding every completed step with it. Carried
	// through as a malformed call, the loop hands the model an error result it
	// can act on: shorten the arguments, or make one call instead of three.
	sort.Ints(s.callOrder)
	for _, index := range s.callOrder {
		acc := s.calls[index]
		if acc.id == "" {
			acc.id = synthesizeCallID("local")
		}
		if acc.name == "" {
			// No name means nothing was asked for: there is no call here to
			// recover, and inventing one would run a tool at random.
			s.malformed = append(s.malformed, MalformedCall{
				ID:     llm.CallID(acc.id),
				Reason: "the stream carried arguments for a tool call that never named a function",
			})
			continue
		}
		args := sanitizeJSONArguments(acc.args.String())
		if !json.Valid(args) {
			s.malformed = append(s.malformed, MalformedCall{
				ID:   llm.CallID(acc.id),
				Name: acc.name,
				Reason: fmt.Sprintf(
					"the arguments were cut off mid-value and are not valid JSON (%s)",
					truncateForMessage(acc.args.String())),
			})
			continue
		}
		msg.Content = append(msg.Content, llm.ToolCallBlock{
			ID: llm.CallID(acc.id), Name: acc.name, Arguments: args,
		})
	}

	for _, fb := range fallbackCalls {
		msg.Content = append(msg.Content, fb)
	}

	stop := s.stopReason
	if stop == "" {
		stop = llm.StopOther
	}
	// A finish reason of "stop" or "other" alongside tool calls means the turn ends with
	// tools pending; the neutral vocabulary distinguishes those, and the loop
	// branches on it.
	totalCalls := len(s.callOrder) - len(s.malformed) + len(fallbackCalls)
	if totalCalls > 0 && (stop == llm.StopEndTurn || stop == llm.StopOther) {
		stop = llm.StopToolUse
	}
	malformed := make([]llm.MalformedCall, 0, len(s.malformed))
	for _, m := range s.malformed {
		malformed = append(malformed, llm.MalformedCall{ID: m.ID, Name: m.Name, Reason: m.Reason})
	}
	s.settled = &llm.Response{
		Message:          msg,
		StopReason:       stop,
		Usage:            s.usage,
		Malformed:        malformed,
		MaxTokensApplied: s.maxTokensApplied,
		Decoding: llm.DecodingReport{
			FallbackFormat:        string(s.fallbackFormat),
			ReasoningReclassified: s.filter.prefillSuspected,
			PrefillDisproved:      s.filter.prefillDisproved,
		},
	}
	return *s.settled, nil
}

func (s *stream) Close() error { return s.sse.Close() }

func mapFinishReason(raw string) llm.StopReason {
	switch raw {
	case "stop", "end_turn":
		return llm.StopEndTurn
	case "tool_calls":
		return llm.StopToolUse
	case "length":
		return llm.StopMaxTokens
	case "content_filter":
		return llm.StopRefusal
	default:
		return llm.StopOther
	}
}

// MalformedCall is a tool call the stream could not reconstruct.
type MalformedCall struct {
	ID     llm.CallID
	Name   string
	Reason string
}

// truncateForMessage bounds a fragment quoted back to the model.
//
// The cut is moved back to a rune boundary. Slicing at a byte offset split
// multi-byte UTF-8 down the middle and put the orphaned continuation bytes into
// MalformedCall.Reason, which is both quoted back to the model and written to
// the session log — measured producing "aaa…\xe6\x97…". Nothing rejected it:
// json.Marshal substitutes U+FFFD, so the record was corrupted quietly rather
// than refused, and the model was handed bytes that are not text.
func truncateForMessage(s string) string {
	const limit = 120
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
