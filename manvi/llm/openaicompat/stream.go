package openaicompat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

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
	// finished is set when a choice carried a finish_reason, the wire's own
	// statement that the model stopped. With the [DONE] sentinel it is one of
	// the two ways a stream can legitimately end; a stream that closed with
	// neither was cut off.
	finished bool
	// textFallback is Options.TextToolFallback for this stream.
	textFallback bool
	// repaired records that a tool call's arguments were rewritten to make them
	// valid JSON, so the settled response can say so. See sanitizeJSONArguments.
	repaired bool
	done     bool
	failure  error
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
		sse:         transport.NewSSEWithStall(body, DoneSentinel, opts.StallTimeout, opts.stallClock),
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
	st.textFallback = opts.TextToolFallback
	return st
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
	Type    string
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
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(data, &asObject); err == nil {
		e.Message, e.Code, e.Type = asObject.Message, asObject.Code, asObject.Type
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
			if len(s.pending) == 1 {
				// Drained: keep the backing array for the next frame's chunks.
				// Re-slicing past the last element left no capacity, so every
				// frame allocated a fresh one.
				s.pending = s.pending[:0]
			} else {
				s.pending = s.pending[1:]
			}
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
			// io.EOF is a finished stream only when the wire said so: a
			// finish_reason, or the [DONE] sentinel. Without either the
			// connection simply closed, and settling that as StopOther would
			// promote whatever tool calls had arrived — with whatever
			// arguments had arrived — to a turn the agent runs. It is a
			// failure that carries no response; UsageSoFar answers for the
			// budget.
			if !s.finished && !s.sse.Terminated() {
				s.failure = transport.TruncatedStream(s.name, "finish_reason or [DONE]")
				return llm.Chunk{}, s.failure
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
			s.finished = true
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
		// so PrepareHistoryFor will strip it on the next turn and report the drop.
		// Recording it here keeps the session log complete.
		msg.Content = append(msg.Content, llm.ReasoningBlock{Text: s.reasoning.String()})
	}

	var visibleText = s.text.String()
	var fallbackCalls []llm.ToolCallBlock

	// If no wire tool calls were accumulated, inspect the visible text for
	// fallback tool calls — but only for a server declared not to parse them.
	// A server that does parse them (every hosted provider) never leaves a call
	// in the text, so anything in the text that looks like one is the model
	// quoting an example, and running it would execute prose.
	if s.textFallback && len(s.callOrder) == 0 && visibleText != "" {
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
		args, repaired := sanitizeJSONArgumentsReport(acc.args.String())
		if repaired {
			s.repaired = true
		}
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
			ArgumentsRepaired:     s.repaired,
			ReasoningReclassified: s.filter.prefillSuspected,
			PrefillDisproved:      s.filter.prefillDisproved,
		},
	}
	return *s.settled, nil
}

func (s *stream) Close() error { return s.sse.Close() }

// UsageSoFar reports the usage the stream had seen, answering
// budget.UsageReporter. A prompt count is only trustworthy once the server has
// sent one, so a stream that reported none answers false and the caller keeps
// the worst-case reservation.
func (s *stream) UsageSoFar() (llm.Usage, bool) {
	return s.usage, s.usage.InputTokens > 0
}

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
