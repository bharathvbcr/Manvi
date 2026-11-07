package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// When the server clears tool results the loop plans nothing, but it must still
// say so when clearing cannot be enough: the history that is left after the
// server keeps only its most recent tool uses is still over Manvi's budget. That
// is the signal the client-side planner gave as Insufficient, recorded as a
// ContextOverflow event and flagged on the loop, and it must not go quiet just
// because the provider changed who shrinks the history.

// serverSideOverflowLoop builds an Anthropic loop with a tool offered, over a log of n
// tool rounds whose results are each lines lines of output, ending in a user
// turn. The window is small so a few rounds are over budget.
func serverSideOverflowLoop(t *testing.T, n, lines int) (*Loop, *session.Log, []llm.Message) {
	t.Helper()
	b := bus.New()
	log := session.NewLog()
	reg := tools.NewRegistry(b)
	if err := reg.Register(tools.Tool{
		Schema:  llm.ToolSchema{Name: "echo", Description: "echo", InputSchema: []byte(`{"type":"object"}`)},
		Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "ok"} },
	}); err != nil {
		t.Fatal(err)
	}
	provider := windowed{Provider: anthropic.New("http://unused", adaptertest.Secret("k")), window: 8192}
	loop, err := NewLoop(Config{
		Provider: provider, Model: "claude-opus-5-5", SystemPrompt: "p", MaxSteps: 4, MaxTokens: 1024,
	}, b, log, reg)
	if err != nil {
		t.Fatal(err)
	}
	appendMsg := func(role llm.Role, typ session.Type, blocks ...llm.ContentBlock) {
		if _, err := log.Append(typ, session.MessageData{Message: llm.Message{Role: role, Content: blocks}}); err != nil {
			t.Fatal(err)
		}
	}
	appendMsg(llm.RoleUser, session.UserMessage, llm.TextBlock{Text: "go"})
	for i := 0; i < n; i++ {
		id := llm.CallID(fmt.Sprintf("toolu_%d", i))
		appendMsg(llm.RoleAssistant, session.AssistantMessage, llm.ToolCallBlock{ID: id, Name: "echo", Arguments: json.RawMessage(`{}`)})
		if _, err := log.Append(session.ToolResult, session.ToolResultData{ToolCallID: id, Text: bigOutput(lines, fmt.Sprintf("r%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	appendMsg(llm.RoleUser, session.UserMessage, llm.TextBlock{Text: "continue"})
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	return loop, log, msgs
}

func overflowEvents(log *session.Log) int {
	n := 0
	for _, e := range log.Events() {
		if e.Type == session.ContextOverflow {
			n++
		}
	}
	return n
}

// Three tool results that are each bigger than the whole budget: nothing the
// server clears can bring this under, so the overflow is recorded.
func TestServerSideClearingThatCannotFitIsRecordedAsAnOverflow(t *testing.T) {
	loop, log, msgs := serverSideOverflowLoop(t, 3, 600)
	if !ServerSideClearing(loop.cfg.Provider, loop.schemas()) {
		t.Fatal("the fixture's request does not take the server-side path")
	}
	if got := CountRequestTokens(loop.cfg.SystemPrompt, loop.schemas(), msgs); got <= loop.Budget().Threshold() {
		t.Fatalf("the fixture is not over budget (%d <= %d)", got, loop.Budget().Threshold())
	}
	if _, err := loop.compact(msgs); err != nil {
		t.Fatal(err)
	}
	if overflowEvents(log) != 1 || !loop.overflowed {
		t.Errorf("overflow events=%d flagged=%v, want one event and the flag set", overflowEvents(log), loop.overflowed)
	}
	if n := compactionEvents(log); n != 0 {
		t.Errorf("%d results were compacted client-side on the server-side path", n)
	}
}

// Many small results are over budget together but the server can clear all but
// three of them, which fits: no overflow is reported, and no edit is made.
func TestServerSideClearingThatFitsReportsNothing(t *testing.T) {
	loop, log, msgs := serverSideOverflowLoop(t, 40, 30)
	if got := CountRequestTokens(loop.cfg.SystemPrompt, loop.schemas(), msgs); got <= loop.Budget().Threshold() {
		t.Fatalf("the fixture is not over budget (%d <= %d); it would pass without exercising clearing", got, loop.Budget().Threshold())
	}
	if _, err := loop.compact(msgs); err != nil {
		t.Fatal(err)
	}
	if overflowEvents(log) != 0 || loop.overflowed {
		t.Errorf("overflow events=%d flagged=%v for a history the server can clear under budget", overflowEvents(log), loop.overflowed)
	}
	if compactionEvents(log) != 0 {
		t.Error("a client-side edit was made on the server-side path")
	}
}

// A history already under budget is never reported.
func TestAnUnderBudgetServerSideHistoryReportsNothing(t *testing.T) {
	loop, log, msgs := serverSideOverflowLoop(t, 2, 5)
	if _, err := loop.compact(msgs); err != nil {
		t.Fatal(err)
	}
	if overflowEvents(log) != 0 || loop.overflowed {
		t.Error("an under-budget history was reported as an overflow")
	}
}

// The keep the loop assumes the server applies is the keep the adapter asks for.
func TestTheLoopAssumesTheKeepTheAdapterAsksFor(t *testing.T) {
	edit := anthropic.ContextEditsFor("claude-opus-5-5", 1024)[0]
	if edit.Keep == nil || edit.Keep.Type != "tool_uses" || edit.Keep.Value != serverKeepToolUses {
		t.Errorf("adapter keep = %+v, loop assumes %d tool uses", edit.Keep, serverKeepToolUses)
	}
}
