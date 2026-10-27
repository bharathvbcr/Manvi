package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// compactOnce is one compaction pass the way Loop.compact runs it: derive,
// read the ledger from the log, plan, apply. It returns the plan.
func compactOnce(t *testing.T, log *session.Log, budget Budget) CompactionPlan {
	t.Helper()
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	already, err := log.CompactedCalls()
	if err != nil {
		t.Fatal(err)
	}
	done, err := log.CompactedResults()
	if err != nil {
		t.Fatal(err)
	}
	plan := PlanCompactionCalibrated(msgs, "sys", nil, budget, already, nil, done...)
	if err := plan.Apply(log); err != nil {
		t.Fatal(err)
	}
	return plan
}

func appendResult(t *testing.T, log *session.Log, id llm.CallID, text string) {
	t.Helper()
	for _, step := range []struct {
		typ     session.Type
		payload any
	}{
		{session.AssistantMessage, session.MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCallBlock{ID: id, Name: "grep", Arguments: []byte(`{}`)}}}}},
		{session.ToolResult, session.ToolResultData{ToolCallID: id, Text: text}},
	} {
		if _, err := log.Append(step.typ, step.payload); err != nil {
			t.Fatal(err)
		}
	}
}

func resultTexts(t *testing.T, log *session.Log) []string {
	t.Helper()
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, m := range msgs {
		for _, b := range m.Content {
			if tr, ok := b.(llm.ToolResultBlock); ok {
				out = append(out, firstText(t, tr))
			}
		}
	}
	return out
}

// firstText is the text of a tool result's first block, which these tests
// build as text.
func firstText(t *testing.T, tr llm.ToolResultBlock) string {
	t.Helper()
	if len(tr.Content) == 0 {
		t.Fatalf("tool result %s has no content", tr.ToolCallID)
	}
	text, ok := tr.Content[0].(llm.TextBlock)
	if !ok {
		t.Fatalf("tool result %s starts with %T, want text", tr.ToolCallID, tr.Content[0])
	}
	return text.Text
}

func newIdentityLog(t *testing.T) *session.Log {
	t.Helper()
	log := session.NewLog()
	for _, step := range []struct {
		typ     session.Type
		payload any
	}{
		{session.TurnStart, nil},
		{session.UserMessage, session.MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}}}},
	} {
		if _, err := log.Append(step.typ, step.payload); err != nil {
			t.Fatal(err)
		}
	}
	return log
}

// padTail adds results with unique ids so the earlier ones fall outside the
// protected tail and are eligible.
func padTail(t *testing.T, log *session.Log, prefix string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		appendResult(t, log, llm.CallID(fmt.Sprintf("%s_%d", prefix, i)), "ok")
	}
}

// Two results that share a call id are two results. Each is compacted from its
// own text and replays its own compaction; neither shows the other's.
func TestReusedCallIDResultsAreCompactedIndependently(t *testing.T) {
	log := newIdentityLog(t)
	appendResult(t, log, "call_0", bigOutput(300, "AAAA"))
	appendResult(t, log, "call_0", bigOutput(300, "BBBB"))
	padTail(t, log, "tail", 4)

	plan := compactOnce(t, log, testBudget(4096))
	if len(plan.Steps) < 2 {
		t.Fatalf("both results are eligible and over budget; plan has %d step(s)", len(plan.Steps))
	}
	texts := resultTexts(t, log)
	if !strings.Contains(texts[0], "AAAA") || strings.Contains(texts[0], "BBBB") {
		t.Errorf("first result does not replay its own compaction:\n%s", texts[0])
	}
	if !strings.Contains(texts[1], "BBBB") || strings.Contains(texts[1], "AAAA") {
		t.Errorf("second result does not replay its own compaction:\n%s", texts[1])
	}
	if !strings.Contains(texts[0], "omitted") || !strings.Contains(texts[1], "omitted") {
		t.Errorf("both results should carry the elision notice")
	}
}

// A result compacted earlier must not make a later result that reuses its call
// id look compacted: the later one is over budget and has to be shortened, and
// the earlier one keeps exactly the text it already has.
func TestLaterResultWithReusedCallIDIsStillCompacted(t *testing.T) {
	log := newIdentityLog(t)
	appendResult(t, log, "call_0", bigOutput(300, "AAAA"))
	padTail(t, log, "early", 6)
	compactOnce(t, log, testBudget(4096))
	before := resultTexts(t, log)[0]
	if !strings.Contains(before, "omitted") {
		t.Fatalf("setup: first result not compacted:\n%s", before)
	}

	appendResult(t, log, "call_0", bigOutput(300, "BBBB"))
	padTail(t, log, "late", 6)
	plan := compactOnce(t, log, testBudget(4096))

	texts := resultTexts(t, log)
	if texts[0] != before {
		t.Errorf("an already-compacted result changed text; the prompt prefix moved")
	}
	second := texts[7] // after the first result and its six padding results
	if !strings.Contains(second, "BBBB") || !strings.Contains(second, "omitted") {
		t.Fatalf("the reused-id result was never compacted (plan: %d step(s), insufficient=%v):\n%s", len(plan.Steps), plan.Insufficient, second)
	}
}

// A step that names a result the log does not hold fails the Apply before
// anything is written: a record that named the wrong result would apply to
// nothing, or to the wrong one, and say nothing about it.
func TestApplyRefusesAStepForAResultTheLogDoesNotHold(t *testing.T) {
	log := newIdentityLog(t)
	appendResult(t, log, "call_0", "only one")
	before := log.Len()
	plan := CompactionPlan{Steps: []CompactionStep{
		{ToolCallID: "call_0", Occurrence: 1, Text: "short"},
		{ToolCallID: "call_0", Occurrence: 2, Text: "short"},
	}}
	if err := plan.Apply(log); err == nil {
		t.Fatal("Apply accepted a step for a second call_0 result the log does not hold")
	}
	if log.Len() != before {
		t.Fatalf("a refused Apply still wrote %d event(s)", log.Len()-before)
	}
}

// A step without an occurrence is the old form and still means every result
// with the id.
func TestStepWithoutAnOccurrenceStillCompactsByCallID(t *testing.T) {
	log := newIdentityLog(t)
	appendResult(t, log, "call_0", "first")
	appendResult(t, log, "call_0", "second")
	plan := CompactionPlan{Steps: []CompactionStep{{ToolCallID: "call_0", Text: "SHORT"}}}
	if err := plan.Apply(log); err != nil {
		t.Fatal(err)
	}
	for i, text := range resultTexts(t, log) {
		if text != "SHORT" {
			t.Errorf("result %d is %q, want the id-wide compaction", i, text)
		}
	}
}

// An earlier stage of the pre-step waterfall may hand compaction a history with
// its head cut off. A plan counted over that history names results by a place
// the log does not share, so with a reused call id it shortened the wrong ones.
// Compaction plans over the log's own projection then, and every result keeps
// replaying its own text.
func TestCompactionOverATruncatedHistoryNamesTheRightResults(t *testing.T) {
	h := overflowLoop(t, 100, nil)
	for _, p := range []any{
		session.MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}}},
	} {
		if _, err := h.log.Append(session.UserMessage, p); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 12; i++ {
		appendResult(t, h.log, "call_0", fmt.Sprintf("RESULT-%02d ", i)+strings.Repeat("x y z ", 12000))
	}
	msgs, err := h.log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if msgs == nil {
		t.Fatal("the log projects no history")
	}
	cut := msgs[1+8:] // the prompt and the first four call/result pairs are gone

	if _, err := h.loop.compact(cut); err != nil {
		t.Fatal(err)
	}
	texts := resultTexts(t, h.log)
	compacted := 0
	for i, text := range texts {
		if !strings.HasPrefix(text, fmt.Sprintf("RESULT-%02d ", i)) {
			t.Errorf("result %d replays another result's text: %.20q", i, text)
		}
		if strings.Contains(text, "omitted to fit") {
			compacted++
		}
	}
	if compacted == 0 {
		t.Fatal("nothing was compacted; the fixture is not over budget")
	}
	if !strings.Contains(texts[0], "omitted to fit") {
		t.Errorf("the oldest result was not compacted although it is the first the planner reaches")
	}
}
