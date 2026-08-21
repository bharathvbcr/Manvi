package agent

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"manvi/llm"
	"manvi/session"
)

// buildTurn drives the real compaction path over many steps and returns, for
// each step, the serialized prefix the model would have seen.
func buildTurn(t *testing.T, steps int, budget Budget, resultLines func(int) int) []string {
	t.Helper()
	log := session.NewLog()
	if _, err := log.Append(session.TurnStart, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.UserMessage, session.MessageData{
		Message: llm.Message{Role: llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: "do the work"}}},
	}); err != nil {
		t.Fatal(err)
	}

	done := map[llm.CallID]struct{}{}
	var prefixes []string

	for step := 1; step <= steps; step++ {
		id := llm.CallID(fmt.Sprintf("c%03d", step))
		if _, err := log.Append(session.AssistantMessage, session.MessageData{
			Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				llm.ToolCallBlock{ID: id, Name: "grep", Arguments: json.RawMessage(`{}`)}}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(session.ToolResult, session.ToolResultData{
			ToolCallID: id, Text: bigOutput(resultLines(step), fmt.Sprintf("s%d", step)),
		}); err != nil {
			t.Fatal(err)
		}

		msgs, err := log.DeriveMessages()
		if err != nil {
			t.Fatal(err)
		}
		plan := PlanCompaction(msgs, "sys", nil, budget, done)
		if !plan.Empty() {
			if err := plan.Apply(log); err != nil {
				t.Fatal(err)
			}
			for _, s := range plan.Steps {
				done[s.ToolCallID] = struct{}{}
			}
			msgs, err = log.DeriveMessages()
			if err != nil {
				t.Fatal(err)
			}
		}

		// The invariant must hold on every single step, not just the first.
		if err := session.AssertModelVisible(log, msgs); err != nil {
			t.Fatalf("step %d violated model-visible-means-logged: %v", step, err)
		}
		prefixes = append(prefixes, serialize(msgs))
	}
	return prefixes
}

func serialize(msgs []llm.Message) string {
	out := ""
	for _, m := range msgs {
		out += string(m.Role) + "\x00"
		for _, b := range m.Content {
			switch bl := b.(type) {
			case llm.TextBlock:
				out += bl.Text
			case llm.ToolCallBlock:
				out += bl.Name + string(bl.Arguments)
			case llm.ToolResultBlock:
				for _, in := range bl.Content {
					if tb, ok := in.(llm.TextBlock); ok {
						out += tb.Text
					}
				}
			}
			out += "\x01"
		}
		out += "\x02"
	}
	return out
}

// commonPrefix is what a server's KV cache can reuse between two requests.
func commonPrefix(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// Over a long turn, each step's prompt must extend the previous one — except at
// a compaction, which is allowed to break the prefix once. The count of breaks
// is the count of full re-prefills the server will pay for, and on a 4-bit 27B
// each one measured at two minutes for a 14.7k-token prompt.
func TestALongTurnBreaksThePrefixOnlyWhenItCompacts(t *testing.T) {
	const steps = 60
	budget := Budget{ContextWindow: 16000, ReservedOutput: 2048, Overhead: 512}
	prefixes := buildTurn(t, steps, budget, func(int) int { return 30 })

	breaks := 0
	for i := 1; i < len(prefixes); i++ {
		prev, cur := prefixes[i-1], prefixes[i]
		if commonPrefix(prev, cur) < len(prev) {
			breaks++
		}
	}
	// Without stickiness this was one break per step from the first compaction
	// onward — roughly fifty. A handful is the cost of the compactions
	// themselves.
	if breaks > steps/6 {
		t.Fatalf("%d prefix breaks over %d steps; compaction is not sticky", breaks, steps)
	}
	t.Logf("%d prefix breaks over %d steps", breaks, steps)
}

func TestCompactionNeverGrowsAResult(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 2000; i++ {
		lines := rng.Intn(60)
		var text string
		for j := 0; j < lines; j++ {
			text += fmt.Sprintf("%d:%s\n", j, string(make([]byte, rng.Intn(40))))
		}
		floor := rng.Intn(500)
		got := CompactToolResultText(text, floor)
		if len(got) > len(text) && len(text) > floor && floor > 0 {
			t.Fatalf("compacting %d bytes to a floor of %d produced %d bytes",
				len(text), floor, len(got))
		}
	}
}

// Wildly varying result sizes, including empty ones, must not wedge the planner
// or let the budget run away.
func TestCompactionHandlesRaggedResults(t *testing.T) {
	rng := rand.New(rand.NewSource(4242))
	budget := Budget{ContextWindow: 12000, ReservedOutput: 2048, Overhead: 512}
	prefixes := buildTurn(t, 40, budget, func(int) int { return rng.Intn(120) })
	if len(prefixes) != 40 {
		t.Fatalf("got %d steps", len(prefixes))
	}
}

// A result that is already at or under the floor must not be compacted, or the
// plan would record a change that changes nothing and break the prefix for it.
func TestCompactionSkipsResultsThatAreAlreadySmall(t *testing.T) {
	var msgs []llm.Message
	msgs = append(msgs, llm.Message{Role: llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Text: "task"}}})
	for i := 0; i < 40; i++ {
		msgs = append(msgs, toolResultMsg(llm.CallID(fmt.Sprintf("tiny%d", i)), "ok"))
	}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, toolResultMsg(llm.CallID(fmt.Sprintf("big%d", i)), bigOutput(300, "b")))
	}
	plan := PlanCompaction(msgs, "sys", nil, Budget{ContextWindow: 9000, ReservedOutput: 1024, Overhead: 256},
		map[llm.CallID]struct{}{})
	for _, s := range plan.Steps {
		if s.FromBytes <= s.ToBytes {
			t.Fatalf("%s was 'compacted' from %d to %d bytes", s.ToolCallID, s.FromBytes, s.ToBytes)
		}
	}
}

func TestBudgetIsSaneForAbsurdCapabilities(t *testing.T) {
	for _, b := range []Budget{
		{ContextWindow: 0, ReservedOutput: 0, Overhead: 0},
		{ContextWindow: -1, ReservedOutput: -1, Overhead: -1},
		{ContextWindow: 100, ReservedOutput: 1000000, Overhead: 1000000},
		{ContextWindow: 1 << 30, ReservedOutput: 1, Overhead: 1},
	} {
		if got := b.Threshold(); got < 4096 {
			t.Errorf("Threshold() = %d for %+v; the floor must hold", got, b)
		}
		if got := b.Target(); got <= 0 || got > b.Threshold() {
			t.Errorf("Target() = %d for %+v", got, b)
		}
	}
}
