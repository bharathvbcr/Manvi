package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"manvi/credentials"
	"manvi/llm"
	"manvi/llm/transport"
)

// DefaultBaseURL is the documented API root.
const DefaultBaseURL = "https://api.anthropic.com/v1"

// MessagesPath is the content-generation endpoint.
const MessagesPath = "/messages"

// APIKeyHeader and VersionHeader are the documented authentication and
// versioning headers. The version is required on every request and pinned here
// rather than left to a default, because "whatever the server currently
// defaults to" is not a contract this harness can test against.
const (
	APIKeyHeader  = "x-api-key"
	VersionHeader = "anthropic-version"
	APIVersion    = "2023-06-01"
)

// SSE event names.
const (
	EventMessageStart      = "message_start"
	EventContentBlockStart = "content_block_start"
	EventContentBlockDelta = "content_block_delta"
	EventContentBlockStop  = "content_block_stop"
	EventMessageDelta      = "message_delta"
	EventMessageStop       = "message_stop"
	EventPing              = "ping"
	EventError             = "error"
)

// Content block and delta type discriminators.
const (
	BlockText             = "text"
	BlockThinking         = "thinking"
	BlockRedactedThinking = "redacted_thinking"
	BlockToolUse          = "tool_use"

	DeltaText      = "text_delta"
	DeltaThinking  = "thinking_delta"
	DeltaSignature = "signature_delta"
	DeltaInputJSON = "input_json_delta"
)

// Adapter speaks the Messages API.
type Adapter struct {
	client *transport.Client
	// DefaultMaxTokens bounds a request that did not set its own. The API
	// requires max_tokens, so there is no "unset" to pass through; a default
	// is the only alternative to refusing the request.
	DefaultMaxTokens int
	// Thinking is the reasoning configuration this adapter sends. It is an
	// adapter-level setting rather than a per-request one because what omitting
	// the parameter *means* differs by model — see thinkingOnByDefault — and a
	// harness that leaves it unstated inherits a different behaviour when the
	// model changes underneath it.
	Thinking ThinkingMode
}

// New builds an adapter. The credential is read through a function on every
// request rather than captured, so a rotated key takes effect without a
// restart and so the value is never stored on this struct.
func New(baseURL string, resolve func() (credentials.Secret, error)) *Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	client := transport.New(Name, baseURL, func() (http.Header, error) {
		secret, err := resolve()
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		h.Set(APIKeyHeader, secret.Reveal())
		h.Set(VersionHeader, APIVersion)
		return h, nil
	})
	client.RequestIDHeader = "request-id"
	return &Adapter{client: client, DefaultMaxTokens: 8192, Thinking: ThinkingAdaptive}
}

// Name identifies the adapter.
func (a *Adapter) Name() string { return Name }

// Capability describes a model this adapter serves.
func (a *Adapter) Capability(model string) (llm.Capability, bool) { return Capability(model) }

// wire request types.
type wireRequest struct {
	Model        string          `json:"model"`
	Messages     []wireMessage   `json:"messages"`
	MaxTokens    int             `json:"max_tokens"`
	System       string          `json:"system,omitempty"`
	Tools        []wireTool      `json:"tools,omitempty"`
	Stream       bool            `json:"stream"`
	OutputConfig *wireOutputConf `json:"output_config,omitempty"`
	Thinking     *wireThinking   `json:"thinking,omitempty"`
}

type wireOutputConf struct {
	Effort string `json:"effort"`
}

type wireThinking struct {
	Type string `json:"type"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

type wireBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string      `json:"tool_use_id,omitempty"`
	Content   []wireBlock `json:"content,omitempty"`
	IsError   bool        `json:"is_error,omitempty"`

	// image
	Source *wireSource `json:"source,omitempty"`
}

type wireSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// Stream begins a model call.
func (a *Adapter) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	if err := ValidateRequest(req, a.Thinking); err != nil {
		return nil, err
	}
	body, err := a.buildRequest(req)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Post(ctx, MessagesPath, body)
	if err != nil {
		return nil, err
	}
	return newStream(resp.Body, req.Model, a.appliedMaxTokens(req)), nil
}

// appliedMaxTokens is the bound this adapter will actually send. Kept apart
// from Capability.MaxOutputTokens, which is the model's ceiling: this adapter
// declares 128000 there and sends 8192, so a caller comparing a response
// against the ceiling would never see a truncation.
func (a *Adapter) appliedMaxTokens(req llm.Request) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return a.DefaultMaxTokens
}

func (a *Adapter) buildRequest(req llm.Request) (*wireRequest, error) {
	maxTokens := a.appliedMaxTokens(req)
	out := &wireRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		System:    req.System,
		Stream:    true,
	}
	// Temperature is deliberately absent from the wire type. Current models
	// reject the sampling parameters outright, and ValidateRequest refuses a
	// request that sets one — a field that can never be populated is worse than
	// no field, because it reads as support.
	switch a.Thinking {
	case ThinkingAdaptive:
		out.Thinking = &wireThinking{Type: "adaptive"}
	case ThinkingDisabled:
		out.Thinking = &wireThinking{Type: "disabled"}
	case ThinkingDefault:
		// Omitted on purpose; the model's own default applies.
	default:
		return nil, fmt.Errorf("anthropic: unknown thinking mode %q", a.Thinking)
	}
	if req.Effort != "" {
		out.OutputConfig = &wireOutputConf{Effort: req.Effort}
	}
	for _, tool := range req.Tools {
		out.Tools = append(out.Tools, wireTool{
			Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema,
		})
	}
	for i, msg := range req.Messages {
		converted, err := toWireMessage(msg)
		if err != nil {
			return nil, fmt.Errorf("anthropic: message %d: %w", i, err)
		}
		out.Messages = append(out.Messages, converted)
	}
	return out, nil
}

func toWireMessage(msg llm.Message) (wireMessage, error) {
	// The API takes the system prompt as a top-level field, not as a role. A
	// system message reaching here means the caller put it in the wrong place,
	// and silently folding it into the user turn would change the prompt.
	if msg.Role == llm.RoleSystem {
		return wireMessage{}, errors.New("system content belongs in Request.System, not in Messages")
	}
	out := wireMessage{Role: string(msg.Role)}
	for _, block := range msg.Content {
		converted, err := toWireBlock(block)
		if err != nil {
			return wireMessage{}, err
		}
		out.Content = append(out.Content, converted)
	}
	return out, nil
}

func toWireBlock(block llm.ContentBlock) (wireBlock, error) {
	switch b := block.(type) {
	case llm.TextBlock:
		return wireBlock{Type: BlockText, Text: b.Text}, nil
	case llm.ReasoningBlock:
		if b.Redacted {
			return wireBlock{Type: BlockRedactedThinking, Data: b.Signature}, nil
		}
		return wireBlock{Type: BlockThinking, Thinking: b.Text, Signature: b.Signature}, nil
	case llm.ImageBlock:
		return wireBlock{Type: "image", Source: &wireSource{
			Type: "base64", MediaType: b.MediaType, Data: encodeBase64(b.Data),
		}}, nil
	case llm.ToolCallBlock:
		input := b.Arguments
		if len(input) == 0 {
			// An absent argument object is not the same as no argument object:
			// the field is required, and omitting it produces a 400 that reads
			// as a schema problem with the tool.
			input = json.RawMessage("{}")
		}
		return wireBlock{Type: BlockToolUse, ID: string(b.ID), Name: b.Name, Input: input}, nil
	case llm.ToolResultBlock:
		out := wireBlock{Type: "tool_result", ToolUseID: string(b.ToolCallID), IsError: b.IsError}
		for _, inner := range b.Content {
			converted, err := toWireBlock(inner)
			if err != nil {
				return wireBlock{}, err
			}
			out.Content = append(out.Content, converted)
		}
		return out, nil
	default:
		return wireBlock{}, fmt.Errorf("anthropic: cannot send a %s block", block.Kind())
	}
}

// stream decodes the SSE event sequence into neutral chunks.
type stream struct {
	sse   *transport.SSE
	model string
	// maxTokensApplied is the output bound this request carried, carried back
	// so a caller can tell a response that ran to its budget from one the
	// server merely labelled a normal stop. See llm.Response.MaxTokensApplied.
	maxTokensApplied int

	// blocks accumulates content as it arrives, indexed by the API's block
	// index. The settled Response is assembled from these, so nothing depends
	// on the caller having consumed every chunk.
	blocks map[int]*accumulator
	order  []int

	stopReason llm.StopReason
	usage      llm.Usage
	done       bool
	failure    error
}

type accumulator struct {
	kind      string
	text      strings.Builder
	signature string
	data      string
	toolID    string
	toolName  string
	args      strings.Builder
}

func newStream(body io.ReadCloser, model string, maxTokensApplied int) *stream {
	return &stream{
		// No [DONE] sentinel: this API ends the stream with message_stop and
		// then closes.
		sse:    transport.NewSSEWithStall(body, "", transport.DefaultHostedStallTimeout),
		model:  model,
		blocks: map[int]*accumulator{},

		maxTokensApplied: maxTokensApplied,
	}
}

type wireEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	Message *struct {
		StopReason string    `json:"stop_reason"`
		Usage      wireUsage `json:"usage"`
	} `json:"message"`

	ContentBlock *wireBlock `json:"content_block"`

	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`

	Usage *wireUsage `json:"usage"`

	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokensDetails      *struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

func (s *stream) Next() (llm.Chunk, error) {
	for {
		if s.failure != nil {
			return llm.Chunk{}, s.failure
		}
		event, err := s.sse.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.done = true
			}
			return llm.Chunk{}, err
		}

		var ev wireEvent
		if err := json.Unmarshal(event.Data, &ev); err != nil {
			s.failure = fmt.Errorf("anthropic: undecodable %s event: %w", event.Name, err)
			return llm.Chunk{}, s.failure
		}
		// The event name and the payload's own type field must agree. When they
		// do not, the stream is not what this decoder was written against, and
		// guessing which to believe is how a decoder silently drops content.
		if event.Name != "" && ev.Type != "" && event.Name != ev.Type {
			s.failure = fmt.Errorf("anthropic: event name %q disagrees with payload type %q",
				event.Name, ev.Type)
			return llm.Chunk{}, s.failure
		}

		chunk, emit, err := s.apply(ev)
		if err != nil {
			s.failure = err
			return llm.Chunk{}, err
		}
		if emit {
			return chunk, nil
		}
	}
}

func (s *stream) apply(ev wireEvent) (llm.Chunk, bool, error) {
	switch ev.Type {
	case EventError:
		message := "unspecified"
		if ev.Error != nil {
			message = ev.Error.Type + ": " + ev.Error.Message
		}
		return llm.Chunk{}, false, fmt.Errorf("anthropic: stream error: %s", message)

	case EventMessageStart:
		if ev.Message != nil {
			s.usage = mergeUsage(s.usage, ev.Message.Usage)
		}
		return llm.Chunk{}, false, nil

	case EventContentBlockStart:
		if ev.ContentBlock == nil {
			return llm.Chunk{}, false, errors.New("anthropic: content_block_start without a block")
		}
		acc := &accumulator{kind: ev.ContentBlock.Type}
		s.blocks[ev.Index] = acc
		s.order = append(s.order, ev.Index)
		switch ev.ContentBlock.Type {
		case BlockToolUse:
			acc.toolID = ev.ContentBlock.ID
			acc.toolName = ev.ContentBlock.Name
			return llm.Chunk{
				Kind: llm.ChunkToolCallStart, BlockIndex: ev.Index,
				ToolCallID: llm.CallID(acc.toolID), ToolName: acc.toolName,
			}, true, nil
		case BlockText:
			if ev.ContentBlock.Text != "" {
				acc.text.WriteString(ev.ContentBlock.Text)
				return llm.Chunk{Kind: llm.ChunkText, BlockIndex: ev.Index, Text: ev.ContentBlock.Text}, true, nil
			}
		case BlockRedactedThinking:
			acc.data = ev.ContentBlock.Data
		}
		return llm.Chunk{}, false, nil

	case EventContentBlockDelta:
		acc := s.blocks[ev.Index]
		if acc == nil || ev.Delta == nil {
			// A delta for a block that never started means events were lost.
			// Continuing would assemble a message with a hole in it.
			return llm.Chunk{}, false, fmt.Errorf(
				"anthropic: delta for unstarted block %d; the stream is missing events", ev.Index)
		}
		switch ev.Delta.Type {
		case DeltaText:
			acc.text.WriteString(ev.Delta.Text)
			return llm.Chunk{Kind: llm.ChunkText, BlockIndex: ev.Index, Text: ev.Delta.Text}, true, nil
		case DeltaThinking:
			acc.text.WriteString(ev.Delta.Thinking)
			return llm.Chunk{Kind: llm.ChunkReasoning, BlockIndex: ev.Index, Text: ev.Delta.Thinking}, true, nil
		case DeltaSignature:
			// The signature is what makes a thinking block replayable. It is
			// accumulated but not surfaced as a chunk: it is not content.
			acc.signature += ev.Delta.Signature
			return llm.Chunk{}, false, nil
		case DeltaInputJSON:
			acc.args.WriteString(ev.Delta.PartialJSON)
			return llm.Chunk{
				Kind: llm.ChunkToolCallDelta, BlockIndex: ev.Index,
				ToolCallID: llm.CallID(acc.toolID), ToolName: acc.toolName,
				ArgumentsRaw: ev.Delta.PartialJSON,
			}, true, nil
		default:
			// An unrecognised delta type is content this build cannot
			// represent. Dropping it silently would produce a truncated
			// message that looks complete.
			return llm.Chunk{}, false, fmt.Errorf("anthropic: unknown delta type %q", ev.Delta.Type)
		}

	case EventMessageDelta:
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			s.stopReason = mapStopReason(ev.Delta.StopReason)
		}
		if ev.Usage != nil {
			s.usage = mergeUsage(s.usage, *ev.Usage)
		}
		return llm.Chunk{}, false, nil

	case EventContentBlockStop, EventMessageStop, EventPing:
		return llm.Chunk{}, false, nil

	default:
		// Unknown *event* types are ignored rather than fatal: the API adds
		// them, and an event carrying no content this decoder needs is not a
		// reason to fail a turn. Unknown deltas above are different — those
		// carry content.
		return llm.Chunk{}, false, nil
	}
}

// Response assembles the settled message.
func (s *stream) Response() (llm.Response, error) {
	if s.failure != nil {
		return llm.Response{}, s.failure
	}
	if !s.done {
		return llm.Response{}, errors.New("anthropic: Response called before the stream was exhausted")
	}

	msg := llm.Message{
		Role:       llm.RoleAssistant,
		Provenance: &llm.AssistantProvenance{Provider: Name, Model: s.model},
	}
	for _, index := range s.order {
		acc := s.blocks[index]
		switch acc.kind {
		case BlockText:
			msg.Content = append(msg.Content, llm.TextBlock{Text: acc.text.String()})
		case BlockThinking:
			msg.Content = append(msg.Content, llm.ReasoningBlock{
				Text: acc.text.String(), Signature: acc.signature,
			})
		case BlockRedactedThinking:
			msg.Content = append(msg.Content, llm.ReasoningBlock{Redacted: true, Signature: acc.data})
		case BlockToolUse:
			args := json.RawMessage(acc.args.String())
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			if !json.Valid(args) {
				// A tool call whose arguments did not arrive whole must not be
				// handed to the tool layer. Truncated JSON that happens to
				// parse is the failure this guards against.
				return llm.Response{}, fmt.Errorf(
					"anthropic: tool call %q arrived with incomplete arguments: %q", acc.toolName, args)
			}
			msg.Content = append(msg.Content, llm.ToolCallBlock{
				ID: llm.CallID(acc.toolID), Name: acc.toolName, Arguments: args,
			})
		}
	}

	stop := s.stopReason
	if stop == "" {
		stop = llm.StopOther
	}
	return llm.Response{Message: msg, StopReason: stop, Usage: s.usage, MaxTokensApplied: s.maxTokensApplied}, nil
}

// Close releases the stream.
func (s *stream) Close() error { return s.sse.Close() }

func mapStopReason(raw string) llm.StopReason {
	switch raw {
	case "end_turn", "stop_sequence":
		return llm.StopEndTurn
	case "tool_use":
		return llm.StopToolUse
	case "max_tokens", "model_context_window_exceeded":
		return llm.StopMaxTokens
	case "refusal":
		return llm.StopRefusal
	default:
		return llm.StopOther
	}
}

func mergeUsage(into llm.Usage, w wireUsage) llm.Usage {
	// message_start carries input counts and message_delta carries the final
	// output count, so the two are merged rather than replaced. Taking the
	// later event wholesale would zero the input tokens.
	if w.InputTokens > 0 {
		into.InputTokens = w.InputTokens
	}
	if w.OutputTokens > 0 {
		into.OutputTokens = w.OutputTokens
	}
	if w.CacheReadInputTokens > 0 {
		into.CacheReadTokens = w.CacheReadInputTokens
	}
	if w.CacheCreationInputTokens > 0 {
		into.CacheWriteTokens = w.CacheCreationInputTokens
	}
	if w.OutputTokensDetails != nil && w.OutputTokensDetails.ThinkingTokens > 0 {
		into.ReasoningTokens = w.OutputTokensDetails.ThinkingTokens
	}
	return into
}

// ReplayableOn answers the ReasoningReplayer question for this adapter.
// Anthropic signs thinking blocks per model, so replay is same-model only.
func (a *Adapter) ReplayableOn(fromModel, toModel string) bool {
	return ReplayableOn(fromModel, toModel)
}
