package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"manvi/llm"
	"manvi/session"
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
