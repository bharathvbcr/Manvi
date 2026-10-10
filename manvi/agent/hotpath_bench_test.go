package agent

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// benchCompactionHistory is n exchanges of an assistant tool call and its
// result, the bulk of a long turn's request.
func benchCompactionHistory(n int) []llm.Message {
	msgs := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "do the work"}}}}
	for i := 0; i < n; i++ {
		id := llm.CallID(fmt.Sprintf("c%04d", i))
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				llm.ToolCallBlock{ID: id, Name: "grep", Arguments: json.RawMessage(`{"pattern":"x"}`)}}},
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
				llm.ToolResultBlock{ToolCallID: id, Content: llm.Content{llm.TextBlock{Text: bigOutput(40, string(id))}}}}})
	}
	return msgs
}

// BenchmarkPlanCompaction is the planner's per-step cost over a long history:
// the under-threshold case, which is almost every step, and the over-threshold
// one, which elects results to compact.
func BenchmarkPlanCompaction(b *testing.B) {
	for _, n := range []int{50, 250, 1000} {
		msgs := benchCompactionHistory(n)
		total := CountRequestTokens("sys", nil, msgs)
		for _, tc := range []struct {
			name   string
			window int
		}{{"under", total * 4}, {"over", total * 3 / 4}} {
			b.Run(fmt.Sprintf("exchanges=%d/%s", n, tc.name), func(b *testing.B) {
				budget := Budget{ContextWindow: tc.window, ReservedOutput: 1024, Overhead: 256}
				already := map[llm.CallID]struct{}{}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					PlanCompactionCalibrated(msgs, "sys", nil, budget, already, nil)
				}
			})
		}
	}
}
