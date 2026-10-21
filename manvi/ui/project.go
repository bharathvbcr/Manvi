package ui

import (
	"encoding/json"
	"fmt"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// Project turns one session-log event into the UI events that should be shown
// for it, or nothing when the record has no visible counterpart.
//
// The direction matters. The UI renders a projection of the log rather than a
// parallel stream emitted alongside it, so there is no code path by which the
// terminal could show a turn that differs from the one the model saw. A second
// stream would drift the first time an event was emitted on one and not the
// other, and the drift would be invisible: both would look plausible.
//
// Events with no visible form — step starts, the system prompt, the
// assembled assistant message that the streamed chunks already displayed —
// return nothing rather than a placeholder. The harness's own records (step
// usage, verify reports, diagnostics) do project, as kinds only the NDJSON face
// shows; see Kind.WireOnly.
func Project(e session.Event) []Event {
	base := Event{At: e.At}

	switch e.Type {
	case session.TurnStart:
		return nil

	case session.TurnEnd:
		base.Kind = KindTurnEnd
		return []Event{base}

	case session.UserMessage:
		var data session.MessageData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		// Who wrote it decides how it is shown. A harness inject occupies the
		// user role because that is the only model-visible slot a natural stop
		// leaves, and rendering it as the operator's turn would put words in
		// their mouth in the one record this system reasons from.
		if data.Origin == session.OriginHarness {
			base.Kind = KindHarnessMessage
		} else {
			base.Kind = KindTurnStart
		}
		base.Text = data.Message.Text()
		return []Event{base}

	case session.AssistantChunk:
		var chunk llm.Chunk
		if err := json.Unmarshal(e.Data, &chunk); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		switch chunk.Kind {
		case llm.ChunkText:
			if chunk.Text == "" {
				return nil
			}
			base.Kind = KindText
			base.Text = chunk.Text
			return []Event{base}
		case llm.ChunkReasoning:
			if chunk.Text == "" {
				return nil
			}
			base.Kind = KindReasoning
			base.Text = chunk.Text
			return []Event{base}
		case llm.ChunkToolCallStart:
			// Deliberately not projected. session.ToolCall is logged for the
			// same call moments later and carries its arguments, so emitting
			// here as well rendered every tool call twice — once bare from the
			// stream, once again with its arguments. The duplicate was pure
			// noise in a transcript and, in a long agent turn, doubled the
			// line count a reader has to scan to find what actually happened.
			return nil
		}
		return nil

	case session.AssistantMessage:
		// The chunks already rendered this. Emitting it again would double
		// every response in the transcript.
		return nil

	case session.ToolCall:
		var data session.ToolCallData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		base.Kind = KindToolStart
		base.Tool = data.Name
		base.Arguments = data.Arguments
		return []Event{base}

	case session.ToolResult:
		var data session.ToolResultData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		base.Kind = KindToolResult
		base.Text = data.Text
		base.IsError = data.IsError
		return []Event{base}

	case session.PolicyDenied:
		var data session.DenialData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		base.Kind = KindPolicy
		base.Text = data.Reason
		base.Rule = data.Rule
		base.Severity = data.Severity
		base.Tool = data.Tool
		return []Event{base}

	case session.PolicyQualified:
		var data session.QualificationData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		// Rendered as the qualified pass it was, never as a block. The
		// demotion travels with it so a resumed session can still say what let
		// the call through — replaying it as a bare rule would show an allowed
		// write as a refused one.
		base.Kind = KindPolicy
		base.Text = data.Reason
		base.Rule = data.Rule
		base.Severity = data.Severity
		base.Tool = data.Tool
		base.Demoted = data.Demoted
		base.Degraded = data.Degraded
		return []Event{base}

	case session.GrantApplied:
		var data session.GrantData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		// Rendered as a policy decision carrying its grant, never as a bare
		// success. A face that showed the cleared operation without the grant
		// that cleared it would report an override as a clean pass.
		base.Kind = KindPolicy
		base.Text = "allowed by override: " + data.Target
		base.Rule = data.Rule
		base.GrantID = data.GrantID
		base.GrantedBy = data.GrantedBy
		return []Event{base}

	case session.NullResponseRetried:
		var data session.NullResponseData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		// Degraded, and deliberately so: unlike a provider that cannot replay
		// its own reasoning, this is not a standing property of the wire. It is
		// the server producing nothing, and a turn that met it repeatedly cost
		// time and tokens for responses that carried no work.
		base.Kind = KindNotice
		base.Text = fmt.Sprintf(
			"the model returned nothing on step %d — no text, no tool call, no output tokens; "+
				"asked again (%d of %d)", data.Step, data.Attempt, data.Of)
		base.Degraded = []string{"empty response from the provider"}
		return []Event{base}

	case session.ProvenanceDropped:
		var data session.DropData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		if len(data.Drops) == 0 {
			return nil
		}
		// The two reasons state is dropped are reported differently, because
		// only one of them is something an operator can act on.
		//
		// A crossing — history from one model prepared for another — is a
		// genuine fidelity loss, is avoidable by not crossing, and is reported
		// as a degraded check.
		//
		// A drop whose source and target are the same model is not a handoff.
		// It is an adapter with no documented way to replay its own reasoning,
		// which for a stateless one is permanent: it happens on every step of
		// every turn. Calling that degraded made the degraded signal fire on
		// 100% of steps against a reasoning model, and a signal that is always
		// on is a signal nobody reads. It is still reported, because it is
		// still true that the next call does not carry that reasoning — as a
		// notice, which is what a standing property of the run is.
		crossed := false
		for _, d := range data.Drops {
			if !d.SameTarget() {
				crossed = true
			}
		}
		base.Kind = KindNotice
		if crossed {
			base.Text = fmt.Sprintf("%d message(s) lost provider-private state in handoff", len(data.Drops))
			for _, d := range data.Drops {
				base.Degraded = append(base.Degraded, describeDrop(d))
			}
			return []Event{base}
		}
		model := data.Drops[0].ToProvider + "/" + data.Drops[0].ToModel
		base.Text = fmt.Sprintf(
			"%d message(s) replayed without their reasoning: %s has no way to carry its own back",
			len(data.Drops), model)
		return []Event{base}

	case session.StepEnd:
		// An old log's step/end has no payload at all. That is a step whose
		// usage was never recorded, which has nothing to report; a payload that
		// is present and unreadable is a hole and is reported as one.
		if len(e.Data) == 0 {
			return nil
		}
		var data session.StepEndData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		base.Kind = KindStepUsage
		base.InputTokens = data.Usage.InputTokens
		base.OutputTokens = data.Usage.OutputTokens
		base.ReasoningTokens = data.Usage.ReasoningTokens
		base.CacheReadTokens = data.Usage.CacheReadTokens
		base.CacheWriteTokens = data.Usage.CacheWriteTokens
		base.CostNanoUSD = data.CostNanoUSD
		// The same convention turn.usage uses, so a consumer that knows how to
		// read one reads the other: a step no table covered is counted, never
		// folded into a zero charge.
		if !data.Priced {
			base.UnpricedSteps = 1
		}
		return []Event{base}

	case session.VerifyReport:
		var data session.VerifyReportData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		// Every verdict, the passing ones included: a caller that sees only the
		// failures cannot tell a clean run from one whose check never ran.
		base.Kind = KindVerifyReport
		base.Verdict = data.Verdict
		base.Source = data.Source
		base.Paths = data.Paths
		base.Findings = data.Findings
		base.Degraded = data.Degraded
		base.Bounce = data.Bounce
		return []Event{base}

	case session.PromptAssembled:
		var data session.PromptAssembledData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		return diagnostic(e, base, data)

	case session.ToolResultCompacted:
		var data session.CompactionData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		// The shortened text is the model's copy of a result tool.result
		// already carried whole; the sizes are what the diagnostic is for.
		data.Text = ""
		return diagnostic(e, base, data)

	case session.ContextOverflow:
		var data session.OverflowData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		return diagnostic(e, base, data)

	case session.MalformedToolCall:
		var data session.MalformedCallData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		return diagnostic(e, base, data)

	case session.DecodingCompensated:
		var data session.DecodingData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return []Event{decodeFailure(e, err)}
		}
		return diagnostic(e, base, data)
	}
	return nil
}

// diagnostic projects a harness record as a diagnostic event carrying its
// decoded payload. The payload is re-encoded from the typed value the caller
// just decoded, so what reaches the wire is exactly the shape the log defines.
func diagnostic(e session.Event, base Event, payload any) []Event {
	raw, err := json.Marshal(payload)
	if err != nil {
		return []Event{decodeFailure(e, err)}
	}
	base.Kind = KindDiagnostic
	base.Type = string(e.Type)
	base.Data = raw
	return []Event{base}
}

func describeDrop(d llm.Drop) string {
	if len(d.Blocks) == 0 {
		return "replay state"
	}
	out := d.Blocks[0]
	for _, b := range d.Blocks[1:] {
		out += "+" + b
	}
	return out
}

// decodeFailure reports a log record that could not be read.
//
// Surfaced rather than skipped: a malformed record is a hole in the evidence
// trail, and a UI that silently drops it shows a transcript that looks complete.
func decodeFailure(e session.Event, err error) Event {
	return Event{
		Kind: KindError, At: e.At,
		Text: fmt.Sprintf("session record %d (%s) could not be decoded: %v", e.Seq, e.Type, err),
	}
}

// ProjectSink adapts a Sink to a session-log observer.
//
// This is the whole bridge between the agent loop and any live face: the loop
// writes to the log because it must, and the face reads what the log recorded.
func ProjectSink(sink Sink) func(session.Event) {
	return func(e session.Event) {
		for _, out := range Project(e) {
			sink.Emit(out)
		}
	}
}
