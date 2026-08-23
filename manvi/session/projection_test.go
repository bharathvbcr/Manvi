package session

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"manvi/llm"
)

// TestIncrementalProjectionMatchesFullReplay pins the equivalence between the
// warm projection maintained by Append and the cold full replay: two logs
// carrying identical events must derive byte-identical histories, whatever
// order message, tool-result, chunk, and compaction events arrived in.
func TestIncrementalProjectionMatchesFullReplay(t *testing.T) {
	rng := rand.New(rand.NewSource(7))

	for round := 0; round < 30; round++ {
		var events []Event
		// One log is built incrementally through Append; the other is
		// restored from the resulting event list, which exercises only the
		// cold replay path.
		incremental := NewLog()

		nextCall := llm.CallID("call-0")
		callN := 0
		appendEvent := func(evt Type, payload any) {
			ev, err := incremental.Append(evt, payload)
			if err != nil {
				t.Fatalf("round %d: append %s: %v", round, evt, err)
			}
			events = append(events, ev)
		}

		for i := 0; i < 60; i++ {
			switch n := rng.Intn(6); {
			case n == 0:
				appendEvent(UserMessage, MessageData{Message: llm.Message{
					Role:    llm.RoleUser,
					Content: []llm.ContentBlock{llm.TextBlock{Text: fmt.Sprintf("u%d", i)}},
				}})
			case n == 1:
				callN++
				nextCall = llm.CallID(fmt.Sprintf("call-%d", callN))
				appendEvent(AssistantMessage, MessageData{Message: llm.Message{
					Role: llm.RoleAssistant,
					Content: []llm.ContentBlock{llm.ToolCallBlock{
						ID: nextCall, Name: "tool", Arguments: []byte(`{}`),
					}},
				}})
			case n == 2 && callN > 0:
				appendEvent(ToolResult, ToolResultData{
					ToolCallID: nextCall,
					Text:       fmt.Sprintf("result body %d", i),
				})
			case n == 3:
				// Per-token noise: history-neutral, previously the reason a
				// long session paid quadratic derive costs.
				appendEvent(AssistantChunk, llm.Chunk{Kind: llm.ChunkText, Text: "tok"})
			case n == 4 && callN > 0:
				appendEvent(ToolResultCompacted, CompactionData{
					ToolCallID: nextCall,
					Text:       fmt.Sprintf("compacted %d", i),
				})
			default:
				appendEvent(StepStart, nil)
			}
		}

		warm, err := incremental.DeriveMessages()
		if err != nil {
			t.Fatalf("round %d: warm derive: %v", round, err)
		}
		restored, err := RestoreLog(events)
		if err != nil {
			t.Fatalf("round %d: restore: %v", round, err)
		}
		cold, err := restored.DeriveMessages()
		if err != nil {
			t.Fatalf("round %d: cold derive: %v", round, err)
		}

		a, _ := json.Marshal(warm)
		b, _ := json.Marshal(cold)
		if string(a) != string(b) {
			t.Fatalf("round %d: incremental projection diverged from full replay:\n warm=%s\n cold=%s",
				round, a, b)
		}

		// And a second warm derive of the same log is stable.
		again, _ := incremental.DeriveMessages()
		c, _ := json.Marshal(again)
		if string(a) != string(c) {
			t.Fatalf("round %d: warm derive is not idempotent:\n first=%s\n again=%s", round, a, c)
		}
	}
}

// TestCompactionAppliesWhereverItLands: a compaction event appended after its
// tool result changes how that result replays, regardless of position — the
// rule the original algorithm encoded and the incremental cache must honour
// by invalidating.
func TestCompactionAppliesWhereverItLands(t *testing.T) {
	l := NewLog()
	if _, err := l.Append(AssistantMessage, MessageData{Message: llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.ToolCallBlock{
			ID: "call-1", Name: "tool", Arguments: []byte(`{}`),
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(ToolResult, ToolResultData{ToolCallID: "call-1", Text: "very long output"}); err != nil {
		t.Fatal(err)
	}

	before, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if got := visibleText(before); got != "very long output" {
		t.Fatalf("pre-compaction derive = %q", got)
	}

	if _, err := l.Append(ToolResultCompacted, CompactionData{ToolCallID: "call-1", Text: "summary"}); err != nil {
		t.Fatal(err)
	}

	after, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if got := visibleText(after); got != "summary" {
		t.Fatalf("post-compaction derive = %q; the invalidated cache did not apply the compaction", got)
	}
}

// TestWarmDeriveStaysFlatAsChunksGrow is the performance guard. The log below
// carries 20k history-neutral chunk events; under the old re-decode-everything
// shape each derive walked all of them, so a long streaming session paid
// quadratic cost. The warm path touches none of them; the ceiling here is
// orders of magnitude above the warm cost and far below what a single cold
// replay of this log takes.
func TestWarmDeriveStaysFlatAsChunksGrow(t *testing.T) {
	l := NewLog()
	mustAppend := func(evt Type, payload any) {
		if _, err := l.Append(evt, payload); err != nil {
			t.Fatal(err)
		}
	}
	mustAppend(UserMessage, MessageData{Message: llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}},
	}})
	for i := 0; i < 20000; i++ {
		mustAppend(AssistantChunk, llm.Chunk{Kind: llm.ChunkText, Text: "t"})
	}
	mustAppend(AssistantMessage, MessageData{Message: llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}},
	}})

	if _, err := l.DeriveMessages(); err != nil { // warm
		t.Fatal(err)
	}

	start := time.Now()
	for i := 0; i < 100; i++ {
		if _, err := l.DeriveMessages(); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)

	if elapsed > 250*time.Millisecond {
		t.Fatalf("100 warm derives over a 20k-event log took %v; the projection is being rebuilt", elapsed)
	}
}

func visibleText(msgs []llm.Message) string {
	out := ""
	var walk func(blocks []llm.ContentBlock)
	walk = func(blocks []llm.ContentBlock) {
		for _, b := range blocks {
			switch v := b.(type) {
			case llm.TextBlock:
				out += v.Text
			case llm.ToolResultBlock:
				walk(v.Content)
			}
		}
	}
	for _, m := range msgs {
		walk(m.Content)
	}
	return out
}
