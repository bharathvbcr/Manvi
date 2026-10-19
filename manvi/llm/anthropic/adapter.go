package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// DefaultBaseURL is the documented API host root. ANTHROPIC_BASE_URL means the
// same thing to the vendor SDK and to Claude Code: the host, with the version
// carried by the request path.
const DefaultBaseURL = "https://api.anthropic.com"

// MessagesPath is the content-generation endpoint, relative to the host root.
const MessagesPath = "/v1/messages"

// apiVersionSegment is the path segment MessagesPath begins with.
const apiVersionSegment = "/v1"

// hostRoot reads a configured base URL as the host root. A value that already
// ends in the version segment is what configs written when Manvi read the base
// as including /v1 still hold; dropping that one segment keeps them working
// instead of requesting /v1/v1/messages.
func hostRoot(baseURL string) string {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if root == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(strings.TrimSuffix(root, apiVersionSegment), "/")
}

// APIKeyHeader and VersionHeader are the documented authentication and
// versioning headers. The version is required on every request and pinned here
// rather than left to a default, because "whatever the server currently
// defaults to" is not a contract this harness can test against.
const (
	APIKeyHeader  = "x-api-key"
	VersionHeader = "anthropic-version"
	APIVersion    = "2023-06-01"
)

// WorkspaceHeader names the workspace a request acts in. A key created for one
// workspace runs there when the header is omitted; a key that is not scoped to
// one (a multi-workspace personal or service-account key) is refused with a
// 400 invalid_request_error on every request that omits it. A header naming a
// workspace the key cannot reach — including one in a different organisation
// from the key's — is a 404 not_found_error, "Workspace `<id>` not found."
// (platform.claude.com/docs/en/manage-claude/authentication#select-a-workspace,
// read 2026-10-19.) So the header belongs with the key it was issued beside,
// never with whichever key happens to be in the environment.
const WorkspaceHeader = "anthropic-workspace-id"

// maxWorkspaceIDLen bounds a configured workspace id. Real ids are around
// thirty characters; the bound exists so a pasted blob is refused rather than
// sent as a header.
const maxWorkspaceIDLen = 128

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
	// the parameter *means* differs by model — see ThinkingDefault — and a
	// harness that leaves it unstated inherits a different behaviour when the
	// model changes underneath it.
	Thinking ThinkingMode

	// baseURL is kept so a fallback client is built against the same host.
	baseURL string
	// fallback is a second key, in its own organisation and workspace, that a
	// request is moved to when the primary's organisation is out of credit.
	// Nil when none is configured. See SetFallback.
	fallback *transport.Client
	// failedOver is set the first time the primary is refused for credit, and
	// never cleared: prepaid credit does not come back mid-session, so asking
	// the primary again would add one refused request to every turn.
	failedOver atomic.Bool
	onFailover func()
}

// New builds an adapter. The credential is read through a function on every
// request rather than captured, so a rotated key takes effect without a
// restart and so the value is never stored on this struct.
func New(baseURL string, resolve func() (credentials.Secret, error)) *Adapter {
	return newAdapter(baseURL, "", resolve)
}

// NewForWorkspace builds an adapter whose every request names workspace. An
// empty or blank workspace means none, which is right for a key created inside
// a workspace. Anything but an identifier is refused here, because the value
// becomes a header and a fault found now is cheaper than a 400 on the first
// turn.
func NewForWorkspace(baseURL, workspace string, resolve func() (credentials.Secret, error)) (*Adapter, error) {
	workspace, err := ParseWorkspaceID(workspace)
	if err != nil {
		return nil, err
	}
	return newAdapter(baseURL, workspace, resolve), nil
}

// ParseWorkspaceID trims a configured workspace id and refuses anything that
// is not an identifier. Blank is not an error: it means no workspace.
func ParseWorkspaceID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if len(id) > maxWorkspaceIDLen {
		return "", fmt.Errorf("workspace id is %d characters; a workspace id is at most %d", len(id), maxWorkspaceIDLen)
	}
	for _, r := range id {
		if !workspaceIDRune(r) {
			return "", fmt.Errorf("workspace id contains %q; a workspace id is letters, digits, '_' and '-' (e.g. wrkspc_01...)", r)
		}
	}
	return id, nil
}

// workspaceIDRune is the alphabet a workspace id is drawn from.
func workspaceIDRune(r rune) bool {
	return r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

func newAdapter(baseURL, workspace string, resolve func() (credentials.Secret, error)) *Adapter {
	return &Adapter{client: newClient(baseURL, workspace, resolve), DefaultMaxTokens: 8192, Thinking: ThinkingAdaptive, baseURL: baseURL}
}

// newClient builds the transport for one key and the workspace it acts in.
func newClient(baseURL, workspace string, resolve func() (credentials.Secret, error)) *transport.Client {
	client := transport.New(Name, hostRoot(baseURL), func() (http.Header, error) {
		secret, err := resolve()
		if err != nil {
			return nil, err
		}
		key, err := secret.Reveal()
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		h.Set(APIKeyHeader, key)
		h.Set(VersionHeader, APIVersion)
		if workspace != "" {
			h.Set(WorkspaceHeader, workspace)
		}
		return h, nil
	})
	client.RequestIDHeader = "request-id"
	return client
}

// Fallback is a second key a request moves to when the primary key's
// organisation has run out of prepaid credit.
type Fallback struct {
	// Workspace is the workspace the fallback key acts in, blank for a key
	// created inside one. It is the fallback's own: the primary's workspace
	// belongs to the primary's organisation and is a 404 under any other key.
	Workspace string
	// Resolve reads the fallback credential on every request, as the primary's
	// resolver does, so the value is never stored here.
	Resolve func() (credentials.Secret, error)
	// OnFailover, if set, runs once, when the first request is moved. It is
	// told nothing about either key.
	OnFailover func()
}

// SetFallback arms request-level failover. It is configuration, to be called
// before the adapter serves requests, not while it does.
//
// Failover happens inside Stream, at the one point where it cannot replay
// anything: the request was refused outright, so no stream was opened, no
// content reached the caller, no tool ran, and — the refusal arriving before
// the model saw the prompt — nothing was billed. Retrying a whole turn from a
// wrapper outside the harness has none of those properties.
func (a *Adapter) SetFallback(f Fallback) error {
	if f.Resolve == nil {
		return errors.New("anthropic: a fallback needs a credential")
	}
	workspace, err := ParseWorkspaceID(f.Workspace)
	if err != nil {
		return fmt.Errorf("fallback %w", err)
	}
	client := newClient(a.baseURL, workspace, f.Resolve)
	client.Retry = a.client.Retry
	client.ResponseHeaderTimeout = a.client.ResponseHeaderTimeout
	a.fallback, a.onFailover = client, f.OnFailover
	return nil
}

// FailedOver reports whether requests are now going to the fallback key.
func (a *Adapter) FailedOver() bool { return a.fallback != nil && a.failedOver.Load() }

// creditExhausted reports whether a failure is the API refusing a request
// because the key's organisation has no prepaid credit left: an HTTP 400
// invalid_request_error whose message says the credit balance is too low.
//
// The test is deliberately narrow. The errors reference documents the 400 and
// its type but not this message, which is quoted from the API's responses, so
// anything that does not match all three is left alone — a spend limit (also a
// 400 invalid_request_error) is a ceiling someone set, and a rate limit, a 402
// billing_error or a 404 workspace are not things another organisation's key
// should quietly route around. InStream is excluded because a failure inside a
// 200 is in a response the API accepted, after message_start had metered the
// prompt.
func creditExhausted(err error) bool {
	var failure *transport.Error
	if !errors.As(err, &failure) || failure.Status != http.StatusBadRequest || failure.InStream() {
		return false
	}
	var body struct {
		Type  string `json:"type"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(failure.Body), &body) != nil || body.Type != EventError || body.Error == nil {
		return false
	}
	return body.Error.Type == "invalid_request_error" &&
		strings.Contains(strings.ToLower(body.Error.Message), "credit balance is too low")
}

// Name identifies the adapter.
func (a *Adapter) Name() string { return Name }

// Capability describes a model this adapter serves.
func (a *Adapter) Capability(model string) (llm.Capability, bool) { return Capability(model) }

// PriceOf answers budget.Pricer.
func (a *Adapter) PriceOf(model string) (budget.Prices, bool) { return PriceOf(model) }

// RejectionUnbilled answers budget.UnbilledRejector: whether an HTTP status
// proves the request was refused before the model processed it. Each status
// here is one the API documents as a refusal of the request itself — invalid
// or over a spend limit (400), unauthenticated (401), billing (402),
// forbidden (403), not found (404), too large (413, refused before it reaches
// the API), rate limited (429), overloaded (529). Usage is billed per token
// sent to and generated by the model, and a refused request reaches neither.
// 500 and 504 are not here: they can fail after processing began.
func (*Adapter) RejectionUnbilled(status int) bool {
	switch status {
	case 400, 401, 402, 403, 404, 413, 429, 529:
		return true
	}
	return false
}

// AttemptGateSupported declares that every Messages API attempt, retries
// included, goes through the shared transport admission gate: Stream's only
// HTTP calls are PostStream on the primary or fallback client, which admits
// each attempt before sending it.
func (*Adapter) AttemptGateSupported() bool { return true }

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
	CacheControl *wireCache      `json:"cache_control,omitempty"`
}

// wireCache is top-level automatic prompt caching: the API places the
// breakpoint on the last cacheable block, so each turn of an agent loop reads
// the history the previous turn wrote instead of paying full input price for
// it again.
type wireCache struct {
	Type string `json:"type"`
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
	// PostStream rather than Post, because a 200 is not yet a success here: the
	// API documents that an overloaded_error can arrive as an `event: error`
	// frame inside an otherwise healthy stream, "which would normally
	// correspond to an HTTP 529 in a non-streaming context". The same condition
	// as a 529 is retried, so it is retried in this form too; see preflight.
	client, onFallback := a.client, a.FailedOver()
	if onFallback {
		client = a.fallback
	}
	body2, err := client.PostStream(ctx, MessagesPath, body, preflight)
	if err != nil && !onFallback && a.fallback != nil && creditExhausted(err) {
		// PostStream returned no body, so nothing of this request reached the
		// caller; sending it again on the other key duplicates nothing. Any
		// failure from the fallback, credit included, is the answer — the
		// primary is already known to be empty.
		if a.failedOver.CompareAndSwap(false, true) && a.onFailover != nil {
			a.onFailover()
		}
		body2, err = a.fallback.PostStream(ctx, MessagesPath, body, preflight)
	}
	if err != nil {
		return nil, err
	}
	return newStream(body2, req.Model, a.AppliedMaxTokens(req)), nil
}

// SetResponseHeaderTimeout sets how long a request may wait for response
// headers before it is abandoned and retried. Zero disables the bound; see
// transport.Client.ResponseHeaderTimeout.
func (a *Adapter) SetResponseHeaderTimeout(d time.Duration) {
	a.client.ResponseHeaderTimeout = d
	if a.fallback != nil {
		a.fallback.ResponseHeaderTimeout = d
	}
}

// AppliedMaxTokens is the bound this adapter will actually send. Kept apart
// from Capability.MaxOutputTokens, which is the model's ceiling: this adapter
// declares 128000 there and sends 8192, so a caller comparing a response
// against the ceiling would never see a truncation.
func (a *Adapter) AppliedMaxTokens(req llm.Request) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return a.DefaultMaxTokens
}

func (a *Adapter) buildRequest(req llm.Request) (*wireRequest, error) {
	maxTokens := a.AppliedMaxTokens(req)
	out := &wireRequest{
		Model:        req.Model,
		MaxTokens:    maxTokens,
		System:       req.System,
		Stream:       true,
		CacheControl: &wireCache{Type: "ephemeral"},
	}
	// Temperature is deliberately absent from the wire type. Current models
	// reject the sampling parameters outright, and ValidateRequest refuses a
	// request that sets one — a field that can never be populated is worse than
	// no field, because it reads as support.
	// A model that does not reason is sent no thinking configuration at all:
	// Haiku 4.5 rejects {type: "adaptive"} with a 400, and adaptive is this
	// adapter's default.
	thinking := a.Thinking
	if c, ok := Capability(req.Model); ok && !c.SupportsReasoning {
		thinking = ThinkingDefault
	}
	switch thinking {
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

// maxDecodedResponseBytes bounds how much of one response this stream will
// hold across all accumulators. The stall watchdog bounds silence, not volume;
// without a byte ceiling a server generating past any max_tokens it was given
// makes the harness allocate without bound.
//
// It is transport.MaxDecodedResponseBytes rather than a local copy of the same
// literal. Three adapters each declaring the number independently is how they
// came to disagree about what it counted.
const maxDecodedResponseBytes = transport.MaxDecodedResponseBytes

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

	// retained is what those blocks are holding, tallied as it changes rather
	// than totalled on demand. decodedBytes runs once per event, so walking
	// every accumulator to add up its builders made the cap that bounds a
	// runaway quadratic in the size of the runaway — measured at eight seconds
	// of CPU to reach a ceiling the stream should hit in milliseconds, which is
	// its own denial of service.
	retained int
	order    []int

	stopReason llm.StopReason
	usage      llm.Usage
	// promptSeen is set once message_start has reported the prompt's usage,
	// which is final from that point whatever happens to the rest.
	promptSeen bool
	// stopped is set by message_stop, the only event that says the message is
	// complete.
	stopped bool
	done    bool
	failure error
}

type accumulator struct {
	kind      string
	text      strings.Builder
	signature string
	data      string
	toolID    string
	toolName  string
	args      strings.Builder
	// sealed is set by content_block_stop, the event that says this block's
	// content (a tool call's arguments in particular) is complete.
	sealed bool
}

func newStream(body io.ReadCloser, model string, maxTokensApplied int) *stream {
	return &stream{
		// No [DONE] sentinel: this API ends the stream with message_stop and
		// then closes.
		sse:    transport.NewSSEWithStall(body, "", transport.DefaultHostedStallTimeout, nil),
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
				// message_stop is the only event that says the message is
				// whole. A connection that closes without it is a truncated
				// response, reported here so it never settles as one: Response
				// would fail anyway, but a consumer that stops at EOF must not
				// mistake the prefix for the answer.
				if !s.stopped {
					s.failure = transport.TruncatedStream(Name, "message_stop")
					return llm.Chunk{}, s.failure
				}
			} else {
				// A stream that died mid-flight settles as that failure.
				// Without this the reader's error is known only to whoever
				// called Next, and Response answers "called before the stream
				// was exhausted" — blaming the caller for a server that went
				// silent.
				s.failure = err
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
		// Checked once per event rather than per write, and by setting the
		// failure rather than returning it, so a chunk this frame produced
		// still reaches the caller before the refusal does.
		if over := s.decodedBytes(); over > maxDecodedResponseBytes && s.failure == nil {
			s.failure = fmt.Errorf(
				"anthropic: the response exceeded the %d-byte decode limit (%d bytes and still arriving); "+
					"the server is generating past any max_tokens it was given",
				maxDecodedResponseBytes, over)
		}
		if emit {
			return chunk, nil
		}
	}
}

// decodedBytes is everything this stream is holding from the response so far.
//
// Each block is charged transport.RetainedAccumulatorBytes for the fixed cost
// of existing, on top of the content it holds. Without that charge a stream of
// content_block_start events at ever-increasing indices allocated one
// accumulator and one map entry per event while contributing nothing to a tally
// that counted only content, so the cap could not fire on a runaway that sent
// no content at all. The same rule is applied by the openaicompat and gemini
// decoders; this one already counted signature bytes, and gemini did not.
func (s *stream) decodedBytes() int { return s.retained }

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
			s.promptSeen = s.usage.InputTokens+s.usage.CacheReadTokens+s.usage.CacheWriteTokens > 0
		}
		return llm.Chunk{}, false, nil

	case EventContentBlockStart:
		if ev.ContentBlock == nil {
			return llm.Chunk{}, false, errors.New("anthropic: content_block_start without a block")
		}
		acc := &accumulator{kind: ev.ContentBlock.Type}
		s.blocks[ev.Index] = acc
		s.order = append(s.order, ev.Index)
		// Charged the moment the block exists, not when it collects anything.
		// An accumulator that never receives a delta still costs a struct, a
		// builder and an entry in this map, and a stream of empty
		// content_block_start events was previously free of charge — so the
		// cap could not fire on a runaway that sent no content at all.
		s.retained += transport.RetainedAccumulatorBytes
		switch ev.ContentBlock.Type {
		case BlockToolUse:
			acc.toolID = ev.ContentBlock.ID
			acc.toolName = ev.ContentBlock.Name
			s.retained += len(acc.toolID) + len(acc.toolName)
			return llm.Chunk{
				Kind: llm.ChunkToolCallStart, BlockIndex: ev.Index,
				ToolCallID: llm.CallID(acc.toolID), ToolName: acc.toolName,
			}, true, nil
		case BlockText:
			if ev.ContentBlock.Text != "" {
				acc.text.WriteString(ev.ContentBlock.Text)
				s.retained += len(ev.ContentBlock.Text)
				return llm.Chunk{Kind: llm.ChunkText, BlockIndex: ev.Index, Text: ev.ContentBlock.Text}, true, nil
			}
		case BlockRedactedThinking:
			acc.data = ev.ContentBlock.Data
			s.retained += len(acc.data)
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
			s.retained += len(ev.Delta.Text)
			return llm.Chunk{Kind: llm.ChunkText, BlockIndex: ev.Index, Text: ev.Delta.Text}, true, nil
		case DeltaThinking:
			acc.text.WriteString(ev.Delta.Thinking)
			s.retained += len(ev.Delta.Thinking)
			return llm.Chunk{Kind: llm.ChunkReasoning, BlockIndex: ev.Index, Text: ev.Delta.Thinking}, true, nil
		case DeltaSignature:
			// The signature is what makes a thinking block replayable. It is
			// accumulated but not surfaced as a chunk: it is not content. It is
			// still counted: it is retained for the life of the stream and it
			// reaches the session log, and the sibling gemini decoder omitting
			// exactly this field is the disagreement this rule exists to end.
			acc.signature += ev.Delta.Signature
			s.retained += len(ev.Delta.Signature)
			return llm.Chunk{}, false, nil
		case DeltaInputJSON:
			acc.args.WriteString(ev.Delta.PartialJSON)
			s.retained += len(ev.Delta.PartialJSON)
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

	case EventMessageStop:
		s.stopped = true
		return llm.Chunk{}, false, nil

	case EventContentBlockStop:
		if acc := s.blocks[ev.Index]; acc != nil {
			acc.sealed = true
		}
		return llm.Chunk{}, false, nil

	case EventPing:
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
	// A connection that closes cleanly mid-message ends the reader exactly as
	// a finished message does. Only message_stop says the message is whole;
	// without it the content is a prefix and the usage lacks its output count,
	// so assembling a Response would hand the agent a truncated turn and the
	// budget an under-counted charge.
	if !s.stopped {
		return llm.Response{}, transport.TruncatedStream(Name, "message_stop")
	}

	msg := llm.Message{
		Role:       llm.RoleAssistant,
		Provenance: &llm.AssistantProvenance{Provider: Name, Model: s.model},
	}
	for _, index := range s.order {
		acc := s.blocks[index]
		if acc == nil {
			return llm.Response{}, fmt.Errorf("anthropic: content block %d is in the stream's order but was never opened", index)
		}
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
			// A tool call has to be routable and answerable, and both halves
			// zero-value to the empty string. Without a name the tool layer
			// looks up "" and cannot tell that from a tool that does not
			// exist; without an id the tool_result this turn sends back
			// references nothing, so the next request is malformed. Neither is
			// a call — they are the absence of one wearing its shape, and the
			// guard below already refuses the third way a block can arrive
			// unusable.
			//
			// The sibling adapters both already refused this and this one did
			// not: gemini errors with "a tool call arrived with no function
			// name", and openaicompat records a MalformedCall reasoning that
			// "inventing one would run a tool at random". Three ports of one
			// contract, and the odd one out passed the empty call through.
			// Found by FuzzStreamNeverSettlesIntoAToolCallItCannotRoute.
			if strings.TrimSpace(acc.toolName) == "" {
				return llm.Response{}, fmt.Errorf(
					"anthropic: a tool call arrived with no name (id %q); it cannot be routed, "+
						"and running a tool chosen some other way would be inventing the call",
					acc.toolID)
			}
			if strings.TrimSpace(acc.toolID) == "" {
				return llm.Response{}, fmt.Errorf(
					"anthropic: tool call %q arrived with no id; the tool_result answering it "+
						"would reference nothing and the next request would be rejected",
					acc.toolName)
			}
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
			// A call is complete when its block was sealed by content_block_stop.
			// An unsealed block means the stream lost the event that frames the
			// arguments, so whatever arrived is a prefix; running it is how a
			// half-received call becomes `{}`. A sealed call that took no
			// arguments is legitimately empty and is the only way `{}` appears.
			if !acc.sealed {
				return llm.Response{}, fmt.Errorf(
					"anthropic: tool call %q (id %q) was never closed by content_block_stop; "+
						"its arguments may be incomplete and it was not dispatched",
					acc.toolName, acc.toolID)
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
	return llm.Response{Message: msg, StopReason: stop, Usage: wholePrompt(s.usage), MaxTokensApplied: s.maxTokensApplied}, nil
}

// Close releases the stream.
func (s *stream) Close() error { return s.sse.Close() }

// UsageSoFar reports the usage seen so far, answering budget.UsageReporter.
// The prompt counts are final once message_start has arrived; the output
// count is whatever the stream reached, and a caller bounding a cut-short
// attempt must not treat it as final.
func (s *stream) UsageSoFar() (llm.Usage, bool) {
	return wholePrompt(s.usage), s.promptSeen
}

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

// wholePrompt restates usage in llm.Usage's terms. Anthropic's input_tokens
// counts only the uncached part of the prompt, where llm.Usage.InputTokens is
// the whole prompt, as the OpenAI-compatible and Gemini adapters report it.
// Passed through raw, a cached turn reports a prompt smaller than its own
// cache read, and everything priced or ratioed from it is wrong.
func wholePrompt(u llm.Usage) llm.Usage {
	u.InputTokens += u.CacheReadTokens + u.CacheWriteTokens
	return u
}

// ReplayableOn answers the ReasoningReplayer question for this adapter.
// Anthropic signs thinking blocks per model, so replay is same-model only.
func (a *Adapter) ReplayableOn(fromModel, toModel string) bool {
	return ReplayableOn(fromModel, toModel)
}
