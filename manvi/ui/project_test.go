package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

func record(t *testing.T, typ session.Type, payload any) session.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return session.Event{Seq: 1, Type: typ, Data: raw}
}

func TestProjectStreamsTextAndSuppressesTheAssembledMessage(t *testing.T) {
	// The chunks already rendered the response. Emitting the assembled message
	// as well would double every reply in the transcript.
	chunk := record(t, session.AssistantChunk, llm.Chunk{Kind: llm.ChunkText, Text: "hello"})
	got := Project(chunk)
	if len(got) != 1 || got[0].Kind != KindText || got[0].Text != "hello" {
		t.Fatalf("got %#v", got)
	}

	msg := record(t, session.AssistantMessage, session.MessageData{
		Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "hello"}}},
	})
	if got := Project(msg); len(got) != 0 {
		t.Fatalf("the assembled message projected to %#v", got)
	}
}

func TestProjectSeparatesReasoningFromText(t *testing.T) {
	got := Project(record(t, session.AssistantChunk,
		llm.Chunk{Kind: llm.ChunkReasoning, Text: "thinking"}))
	if len(got) != 1 || got[0].Kind != KindReasoning {
		t.Fatalf("got %#v", got)
	}
}

func TestProjectCarriesPolicyDenials(t *testing.T) {
	got := Project(record(t, session.PolicyDenied, session.DenialData{
		Tool: "devcouncil_write_file", Rule: "secret.path",
		Severity: "hard", Reason: "credential path",
	}))
	if len(got) != 1 {
		t.Fatalf("got %#v", got)
	}
	if got[0].Kind != KindPolicy || got[0].Rule != "secret.path" || got[0].Severity != "hard" {
		t.Fatalf("got %#v", got[0])
	}
}

func TestProjectRendersAGrantAsAQualifiedAllowNeverABareSuccess(t *testing.T) {
	// A face that showed the cleared operation without the grant that cleared
	// it would report an override as a clean pass.
	got := Project(record(t, session.GrantApplied, session.GrantData{
		GrantID: "GRANT-3", GrantedBy: "human", Rule: "scope.unplanned",
		Target: "src/helper.go", Reason: "the plan omitted it",
	}))
	if len(got) != 1 {
		t.Fatalf("got %#v", got)
	}
	e := got[0]
	if e.GrantID != "GRANT-3" || e.GrantedBy != "human" || e.Rule != "scope.unplanned" {
		t.Fatalf("the grant did not travel with the event: %#v", e)
	}
	if !e.Qualified() {
		t.Fatal("a granted allow did not report as qualified")
	}
}

func TestProjectReportsDroppedProvenanceAsADegradedCheck(t *testing.T) {
	// Provider state that could not be replayed is history the next call does
	// not have — a fact about the run's fidelity, not a log line.
	for _, drop := range []llm.Drop{
		// A genuine crossing.
		{Blocks: []string{"reasoning"}, FromProvider: "anthropic", FromModel: "a",
			ToProvider: "gemini", ToModel: "b"},
		// And one whose provenance nobody recorded: unnamed is not the same as
		// same, and must take the louder reading.
		{Blocks: []string{"reasoning"}},
	} {
		got := Project(record(t, session.ProvenanceDropped, session.DropData{
			Drops: []llm.Drop{drop},
		}))
		if len(got) != 1 {
			t.Fatalf("got %#v", got)
		}
		if len(got[0].Degraded) != 1 {
			t.Fatalf("the drop was not reported as degraded: %#v", got[0])
		}
		if !got[0].Qualified() {
			t.Fatal("a lossy handoff did not report as qualified")
		}
	}
}

// TestAModelThatCannotReplayItsOwnReasoningIsNotADegradedHandoff.
//
// gemini's adapter reports no reasoning as replayable at all — its only
// mechanism for carrying thought across turns is server-side conversation
// state, which this harness deliberately opts out of so the session log stays
// the complete record. The consequence is that every step of every
// reasoning-enabled run drops reasoning, and every one of them was being
// reported as a degraded check in a lossy handoff.
//
// Nothing was handed off, and marking it degraded made the one signal that is
// supposed to mean "look at this" fire on 100% of steps. It is still reported,
// because the next call genuinely does not carry that reasoning — as a notice,
// which is what a standing property of a provider is.
func TestAModelThatCannotReplayItsOwnReasoningIsNotADegradedHandoff(t *testing.T) {
	got := Project(record(t, session.ProvenanceDropped, session.DropData{
		Drops: []llm.Drop{{
			Blocks:       []string{"reasoning"},
			FromProvider: "gemini", FromModel: "gemini-3.7-flash",
			ToProvider: "gemini", ToModel: "gemini-3.7-flash",
		}},
	}))
	if len(got) != 1 {
		t.Fatalf("got %#v", got)
	}
	if len(got[0].Degraded) != 0 {
		t.Errorf("a provider that cannot replay its own reasoning was reported as a degraded "+
			"handoff: %#v", got[0])
	}
	if !strings.Contains(got[0].Text, "gemini/gemini-3.7-flash") {
		t.Errorf("the notice does not name the model it is about: %q", got[0].Text)
	}
	if strings.Contains(got[0].Text, "handoff") {
		t.Errorf("the notice calls a same-model drop a handoff: %q", got[0].Text)
	}
}

func TestProjectSurfacesAMalformedRecordRatherThanSkippingIt(t *testing.T) {
	// A record that cannot be read is a hole in the evidence trail. Dropping it
	// silently shows a transcript that looks complete.
	bad := session.Event{Seq: 9, Type: session.ToolResult, Data: json.RawMessage(`{"text":`)}
	got := Project(bad)
	if len(got) != 1 || got[0].Kind != KindError {
		t.Fatalf("got %#v", got)
	}
	if !strings.Contains(got[0].Text, "could not be decoded") {
		t.Fatalf("got %q", got[0].Text)
	}
}

// TestProjectCoversTheRecordsHeadlessCallersNeverSaw is one case per record
// type that used to project to nothing, so `manvi run --json` never carried it.
func TestProjectCoversTheRecordsHeadlessCallersNeverSaw(t *testing.T) {
	cases := []struct {
		name    string
		typ     session.Type
		payload any
		kind    Kind
		check   func(t *testing.T, e Event)
	}{
		{"step end", session.StepEnd, session.StepEndData{
			Usage:       llm.Usage{InputTokens: 10, OutputTokens: 2, ReasoningTokens: 3, CacheReadTokens: 4, CacheWriteTokens: 5},
			CostNanoUSD: 77, Priced: true,
		}, KindStepUsage, func(t *testing.T, e Event) {
			if e.InputTokens != 10 || e.OutputTokens != 2 || e.ReasoningTokens != 3 ||
				e.CacheReadTokens != 4 || e.CacheWriteTokens != 5 || e.CostNanoUSD != 77 || e.UnpricedSteps != 0 {
				t.Errorf("step usage = %+v", e)
			}
		}},
		{"unpriced step end", session.StepEnd, session.StepEndData{
			Usage: llm.Usage{InputTokens: 1},
		}, KindStepUsage, func(t *testing.T, e Event) {
			if e.UnpricedSteps != 1 || e.CostNanoUSD != 0 {
				t.Errorf("an unpriced step reads as %+v", e)
			}
		}},
		{"verify passed", session.VerifyReport, session.VerifyReportData{
			Verdict: "passed", Source: "command", Paths: []string{"a.go"},
			Findings: []string{"ok"}, Degraded: []string{"lint"}, Bounce: 2,
		}, KindVerifyReport, func(t *testing.T, e Event) {
			if e.Verdict != "passed" || e.Source != "command" || len(e.Paths) != 1 ||
				len(e.Findings) != 1 || len(e.Degraded) != 1 || e.Bounce != 2 {
				t.Errorf("verify report = %+v", e)
			}
		}},
		{"prompt assembled", session.PromptAssembled, session.PromptAssembledData{
			Included: []string{"identity"}, Dropped: []string{"x: over budget"},
			Tokens: 9, Budget: 100, Digest: "abc",
		}, KindDiagnostic, func(t *testing.T, e Event) { wantData(t, e, "system/prompt_assembled", "abc") }},
		{"compaction", session.ToolResultCompacted, session.CompactionData{
			ToolCallID: "c1", Text: "SHORT-FORM", FromBytes: 900, ToBytes: 90,
		}, KindDiagnostic, func(t *testing.T, e Event) {
			wantData(t, e, "context/tool_result_compacted", "900")
			// The compacted text is the tool result's own and already on the
			// wire as tool.result; repeating it would double the largest field.
			if strings.Contains(string(e.Data), "SHORT-FORM") {
				t.Errorf("compaction diagnostic repeats the result text: %s", e.Data)
			}
		}},
		{"overflow", session.ContextOverflow, session.OverflowData{
			EstimatedTokens: 5000, Threshold: 4000, ContextWindow: 4096,
		}, KindDiagnostic, func(t *testing.T, e Event) { wantData(t, e, "context/overflow", "5000") }},
		{"malformed call", session.MalformedToolCall, session.MalformedCallData{
			ToolCallID: "c1", Tool: "write", Reason: "cut off",
		}, KindDiagnostic, func(t *testing.T, e Event) { wantData(t, e, "tool/malformed", "cut off") }},
		{"decoding", session.DecodingCompensated, session.DecodingData{
			FallbackFormat: "xml", PrefillDisproved: true,
		}, KindDiagnostic, func(t *testing.T, e Event) { wantData(t, e, "llm/decoding_compensated", "xml") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Project(record(t, tc.typ, tc.payload))
			if len(got) != 1 {
				t.Fatalf("projected %d events, want 1: %#v", len(got), got)
			}
			if got[0].Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", got[0].Kind, tc.kind)
			}
			tc.check(t, got[0])
		})
	}
}

func wantData(t *testing.T, e Event, typ, contains string) {
	t.Helper()
	if e.Type != typ {
		t.Errorf("type = %q, want %q", e.Type, typ)
	}
	if !json.Valid(e.Data) || !strings.Contains(string(e.Data), contains) {
		t.Errorf("data = %s, want valid JSON containing %q", e.Data, contains)
	}
}

// A record the projection newly reads and cannot decode is an error on the
// wire, as every other malformed record is, not a silent gap.
func TestMalformedObservabilityRecordsProjectToAnError(t *testing.T) {
	for _, typ := range []session.Type{
		session.StepEnd, session.VerifyReport, session.PromptAssembled,
		session.ToolResultCompacted, session.ContextOverflow,
		session.MalformedToolCall, session.DecodingCompensated,
	} {
		got := Project(session.Event{Seq: 3, Type: typ, Data: json.RawMessage(`{"nope":`)})
		if len(got) != 1 || got[0].Kind != KindError {
			t.Errorf("%s with a truncated payload projected to %#v, want one error", typ, got)
		}
	}
}

// Logs written before step/end carried a payload hold it with no data at all.
// That is a step whose usage was not recorded, not a malformed record.
func TestAStepEndWithNoPayloadProjectsToNothing(t *testing.T) {
	if got := Project(session.Event{Seq: 1, Type: session.StepEnd}); len(got) != 0 {
		t.Fatalf("an old step/end projected to %#v", got)
	}
}

func TestProjectSinkFeedsASink(t *testing.T) {
	var seen []Event
	fn := ProjectSink(SinkFunc(func(e Event) { seen = append(seen, e) }))
	fn(record(t, session.ToolCall, session.ToolCallData{Name: "devcouncil_read_file"}))
	fn(record(t, session.StepStart, nil))
	if len(seen) != 1 || seen[0].Tool != "devcouncil_read_file" {
		t.Fatalf("got %#v", seen)
	}
}

func TestLogObserverRendersWhatTheModelSaw(t *testing.T) {
	// The bridge: the loop writes to the log because it must, and the face
	// reads what the log recorded — so the two cannot disagree.
	log := session.NewLog()
	var seen []Event
	log.Observe(ProjectSink(SinkFunc(func(e Event) { seen = append(seen, e) })))

	if _, err := log.Append(session.UserMessage, session.MessageData{
		Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.AssistantChunk, llm.Chunk{Kind: llm.ChunkText, Text: "ok"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("observer saw %d events: %#v", len(seen), seen)
	}
	if seen[0].Kind != KindTurnStart || seen[1].Kind != KindText {
		t.Fatalf("got %#v", seen)
	}
}
