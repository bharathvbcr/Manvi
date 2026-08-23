package gemini

import (
	"bytes"
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

// Adapter speaks the interactions API.
type Adapter struct {
	client *transport.Client
	// ThinkingLevel is sent as generation_config.thinking_level when a request
	// asks for reasoning. The request's Effort selects it; this is the fallback
	// for a model that reasons but was given no explicit level.
	DefaultThinkingLevel string
}

// New builds an adapter.
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
		// A bespoke header rather than an Authorization bearer. Copying the
		// bearer form from another adapter is the single easiest way to get
		// this wrong, and it fails as a 401 that looks like a bad key.
		h.Set(APIKeyHeader, secret.Reveal())
		return h, nil
	})
	return &Adapter{client: client}
}

// Name identifies the adapter.
func (a *Adapter) Name() string { return Name }

// Capability describes a model this adapter serves.
func (a *Adapter) Capability(model string) (llm.Capability, bool) { return Capability(model) }

type wireRequest struct {
	Model             string       `json:"model"`
	Input             []wireInput  `json:"input"`
	SystemInstruction string       `json:"system_instruction,omitempty"`
	Tools             []wireTool   `json:"tools,omitempty"`
	GenerationConfig  *wireGenConf `json:"generation_config,omitempty"`
	// Store is always sent explicitly rather than left to the API's default.
	// See StoreInteractions for why it says what it says, and note what is
	// absent here: there is no previous_interaction_id field, so no request
	// this adapter builds can resume an earlier interaction. That absence is
	// what keeps the session log the complete record of what the model saw.
	Store  bool `json:"store"`
	Stream bool `json:"stream"`
}

type wireGenConf struct {
	ThinkingLevel   string `json:"thinking_level,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
}

type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireInput struct {
	Type    string        `json:"type"`
	Content []wireContent `json:"content,omitempty"`

	// function_call. The call's own identifier is `id` here.
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	ID        string          `json:"id,omitempty"`

	// Signature authorises replaying a function_call. Without it the call is
	// refused; with it the model can see what it already did.
	Signature string `json:"signature,omitempty"`

	// function_result. The field naming is deliberately asymmetric with
	// function_call above and that asymmetry is the API's, not a slip: a call
	// carries its own `id`, and a result references that call through
	// `call_id`. Sending `id` on a result is rejected outright —
	//
	//	http 400: Unknown parameter 'id' at 'input[2]'
	//
	// — which fails the *second* step of any turn that called a tool, while a
	// single-step probe passes. That is why this went unnoticed: the shape is
	// only reachable once history contains a completed tool call.
	CallID string          `json:"call_id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

type wireContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Input item type discriminators.
const (
	InputUser        = "user_input"
	InputModelOutput = "model_output"
)

// Stream begins a model call.
func (a *Adapter) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	if err := ValidateRequest(req); err != nil {
		return nil, err
	}
	body, err := a.buildRequest(req)
	if err != nil {
		return nil, err
	}
	// PostStream rather than Post, because a 200 from this endpoint is not yet
	// a success: an overloaded model and some transient backend faults are
	// reported as an `event: error` frame inside an otherwise healthy stream.
	// See preflight.
	stream, err := a.client.PostStream(ctx, InteractionsPath+"?"+StreamQuery, body, preflight)
	if err != nil {
		return nil, err
	}
	return newStream(stream, req.Model, req.MaxTokens), nil
}

func (a *Adapter) buildRequest(req llm.Request) (*wireRequest, error) {
	out := &wireRequest{
		Model:             req.Model,
		SystemInstruction: req.System,
		Store:             StoreInteractions,
		Stream:            true,
	}
	level := req.Effort
	if level == "" {
		level = a.DefaultThinkingLevel
	}
	if level != "" || req.MaxTokens > 0 {
		out.GenerationConfig = &wireGenConf{ThinkingLevel: level, MaxOutputTokens: req.MaxTokens}
	}
	for _, tool := range req.Tools {
		out.Tools = append(out.Tools, wireTool{
			Type: TypeFunction, Name: tool.Name,
			Description: tool.Description, Parameters: tool.InputSchema,
		})
	}

	// Two passes, because what one message may send depends on what the rest of
	// the history contains. See historyShape for the two rules and how they
	// were established.
	shape := historyShape(req.Messages)
	for i, msg := range req.Messages {
		items, err := toWireInputShaped(msg, shape)
		if err != nil {
			return nil, fmt.Errorf("gemini: message %d: %w", i, err)
		}
		out.Input = append(out.Input, items...)
	}
	if len(out.Input) == 0 {
		return nil, errors.New("gemini: every message was dropped; there is nothing to send")
	}
	return out, nil
}

// historyShape is what one pass over the conversation has to know before any of
// it can be encoded.
//
// Both fields exist because this wire refuses shapes that look obviously
// reasonable, and the refusals are vague enough ("Invalid input received.",
// "Request contains an invalid argument.") that they have to be established by
// experiment rather than read off the error. Measured against the live endpoint
// on 2026-08-19, each cell repeated until it was stable:
//
//	user + function_call + function_result            0/3 accepted
//	user + function_result (no name)                  0/3
//	user + function_result (with name)                3/3
//	user + model_output + function_result             0/2
//	user + function_result + model_output             0/2
//	user + function_result + function_result          2/2
//	user + model_output + user   (no results at all)  3/3
//
// So: a function_call is never replayed as an input item — the result carries
// the call's id and its name, and that is the whole of what goes back — and a
// conversation that contains any tool result may not carry model_output at all,
// anywhere in the input, before or after.
//
// The second rule is a real loss and is worth naming: on a turn that has used a
// tool, the model does not get its own earlier prose back. Nothing can be done
// about that from this side, and it is better than the alternative the harness
// had, which was every such request refused outright.
type historyShapeInfo struct {
	// hasToolResults reports whether any tool result is being sent, which is
	// what makes model_output unsendable.
	hasToolResults bool
	// callNames maps a tool call's id to the function it named, because the
	// result has to carry that name and llm.ToolResultBlock does not hold one.
	callNames map[llm.CallID]string
}

func historyShape(messages []llm.Message) historyShapeInfo {
	shape := historyShapeInfo{callNames: map[llm.CallID]string{}}
	for _, msg := range messages {
		for _, block := range msg.Content {
			switch b := block.(type) {
			case llm.ToolCallBlock:
				shape.callNames[b.ID] = b.Name
			case llm.ToolResultBlock:
				shape.hasToolResults = true
			}
		}
	}
	return shape
}

func toWireInputShaped(msg llm.Message, shape historyShapeInfo) ([]wireInput, error) {
	if msg.Role == llm.RoleSystem {
		return nil, errors.New("system content belongs in Request.System, not in Messages")
	}
	itemType := InputUser
	if msg.Role == llm.RoleAssistant {
		itemType = InputModelOutput
	}

	var out []wireInput
	primary := wireInput{Type: itemType}
	for _, block := range msg.Content {
		switch b := block.(type) {
		case llm.TextBlock:
			primary.Content = append(primary.Content, wireContent{Type: DeltaText, Text: b.Text})
		case llm.ReasoningBlock:
			// Dropped, and it used to be an error.
			//
			// The refusal made sense while nothing about an assistant turn was
			// replayable: PrepareHistory stripped the whole message, so a
			// reasoning block arriving here meant something upstream had gone
			// wrong. Now the message is kept — it carries the call signatures
			// that let a tool call be replayed at all — and the reasoning
			// travels with it as far as this encoder, which still has nowhere
			// to put it. This wire has no input field for thought text, and
			// sending it as ordinary text would put the model's private
			// reasoning into the prompt as if it had said it aloud.
			//
			// Nothing is lost that could have been sent. The live stream
			// delivers no reasoning text in the first place: a thinking step
			// carries only its signature, and that is preserved.
			continue
		case llm.ToolCallBlock:
			// Replayed, and only with its signature. A call sent back without
			// one is refused outright, so an unsigned call is dropped rather
			// than sent — the turn then continues from the results alone, which
			// is degraded but works, instead of failing the request entirely.
			signature := signatureFor(msg, b.ID)
			if signature == "" {
				continue
			}
			args := b.Arguments
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			out = append(out, wireInput{
				Type: TypeFunctionCall, Name: b.Name, Arguments: args,
				ID: string(b.ID), Signature: signature,
			})
		case llm.ToolResultBlock:
			var body strings.Builder
			for _, inner := range b.Content {
				if t, ok := inner.(llm.TextBlock); ok {
					body.WriteString(t.Text)
				}
			}
			text := body.String()
			if b.IsError {
				// Carried in the text because no documented example puts an
				// is_error field on this wire, and an unknown parameter here is
				// not a degraded result — it is a refusal of the whole request.
				text = "ERROR: " + text
			}
			encoded, err := json.Marshal([]map[string]any{{"type": DeltaText, "text": text}})
			if err != nil {
				return nil, err
			}
			// name is required, and llm.ToolResultBlock does not carry one, so
			// it comes from the call this result answers. A result whose call is
			// not in the history cannot name its function and would be refused;
			// saying so here beats a vague refusal from the server.
			name, ok := shape.callNames[b.ToolCallID]
			if !ok {
				return nil, fmt.Errorf(
					"tool result %q answers a call that is not in this history, so its function "+
						"cannot be named; this wire requires the name on every result", b.ToolCallID)
			}
			out = append(out, wireInput{
				Type: TypeFunctionResult, CallID: string(b.ToolCallID), Name: name, Result: encoded,
			})
		case llm.ImageBlock:
			return nil, errors.New("this adapter does not send images")
		default:
			return nil, fmt.Errorf("cannot send a %s block", block.Kind())
		}
	}

	var items []wireInput
	if len(primary.Content) > 0 {
		// An assistant turn's prose is dropped from a conversation that carries
		// tool results, because this wire refuses model_output alongside them.
		// A user turn is never dropped: it is the instruction, and a request
		// missing it is a different request.
		if itemType != InputModelOutput || !shape.hasToolResults {
			items = append(items, primary)
		}
	}
	items = append(items, out...)
	// No error on an empty result any more. An assistant turn that held only a
	// tool call, or only prose that this history cannot carry, legitimately
	// contributes nothing — and refusing the whole request over it would fail
	// every turn that used a tool. buildRequest checks that something survived.
	return items, nil
}

type stream struct {
	sse   *transport.SSE
	model string
	// maxTokensApplied is the output bound this request carried, carried back
	// so a caller can tell a response that ran to its budget from one the
	// server merely labelled a normal stop. See llm.Response.MaxTokensApplied.
	maxTokensApplied int

	text      strings.Builder
	reasoning strings.Builder
	// order preserves the sequence tool calls were opened in, so the settled
	// message lists them the way the model produced them rather than in map
	// iteration order.
	order []*callAccumulator

	usage      llm.Usage
	stopReason llm.StopReason
	done       bool
	failure    error

	nextIndex int
	textIndex int
	reasonIdx int

	// stepKinds and calls are keyed by the stream's own step index, because a
	// step.delta on the live wire carries nothing but that index and a payload.
	// Without them a delta cannot be attributed to anything.
	stepKinds map[int]string
	calls     map[int]*callAccumulator
	// lastSignature is the most recent thought signature seen on this stream.
	// A thinking step precedes the call it produced, so this is set by the time
	// the call opens.
	lastSignature string
}

type callAccumulator struct {
	id    string
	name  string
	args  strings.Builder
	block int
	// signature is the thought signature that preceded this call.
	//
	// It is the credential that makes the call replayable. A function_call sent
	// back without one is refused -- "Request contains an invalid argument." --
	// and with one it is accepted, which is the difference between a model that
	// can see what it already did and one that cannot.
	signature string
	// sealed marks a call the stream has said is complete, via the step.stop
	// event that frames it.
	//
	// The event was being discarded, and discarding it left the accumulator
	// with no notion of a finished call: callFor matches on id, so a second
	// call arriving under an id already used appended its arguments to the
	// first one's. Two well-formed calls became one whose arguments were two
	// JSON objects run together, and the turn died reporting them as
	// "incomplete" — a diagnosis that sends the reader to look for a truncated
	// stream that never happened.
	//
	// A server has no obligation to make ids unique across a stream, and this
	// decoder has no way to make it. What it can do is stop treating "same id"
	// as "same call" once the stream has framed the first one as over.
	sealed bool
}

// maxTokensApplied is req.MaxTokens verbatim: this adapter emits
// max_output_tokens only when the caller set one, so zero here is not a
// missing number, it is an unbounded request.
func newStream(body io.ReadCloser, model string, maxTokensApplied int) *stream {
	return &stream{
		sse:       transport.NewSSEWithStall(body, DoneSentinel, transport.DefaultHostedStallTimeout),
		model:     model,
		textIndex: -1,
		reasonIdx: -1,
		stepKinds: map[int]string{},
		calls:     map[int]*callAccumulator{},

		maxTokensApplied: maxTokensApplied,
	}
}

type wireEvent struct {
	EventType string `json:"event_type"`
	// Index identifies which step a frame belongs to. A step.delta carries no
	// step object at all -- only this index and a delta -- so without it a
	// delta cannot be attributed to the call or the message it continues.
	Index int `json:"index"`
	// Delta is the live stream's payload for step.delta. The documented shape
	// puts content blocks inside `step`; the wire puts them here.
	Delta *struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thought   string          `json:"thought"`
		Arguments json.RawMessage `json:"arguments"`
		Signature string          `json:"signature"`
	} `json:"delta"`
	Step *struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Content   []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Thought string `json:"thought"`
		} `json:"content"`
	} `json:"step"`
	Interaction *struct {
		Status string `json:"status"`
		Usage  *struct {
			TotalInputTokens        int `json:"total_input_tokens"`
			TotalOutputTokens       int `json:"total_output_tokens"`
			ThoughtTokens           int `json:"total_thought_tokens"`
			CachedContentTokenCount int `json:"cached_content_token_count,omitempty"`
		} `json:"usage"`
	} `json:"interaction"`
	Error *struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
	// Status is the interaction status on an interaction.status_update frame,
	// where it sits at the top level rather than inside `interaction`. Decoding
	// it from the wrong place is how a terminal status arrives as a zero value
	// and the turn ends with StopOther for no visible reason.
	Status string `json:"status"`
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
			} else {
				// A stream that died mid-flight settles as that failure, so
				// Response reports the server's silence instead of blaming
				// the caller for exhausting nothing.
				s.failure = err
			}
			return llm.Chunk{}, err
		}

		if event.Name == EventDone || string(bytes.TrimSpace(event.Data)) == DoneSentinel {
			s.done = true
			return llm.Chunk{}, io.EOF
		}

		var ev wireEvent
		if err := json.Unmarshal(event.Data, &ev); err != nil {
			s.failure = fmt.Errorf("gemini: undecodable %s event: %w", event.Name, err)
			return llm.Chunk{}, s.failure
		}
		// The type travels twice — in the SSE event name and in the payload —
		// and they must agree. Believing one over the other when they differ is
		// how a decoder silently misroutes content.
		kind := ev.EventType
		if event.Name != "" {
			if kind != "" && kind != event.Name {
				s.failure = fmt.Errorf("gemini: event name %q disagrees with payload event_type %q",
					event.Name, kind)
				return llm.Chunk{}, s.failure
			}
			kind = event.Name
		}

		chunk, emit, err := s.apply(kind, ev)
		if err != nil {
			s.failure = err
			return llm.Chunk{}, err
		}
		// Checked once per event rather than per write, and by setting the
		// failure rather than returning it, so a chunk this frame produced
		// still reaches the caller before the refusal does.
		if over := s.decodedBytes(); over > maxDecodedResponseBytes && s.failure == nil {
			s.failure = fmt.Errorf(
				"gemini: the response exceeded the %d-byte decode limit (%d bytes and still arriving); "+
					"the server is generating past any max_tokens it was given",
				maxDecodedResponseBytes, over)
		}
		if emit {
			return chunk, nil
		}
	}
}

// maxDecodedResponseBytes bounds how much of one response this stream will
// hold across all accumulators. Mirrors openaicompat's limit: the stall
// watchdog bounds silence, not volume.
const maxDecodedResponseBytes = 4 << 20

// decodedBytes is everything this stream is holding from the response so far.
func (s *stream) decodedBytes() int {
	total := s.text.Len() + s.reasoning.Len()
	for _, acc := range s.calls {
		if acc == nil {
			continue
		}
		total += acc.args.Len()
	}
	return total
}

func (s *stream) apply(kind string, ev wireEvent) (llm.Chunk, bool, error) {
	if ev.Error != nil {
		return llm.Chunk{}, false, fmt.Errorf("gemini: stream error: %s (%s)",
			ev.Error.Message, ev.Error.Status)
	}

	switch kind {
	case EventInteractionCompleted:
		if ev.Interaction != nil {
			if u := ev.Interaction.Usage; u != nil {
				s.usage.InputTokens = u.TotalInputTokens
				s.usage.OutputTokens = u.TotalOutputTokens
				s.usage.ReasoningTokens = u.ThoughtTokens
				s.usage.CacheReadTokens = u.CachedContentTokenCount
			}
			s.stopReason = mapStatus(ev.Interaction.Status)
		}
		return llm.Chunk{}, false, nil

	case EventInteractionStatusUpdate:
		// Progress frames, sent repeatedly while the model works. They carry no
		// content, but they do carry a status, and a terminal one here has to
		// be believed: ignoring the frame entirely would discard the only
		// notice on a stream that reports its ending this way. A non-terminal
		// status is left alone so an in_progress frame cannot overwrite a
		// stop reason already settled by a completion frame.
		if stop := mapStatus(ev.Status); stop != llm.StopOther {
			s.stopReason = stop
		}
		return llm.Chunk{}, false, nil

	case EventInteractionNeedsInput:
		// The model is waiting on a tool result. That is the tool-use stop
		// reason in neutral terms, and the loop branches on it.
		s.stopReason = llm.StopToolUse
		return llm.Chunk{}, false, nil

	case EventStepStop:
		// The frame that says a call is complete. Nothing is emitted — the
		// arguments already went out as deltas — but the accumulator is closed
		// so a later step carrying the same id starts a new call instead of
		// appending to this one.
		if ev.Step != nil && ev.Step.Type == TypeFunctionCall {
			s.seal(ev.Step.ID, ev.Step.Name)
		}
		return llm.Chunk{}, false, nil

	case EventStepStart:
		if ev.Step == nil {
			return llm.Chunk{}, false, nil
		}
		// The step's own type tells every later delta at this index what it is
		// continuing, because the deltas themselves do not say.
		s.stepKinds[ev.Index] = ev.Step.Type
		return s.applyStep(ev)

	case EventStepDelta:
		// Two shapes. The live stream sends `delta` beside an `index`; the
		// documented form nests content inside `step`. Whichever arrived is
		// decoded, and neither is preferred over the other -- a frame carries
		// one or the other, never both.
		if ev.Delta != nil {
			return s.applyDelta(ev)
		}
		if ev.Step == nil {
			return llm.Chunk{}, false, nil
		}
		return s.applyStep(ev)

	default:
		// interaction.created, interaction.in_progress, and any event added
		// later carry nothing this decoder needs.
		return llm.Chunk{}, false, nil
	}
}

// applyDelta decodes the live stream's step.delta shape: an index naming the
// step, and a delta whose own type says what it carries.
//
// The adapter previously decoded only the documented shape -- content blocks
// nested inside `step` -- and the live wire sends none. The consequence was not
// a degraded answer but no answer at all: model text arrives exclusively as
// `delta.text`, so every turn settled with an empty message and the harness
// reported "the turn ended without an answer". Tool calls appeared to work only
// because step.start names the function; their arguments arrive here too, and
// were being dropped, so every call was dispatched with `{}`.
func (s *stream) applyDelta(ev wireEvent) (llm.Chunk, bool, error) {
	d := ev.Delta
	switch d.Type {
	case DeltaText:
		if d.Text == "" {
			return llm.Chunk{}, false, nil
		}
		if s.textIndex < 0 {
			s.textIndex = s.nextIndex
			s.nextIndex++
		}
		s.text.WriteString(d.Text)
		return llm.Chunk{Kind: llm.ChunkText, BlockIndex: s.textIndex, Text: d.Text}, true, nil

	case DeltaThought:
		thought := d.Thought
		if thought == "" {
			thought = d.Text
		}
		if thought == "" {
			return llm.Chunk{}, false, nil
		}
		if s.reasonIdx < 0 {
			s.reasonIdx = s.nextIndex
			s.nextIndex++
		}
		s.reasoning.WriteString(thought)
		return llm.Chunk{Kind: llm.ChunkReasoning, BlockIndex: s.reasonIdx, Text: thought}, true, nil

	case DeltaThoughtSignature:
		// Kept, not dropped, and kept out of the transcript.
		//
		// It is opaque and it is not reasoning text, so it is never stored as a
		// reasoning block -- that would put base64 in the transcript and in
		// every later prompt. But it is exactly what makes the call that
		// follows replayable, and discarding it cost more than it looked like:
		// without it the adapter could not send a function_call back at all, so
		// the model was asked to continue from a list of results for calls it
		// could not see. Measured live, that produced runs of responses with no
		// content whatsoever once a few results had accumulated.
		if d.Signature != "" {
			s.lastSignature = d.Signature
		}
		return llm.Chunk{}, false, nil

	case DeltaArgumentsDelta, DeltaArguments:
		acc := s.callAt(ev.Index)
		if acc == nil {
			// A delta for a step that never opened. Dropping it silently would
			// dispatch a call with missing arguments, which is the failure this
			// function exists to remove.
			return llm.Chunk{}, false, fmt.Errorf(
				"gemini: arguments arrived for step %d, which no step.start opened", ev.Index)
		}
		fragment, err := argumentFragment(d.Arguments)
		if err != nil {
			return llm.Chunk{}, false, fmt.Errorf(
				"gemini: tool call %q sent arguments this decoder cannot read: %w", acc.name, err)
		}
		if fragment == "" {
			return llm.Chunk{}, false, nil
		}
		acc.args.WriteString(fragment)
		return llm.Chunk{
			Kind: llm.ChunkToolCallDelta, BlockIndex: acc.block,
			ToolCallID: llm.CallID(acc.id), ToolName: acc.name,
			ArgumentsRaw: fragment,
		}, true, nil
	}
	return llm.Chunk{}, false, nil
}

// callAt returns the accumulator for a step index, if that step is a call.
func (s *stream) callAt(index int) *callAccumulator {
	return s.calls[index]
}

func (s *stream) applyStep(ev wireEvent) (llm.Chunk, bool, error) {
	step := ev.Step
	switch step.Type {
	case TypeFunctionCall:
		acc := s.callFor(step.ID, step.Name)
		// Registered under the stream's step index as well, because every later
		// delta for this call names only that index.
		s.calls[ev.Index] = acc
		if acc.signature == "" {
			acc.signature = s.lastSignature
		}
		// step.start opens a call with `"arguments": {}` on the live wire --
		// a placeholder, not an empty argument list. Writing it would put `{}`
		// in front of the real arguments that follow as deltas and make the
		// concatenation unreadable; on a call that genuinely takes none, the
		// empty object is supplied at settle time anyway.
		if ev.EventType == EventStepStart && isEmptyObject(step.Arguments) {
			return llm.Chunk{
				Kind: llm.ChunkToolCallStart, BlockIndex: acc.block,
				ToolCallID: llm.CallID(acc.id), ToolName: acc.name,
			}, true, nil
		}
		if len(step.Arguments) > 0 {
			fragment, err := argumentFragment(step.Arguments)
			if err != nil {
				return llm.Chunk{}, false, fmt.Errorf(
					"gemini: tool call %q sent arguments this decoder cannot read: %w", acc.name, err)
			}
			acc.args.WriteString(fragment)
			return llm.Chunk{
				Kind: llm.ChunkToolCallDelta, BlockIndex: acc.block,
				ToolCallID: llm.CallID(acc.id), ToolName: acc.name,
				ArgumentsRaw: fragment,
			}, true, nil
		}
		return llm.Chunk{
			Kind: llm.ChunkToolCallStart, BlockIndex: acc.block,
			ToolCallID: llm.CallID(acc.id), ToolName: acc.name,
		}, true, nil

	default:
		for _, content := range step.Content {
			switch content.Type {
			case DeltaText:
				if content.Text == "" {
					continue
				}
				if s.textIndex < 0 {
					s.textIndex = s.nextIndex
					s.nextIndex++
				}
				s.text.WriteString(content.Text)
				return llm.Chunk{Kind: llm.ChunkText, BlockIndex: s.textIndex, Text: content.Text}, true, nil
			case DeltaThought:
				thought := content.Thought
				if thought == "" {
					thought = content.Text
				}
				if thought == "" {
					continue
				}
				if s.reasonIdx < 0 {
					s.reasonIdx = s.nextIndex
					s.nextIndex++
				}
				s.reasoning.WriteString(thought)
				return llm.Chunk{Kind: llm.ChunkReasoning, BlockIndex: s.reasonIdx, Text: thought}, true, nil
			}
		}
		return llm.Chunk{}, false, nil
	}
}

// argumentFragment reads one delta's `arguments` field into the text it
// contributes to the call.
//
// The field arrives in one of two shapes and both are legitimate on a
// streaming API. A short call carries the whole argument object inline, as
// JSON. A long one cannot: an SSE data line has to be valid JSON on its own,
// so half an object is unrepresentable, and the only way to stream one is as
// successive JSON *strings* whose contents concatenate.
//
// This decoder handled the first and mishandled the second in the quietest way
// available. A single string fragment passed json.Valid — a JSON string is
// valid JSON — so a call was dispatched whose Arguments were a quoted string
// rather than an object, and the tool that received it failed to decode its own
// schema. Two or more fragments concatenated into `"..." "..."`, which is not
// valid JSON at all, and killed the turn.
//
// Deciding by shape rather than by configuration is what makes this safe under
// a server that uses either, or both in one stream: a fragment that begins a
// JSON string is unquoted, anything else is raw JSON and is taken as it stands.
func argumentFragment(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", nil
	}
	if trimmed[0] != '"' {
		return string(raw), nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err != nil {
		return "", err
	}
	return text, nil
}

// isEmptyObject reports whether raw is `{}` -- the placeholder step.start uses
// where a call's arguments will later arrive as deltas.
func isEmptyObject(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "{}"
}

func (s *stream) callFor(id, name string) *callAccumulator {
	// Searched newest-first so a reused id continues the most recent call
	// under it rather than the oldest, which is the only reading that stays
	// correct once sealed calls can share an id with a live one.
	for i := len(s.order) - 1; i >= 0; i-- {
		acc := s.order[i]
		// A sealed call is finished. A step arriving under its id is a new
		// call that happens to reuse the id, not a continuation.
		if acc.sealed {
			continue
		}
		// A step with no id still belongs to the call it continues, which is
		// the most recent one of that name.
		if (id != "" && acc.id == id) || (id == "" && acc.name == name) {
			if acc.name == "" {
				acc.name = name
			}
			return acc
		}
	}
	acc := &callAccumulator{id: id, name: name, block: s.nextIndex}
	s.nextIndex++
	s.order = append(s.order, acc)
	return acc
}

// seal closes the accumulator a step.stop event frames, so no later step can
// append to it.
func (s *stream) seal(id, name string) {
	for i := len(s.order) - 1; i >= 0; i-- {
		acc := s.order[i]
		if acc.sealed {
			continue
		}
		if (id != "" && acc.id == id) || (id == "" && acc.name == name) {
			acc.sealed = true
			return
		}
	}
}

func (s *stream) Response() (llm.Response, error) {
	if s.failure != nil {
		return llm.Response{}, s.failure
	}
	if !s.done {
		return llm.Response{}, errors.New("gemini: Response called before the stream was exhausted")
	}

	msg := llm.Message{
		Role:       llm.RoleAssistant,
		Provenance: &llm.AssistantProvenance{Provider: Name, Model: s.model},
	}
	// The signatures travel as adapter-private replay state, which is exactly
	// what AssistantProvenance.ReplayState is for: opaque bytes this adapter
	// needs to send its own history back, carried through the neutral log
	// without the neutral layer interpreting them, and handed back only when
	// the same provider is on both ends.
	if sigs := s.signatures(); len(sigs) > 0 {
		encoded, err := json.Marshal(replayState{CallSignatures: sigs})
		if err != nil {
			return llm.Response{}, err
		}
		msg.Provenance.ReplayState = encoded
	}
	if s.reasoning.Len() > 0 {
		msg.Content = append(msg.Content, llm.ReasoningBlock{Text: s.reasoning.String()})
	}
	if s.text.Len() > 0 {
		msg.Content = append(msg.Content, llm.TextBlock{Text: s.text.String()})
	}
	for _, acc := range s.order {
		args := json.RawMessage(acc.args.String())
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		// Checked as an object, not merely as valid JSON. A tool's arguments are
		// an object by definition — every schema this harness sends declares
		// "type":"object" — and the looser check let a bare JSON string through
		// as if it were one. What reached the tool was a quoted string, and the
		// tool reported a decode failure against its own schema, which reads as
		// the model having sent nonsense rather than as the adapter having
		// failed to unwrap the wire form.
		if !isJSONObject(args) {
			// Named as unusable rather than as incomplete. Truncation is the
			// common cause and it is not the only one — arguments that are two
			// concatenated objects are complete and still unusable — and
			// telling a reader the stream was cut off when it was not sends
			// them to investigate the wrong half of the system.
			return llm.Response{}, fmt.Errorf(
				"gemini: tool call %q arrived with arguments that are not a JSON object, so it was not "+
					"dispatched — the stream was cut off mid-call, or the server framed two calls as "+
					"one: %q", acc.name, args)
		}
		if acc.name == "" {
			return llm.Response{}, errors.New("gemini: a tool call arrived with no function name")
		}
		msg.Content = append(msg.Content, llm.ToolCallBlock{
			ID: llm.CallID(acc.id), Name: acc.name, Arguments: args,
		})
	}

	stop := s.stopReason
	if stop == "" {
		stop = llm.StopOther
	}
	if len(s.order) > 0 && stop == llm.StopEndTurn {
		stop = llm.StopToolUse
	}
	return llm.Response{Message: msg, StopReason: stop, Usage: s.usage, MaxTokensApplied: s.maxTokensApplied}, nil
}

// isJSONObject reports whether raw is a complete JSON object.
func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	return json.Valid(trimmed)
}

// replayState is what this adapter needs handed back to send its own history.
type replayState struct {
	// CallSignatures maps a tool call's id to the thought signature that
	// authorises replaying it.
	CallSignatures map[string]string `json:"call_signatures,omitempty"`
}

// signatures collects the per-call signatures this stream observed.
func (s *stream) signatures() map[string]string {
	var out map[string]string
	for _, acc := range s.order {
		if acc.signature == "" || acc.id == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[acc.id] = acc.signature
	}
	return out
}

// signatureFor reads a call's signature back out of a message's replay state.
func signatureFor(msg llm.Message, id llm.CallID) string {
	if msg.Provenance == nil || len(msg.Provenance.ReplayState) == 0 {
		return ""
	}
	var state replayState
	if json.Unmarshal(msg.Provenance.ReplayState, &state) != nil {
		return ""
	}
	return state.CallSignatures[string(id)]
}

func (s *stream) Close() error { return s.sse.Close() }

func mapStatus(status string) llm.StopReason {
	switch status {
	case "completed":
		return llm.StopEndTurn
	case "requires_action":
		return llm.StopToolUse
	case "incomplete", "max_tokens":
		return llm.StopMaxTokens
	case "failed":
		return llm.StopOther
	default:
		return llm.StopOther
	}
}

// ReplayableOn answers the ReasoningReplayer question for this adapter.
func (a *Adapter) ReplayableOn(fromModel, toModel string) bool {
	return ReplayableOn(fromModel, toModel)
}
