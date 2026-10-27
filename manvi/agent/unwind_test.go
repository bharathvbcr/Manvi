package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// callLedger reads the projected history the next request would be built from
// and returns, per tool call, how many results answer it.
func callLedger(t *testing.T, log *session.Log) (answers map[llm.CallID]int, results map[llm.CallID]llm.ToolResultBlock) {
	t.Helper()
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatalf("DeriveMessages: %v", err)
	}
	answers = map[llm.CallID]int{}
	results = map[llm.CallID]llm.ToolResultBlock{}
	for _, m := range msgs {
		for _, b := range m.Content {
			switch x := b.(type) {
			case llm.ToolCallBlock:
				answers[x.ID] += 0
			case llm.ToolResultBlock:
				answers[x.ToolCallID]++
				results[x.ToolCallID] = x
			}
		}
	}
	return answers, results
}

// requireEveryCallAnsweredOnce is the property the next request depends on: a
// history with a tool_use no tool_result answers, or one answered twice, is
// refused by the provider.
func requireEveryCallAnsweredOnce(t *testing.T, log *session.Log, wantCalls int) map[llm.CallID]llm.ToolResultBlock {
	t.Helper()
	answers, results := callLedger(t, log)
	if len(answers) != wantCalls {
		t.Fatalf("history holds %d tool calls, want %d", len(answers), wantCalls)
	}
	for id, n := range answers {
		if n != 1 {
			t.Fatalf("tool call %q is answered %d times, want exactly once", id, n)
		}
	}
	return results
}

func answerText(r llm.ToolResultBlock) string {
	var b strings.Builder
	for _, c := range r.Content {
		if t, ok := c.(llm.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func runRecovering(h *harness) (recovered any) {
	defer func() { recovered = recover() }()
	_, _ = h.loop.Run(context.Background(), userMsg("go"))
	return nil
}

// A panic between the assistant's tool_use being logged and its tool_result (a
// ToolsDispatching listener defect) used to persist a history the next request
// could not be built from, and the save overwrote the last good copy. Every
// exit from a step now answers the calls it left open.
func TestAPanicMidStepAnswersTheOpenCalls(t *testing.T) {
	h := build(t, []replay.Turn{toolTurn("c1", "probe", `{}`), textTurn("done")}, nil)
	ran := 0
	registerCounter(t, h, "probe", &ran)
	if _, err := bus.OnSerial(h.bus, func(context.Context, *ToolsDispatching) error {
		panic("listener defect")
	}); err != nil {
		t.Fatal(err)
	}
	if runRecovering(h) == nil {
		t.Fatal("the panic did not reach the caller; the loop must not swallow it")
	}
	results := requireEveryCallAnsweredOnce(t, h.log, 1)
	got := results["c1"]
	if !got.IsError || !strings.Contains(answerText(got), "interrupted") {
		t.Fatalf("the open call was answered with %+v, want an error result saying it was interrupted", got)
	}
	if ran != 0 {
		t.Fatalf("the tool ran %d times although the dispatch panicked first", ran)
	}
	requireBalanced(t, h.log)

	// The next turn on the same log builds a valid request and completes.
	if _, err := h.loop.Run(context.Background(), userMsg("again")); err != nil {
		t.Fatalf("the turn after the panic failed: %v", err)
	}
	requireEveryCallAnsweredOnce(t, h.log, 1)
	requireBalanced(t, h.log)
}

// The same hole without a panic: a listener that refuses the dispatch ends the
// turn after the tool_use is logged.
func TestARefusedDispatchAnswersTheOpenCalls(t *testing.T) {
	h := build(t, []replay.Turn{toolTurn("c1", "probe", `{}`), textTurn("done")}, nil)
	ran := 0
	registerCounter(t, h, "probe", &ran)
	refusal := errors.New("baseline not ready")
	if _, err := bus.OnSerial(h.bus, func(context.Context, *ToolsDispatching) error { return refusal }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.loop.Run(context.Background(), userMsg("go")); !errors.Is(err, refusal) {
		t.Fatalf("Run returned %v, want the listener's refusal", err)
	}
	requireEveryCallAnsweredOnce(t, h.log, 1)
	requireBalanced(t, h.log)
}

// Calls the step already answered are not answered again: the closer answers
// only what is still open, and answers it once however often it runs. The
// state is built the way the loop builds it, through its own logging.
func TestAnUnwindNeverAnswersACallTwice(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("unused")}, nil)
	l, log := h.loop, h.log
	if _, err := log.Append(session.UserMessage, session.MessageData{Message: userMsg("go")}); err != nil {
		t.Fatal(err)
	}
	calls := []llm.ToolCallBlock{
		{ID: "a", Name: "fine", Arguments: json.RawMessage(`{}`)},
		{ID: "b", Name: "fine", Arguments: json.RawMessage(`{"n":1}`)},
		{ID: "c", Name: "fine", Arguments: json.RawMessage(`{"n":2}`)},
	}
	blocks := make([]llm.ContentBlock, len(calls))
	for i, c := range calls {
		blocks[i] = c
		l.openCalls = append(l.openCalls, c.ID)
	}
	if _, err := log.Append(session.AssistantMessage, session.MessageData{
		Message: llm.Message{Role: llm.RoleAssistant, Content: blocks},
	}); err != nil {
		t.Fatal(err)
	}
	// The first call completes normally.
	var out Outcome
	if err := l.recordCall(&out, calls[0], tools.Result{Text: "ok"}); err != nil {
		t.Fatal(err)
	}

	l.answerOpenCalls()
	l.answerOpenCalls() // an unwind that runs twice must find nothing left
	results := requireEveryCallAnsweredOnce(t, log, 3)
	if r := results["a"]; r.IsError || answerText(r) != "ok" {
		t.Fatalf("the call answered before the unwind was rewritten: %+v", r)
	}
	for _, id := range []llm.CallID{"b", "c"} {
		if r := results[id]; !r.IsError || !strings.Contains(answerText(r), "interrupted") {
			t.Fatalf("call %q was answered with %+v", id, r)
		}
	}
	if len(l.openCalls) != 0 {
		t.Fatalf("open calls left after the unwind: %v", l.openCalls)
	}
}
