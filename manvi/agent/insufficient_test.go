package agent

import (
	"strings"
	"testing"

	"manvi/llm"
)

// A history that exceeds the threshold with nothing eligible to shorten must
// still report Insufficient.
//
// The flag's whole purpose is that the request is about to overflow the
// server's window and the harness could not prevent it — "a harness that
// carried on silently would produce a truncation the operator could not
// explain". Zero eligible candidates is the *most* insufficient case, not an
// exception to it, and it is easy to reach: ProtectedTail shields the last six
// messages, so any turn shorter than that has no eligible result at all.
func TestAnOverBudgetHistoryWithNothingEligibleIsStillInsufficient(t *testing.T) {
	huge := strings.Repeat("src/pkg/file.go:1: a matching line of source code\n", 400)

	// Four messages: fewer than ProtectedTail, so every tool result is
	// shielded and nothing can be compacted.
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "fix the build"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCallBlock{ID: "c1", Name: "Grep", Arguments: []byte(`{}`)},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultBlock{
			ToolCallID: "c1",
			Content:    []llm.ContentBlock{llm.TextBlock{Text: huge}},
		}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "looking"}}},
	}

	budget := Budget{ContextWindow: 8192, ReservedOutput: 2048}
	plan := PlanCompaction(messages, "you are a coding assistant", nil, budget, nil)

	if plan.Before <= budget.Threshold() {
		t.Fatalf("fixture is not over budget: before=%d threshold=%d",
			plan.Before, budget.Threshold())
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("the fixture was expected to have nothing eligible, got %d steps", len(plan.Steps))
	}
	if !plan.Insufficient {
		t.Errorf("over budget (%d > %d) with nothing to shorten, but Insufficient is false — "+
			"the turn will be truncated by the server and nobody was told",
			plan.After, budget.Threshold())
	}
}

// And the ordinary case must not regress: under budget is not insufficient.
func TestAnUnderBudgetHistoryIsNotInsufficient(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hello"}}},
	}
	plan := PlanCompaction(messages, "sys", nil, Budget{ContextWindow: 8192}, nil)
	if plan.Insufficient {
		t.Error("a small history was reported insufficient")
	}
}
