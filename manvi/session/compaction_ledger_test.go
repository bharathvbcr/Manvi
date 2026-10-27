package session

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// The ledger a planner reads is keyed by result: the call id and the place of
// the result among those answering it. A compaction that names a result is in
// CompactedResults and not in CompactedCalls, which keeps only the records that
// name the id alone.
func TestCompactedResultsAreKeyedByCallIDAndOccurrence(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, UserMessage, MessageData{Message: user("go")})
	mustAppend(t, l, AssistantMessage, MessageData{Message: callMessage("call_0")})
	appendEvent(t, l, ToolResult, ToolResultData{ToolCallID: "call_0", Text: "first"})
	mustAppend(t, l, AssistantMessage, MessageData{Message: callMessage("other")})
	appendEvent(t, l, ToolResult, ToolResultData{ToolCallID: "other", Text: "unrelated"})
	mustAppend(t, l, AssistantMessage, MessageData{Message: callMessage("call_0")})
	second := appendEvent(t, l, ToolResult, ToolResultData{ToolCallID: "call_0", Text: "second"})
	compactResult(t, l, second, "call_0", "SHORT")

	got, err := l.CompactedResults()
	if err != nil {
		t.Fatal(err)
	}
	if want := []llm.ResultKey{{ID: "call_0", Occurrence: 2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CompactedResults = %v, want %v", got, want)
	}
	calls, err := l.CompactedCalls()
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("CompactedCalls = %v: a record that names its result must not claim every result with the id", calls)
	}

	seqs, err := l.ResultSeqs([]llm.ResultKey{{ID: "call_0", Occurrence: 2}, {ID: "call_0", Occurrence: 1}, {ID: "other", Occurrence: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if seqs[0] != second.Seq || seqs[1] >= seqs[0] || seqs[2] <= seqs[1] || seqs[2] >= seqs[0] {
		t.Fatalf("ResultSeqs = %v, want the second call_0 at %d with the others in log order", seqs, second.Seq)
	}
	if _, err := l.ResultSeqs([]llm.ResultKey{{ID: "call_0", Occurrence: 3}}); err == nil {
		t.Fatal("ResultSeqs resolved a result that is not in the log")
	}
}

// The ledger survives a rebuild from the persisted events: the key is derived,
// not held, so a restored log answers as the live one did.
func TestCompactedResultsSurviveRestore(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, UserMessage, MessageData{Message: user("go")})
	mustAppend(t, l, AssistantMessage, MessageData{Message: callMessage("call_0")})
	appendEvent(t, l, ToolResult, ToolResultData{ToolCallID: "call_0", Text: "first"})
	mustAppend(t, l, AssistantMessage, MessageData{Message: callMessage("call_0")})
	second := appendEvent(t, l, ToolResult, ToolResultData{ToolCallID: "call_0", Text: "second"})
	compactResult(t, l, second, "call_0", "SHORT")

	restored, err := RestoreLog(l.Events())
	if err != nil {
		t.Fatal(err)
	}
	live, _ := l.CompactedResults()
	again, err := restored.CompactedResults()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live, again) || len(again) != 1 {
		t.Fatalf("live %v, restored %v", live, again)
	}
}

// A compaction that names a result must name one this log holds, under that
// call id. A record that named the wrong event would apply to nothing, or to
// the wrong result, with nothing to say so.
func TestCompactionNamingTheWrongResultIsRefused(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, UserMessage, MessageData{Message: user("go")})
	prompt := appendEvent(t, l, UserMessage, MessageData{Message: user("again")})
	mustAppend(t, l, AssistantMessage, MessageData{Message: callMessage("call_0")})
	result := appendEvent(t, l, ToolResult, ToolResultData{ToolCallID: "call_0", Text: "r"})
	before := l.Len()

	for name, data := range map[string]map[string]any{
		"not a tool result": {"tool_call_id": "call_0", "result_seq": prompt.Seq, "text": "x"},
		"another call's":    {"tool_call_id": "other", "result_seq": result.Seq, "text": "x"},
		"not in the log":    {"tool_call_id": "call_0", "result_seq": result.Seq + 50, "text": "x"},
	} {
		if _, err := l.Append(ToolResultCompacted, data); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if l.Len() != before {
		t.Fatalf("a refused compaction still wrote %d event(s)", l.Len()-before)
	}
}

// The persisted field is result_seq, written only when a result is named, so a
// record without one is byte-compatible with what older builds wrote.
func TestCompactionDataFieldIsOmittedWhenNoResultIsNamed(t *testing.T) {
	old, err := json.Marshal(CompactionData{ToolCallID: "c", Text: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != `{"tool_call_id":"c","text":"t","from_bytes":0,"to_bytes":0}` {
		t.Fatalf("an id-only record changed shape: %s", old)
	}
	named, err := json.Marshal(CompactionData{ToolCallID: "c", ResultSeq: 7, Text: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if string(named) != `{"tool_call_id":"c","result_seq":7,"text":"t","from_bytes":0,"to_bytes":0}` {
		t.Fatalf("a named record has the wrong shape: %s", named)
	}
}
