package session

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// staleFixture appends n tool rounds, each an assistant message that opens with
// signed reasoning and calls a tool, then the tool's result. Round i uses call
// id c<i> and signature sig-<i>.
func staleFixture(t *testing.T, log *Log, n int) {
	t.Helper()
	if _, err := log.Append(UserMessage, MessageData{Message: llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		staleRound(t, log, i)
	}
}

func staleRound(t *testing.T, log *Log, i int) {
	t.Helper()
	id := llm.CallID(fmt.Sprintf("c%d", i))
	if _, err := log.Append(AssistantMessage, MessageData{Message: llm.Message{
		Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "think", Signature: fmt.Sprintf("sig-%d", i)},
			llm.TextBlock{Text: fmt.Sprintf("step %d", i)},
			llm.ToolCallBlock{ID: id, Name: "read", Arguments: json.RawMessage(`{}`)},
		}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(ToolResult, ToolResultData{ToolCallID: id, Text: fmt.Sprintf("output %d", i)}); err != nil {
		t.Fatal(err)
	}
}

func compact(t *testing.T, log *Log, id llm.CallID, keyed bool) {
	t.Helper()
	data := CompactionData{ToolCallID: id, Text: "short"}
	if keyed {
		seqs, err := log.ResultSeqs([]llm.ResultKey{{ID: id, Occurrence: 1}})
		if err != nil {
			t.Fatal(err)
		}
		data.ResultSeq = seqs[0]
	}
	if _, err := log.Append(ToolResultCompacted, data); err != nil {
		t.Fatal(err)
	}
}

// signaturesOf lists the signatures still carried, by assistant message index.
func signaturesOf(msgs []llm.Message) map[int]string {
	out := map[int]string{}
	for i, m := range msgs {
		for _, b := range m.Content {
			if r, ok := b.(llm.ReasoningBlock); ok && r.Signature != "" {
				out[i] = r.Signature
			}
		}
	}
	return out
}

func strip(t *testing.T, log *Log) ([]llm.Message, int) {
	t.Helper()
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	out, n, err := log.StripStaleReasoning(msgs)
	if err != nil {
		t.Fatal(err)
	}
	return out, n
}

// A log nobody compacted is returned as it came, the same slice and not a copy:
// this runs on every step of every turn.
func TestAnUneditedLogIsHandedBackUntouched(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 3)
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	out, n, err := log.StripStaleReasoning(msgs)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want nothing stripped", n, err)
	}
	if len(out) != len(msgs) || &out[0] != &msgs[0] {
		t.Error("an unedited history was copied")
	}
	if got := len(signaturesOf(out)); got != 3 {
		t.Errorf("%d signed blocks survive, want 3", got)
	}
}

// Messages: 0 user, then (assistant, results) per round at 1/2, 3/4, 5/6, 7/8.
// A compaction of round 1's result, recorded after round 2 and before round 3.
// Everything signed from the edited position onward goes, including round 3's
// block, which was recorded after the compaction. Round 3 may well have been
// signed against the shortened history, but each block records the one before
// it and the documented removals that keep later blocks valid are only a leading
// run (shared/model-migration.md, Breaking change 3), so removing round 2's block
// and keeping round 3's would be a removal from the middle. An earlier version
// of this test kept round 3 (7: sig-3); that expectation was wrong. What stays is
// what precedes the edited result: round 0, and round 1's own reasoning.
func TestOnlyReasoningBehindTheEditAndBeforeItIsStripped(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 3)
	compact(t, log, "c1", true)
	staleRound(t, log, 3)

	out, n := strip(t, log)
	got := signaturesOf(out)
	want := map[int]string{1: "sig-0", 3: "sig-1"}
	if n != 2 || len(got) != len(want) {
		t.Fatalf("stripped %d, surviving %v, want 1 stripped and %v", n, got, want)
	}
	for i, sig := range want {
		if got[i] != sig {
			t.Errorf("message %d carries %q, want %q (all: %v)", i, got[i], sig, got)
		}
	}
	// Only reasoning goes: round 2's text and call survive.
	if len(out[5].Content) != 2 || out[5].Text() != "step 2" || len(out[5].ToolCalls()) != 1 {
		t.Errorf("stripping touched more than reasoning: %+v", out[5].Content)
	}
}

// A record that names only a call id applies to every result with that id,
// which is what a journal written before records named their result holds. The
// earliest such result is where the history first moved.
func TestACallIDRecordStripsFromTheEarliestResultItNames(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 3)
	compact(t, log, "c0", false)
	out, n := strip(t, log)
	got := signaturesOf(out)
	if n != 2 || len(got) != 1 || got[1] != "sig-0" {
		t.Fatalf("stripped %d, surviving %v, want only round 0's reasoning", n, got)
	}
}

// Two edits strip the union of what each invalidated.
func TestTwoEditsStripTheUnion(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 4)
	compact(t, log, "c2", true) // invalidates round 3
	compact(t, log, "c0", true) // invalidates rounds 1, 2 and 3
	out, n := strip(t, log)
	got := signaturesOf(out)
	if n != 3 || len(got) != 1 || got[1] != "sig-0" {
		t.Fatalf("stripped %d, surviving %v, want only round 0", n, got)
	}
}

// A compaction naming a result the log does not hold is ignored by the
// projection, so it edits nothing and strips nothing.
func TestARecordForAMissingResultStripsNothing(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 3)
	if _, err := log.Append(ToolResultCompacted, CompactionData{ToolCallID: "nope", Text: "short"}); err != nil {
		t.Fatal(err)
	}
	if _, n := strip(t, log); n != 0 {
		t.Errorf("stripped %d blocks for an edit that never happened", n)
	}
}

// Positions are what say which message is which. When the messages handed in
// are not the log's projection (a listener dropped one), the answer is not
// known, and the safe reading is that every signed block may be stale.
func TestAHistoryThatIsNotTheProjectionLosesAllItsSignedReasoning(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 3)
	compact(t, log, "c2", true)
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	out, n, err := log.StripStaleReasoning(msgs[1:])
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || len(signaturesOf(out)) != 0 {
		t.Errorf("stripped %d, surviving %v, want all three gone", n, signaturesOf(out))
	}
}

// An encrypted block carries its data in the signature field, so it is stripped
// by the same rule, and the log keeps what it recorded.
func TestRedactedReasoningIsStrippedAndTheLogIsLeftAlone(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 1)
	if _, err := log.Append(AssistantMessage, MessageData{Message: llm.Message{
		Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Signature: "encrypted", Redacted: true},
			llm.ToolCallBlock{ID: "c9", Name: "read", Arguments: json.RawMessage(`{}`)},
		}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(ToolResult, ToolResultData{ToolCallID: "c9", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	compact(t, log, "c0", true)
	before := len(log.Events())
	out, n := strip(t, log)
	if n != 1 {
		t.Fatalf("stripped %d, want the redacted block", n)
	}
	// Round 0's own reasoning precedes the result that was shortened.
	if got := signaturesOf(out); len(got) != 1 || got[1] != "sig-0" {
		t.Errorf("surviving signed blocks = %v, want only round 0's", got)
	}
	if len(log.Events()) != before {
		t.Error("stripping changed the log")
	}
	again, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if got := signaturesOf(again); len(got) != 2 {
		t.Errorf("the projection lost reasoning it should still hold: %v", got)
	}
}
