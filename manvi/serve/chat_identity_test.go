package serve

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// firstResult is the ledger key of the tool message answering id when no other
// tool message carries it.
func firstResult(id string) llm.ResultKey {
	return llm.ResultKey{ID: llm.CallID(id), Occurrence: 1}
}

func taggedResult(id, tag string, lines int) WireMessage {
	var b strings.Builder
	for ln := 0; ln < lines; ln++ {
		fmt.Fprintf(&b, "%s src/pkg/file.go:%d: some matching line of source code here\n", tag, ln)
	}
	return WireMessage{Role: "tool", ToolCallID: id, Text: b.String()}
}

func callOf(id string) WireMessage {
	return WireMessage{Role: "assistant", ToolCalls: []WireToolCall{{ID: id, Name: "Grep", Arguments: `{}`}}}
}

// preparedSteps runs one prepare and returns the steps as the host reads them
// off the wire, so a field the host cannot see does not count.
func preparedSteps(t *testing.T, d *driver, p PrepareParams) []map[string]any {
	t.Helper()
	resp := d.call(prepareRequest(t, fmt.Sprint(d.n), p))
	if !resp.OK {
		t.Fatalf("chat.prepare failed: %+v", resp.Error)
	}
	var wire struct {
		Steps []map[string]any `json:"steps"`
	}
	if err := json.Unmarshal(resp.Result, &wire); err != nil {
		t.Fatal(err)
	}
	return wire.Steps
}

// A host that reuses a call id across responses has two results with one id.
// The ledger names a result, not an id: shortening the first must neither stand
// in for the second nor make the planner believe the second is already short.
func TestChatLedgerNamesAResultNotACallID(t *testing.T) {
	d := newDriver(t)
	history := []WireMessage{{Role: "user", Text: "go"}}
	history = append(history, callOf("call_0"), taggedResult("call_0", "AAAA", 300))
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("tail_%d", i)
		history = append(history, callOf(id), taggedResult(id, "tail", 2))
	}
	params := PrepareParams{SessionID: "reuse", System: "sys", Messages: history, ContextWindow: 4096}

	first := preparedSteps(t, d, params)
	if len(first) != 1 || first[0]["tool_call_id"] != "call_0" {
		t.Fatalf("setup: first prepare steps = %v, want one for call_0", first)
	}
	if got, _ := first[0]["occurrence"].(float64); got != 1 {
		t.Errorf("first step names occurrence %v of call_0, want 1", first[0]["occurrence"])
	}
	firstText, ok := first[0]["text"].(string)
	if !ok {
		t.Fatalf("first step carries no text: %v", first[0])
	}
	history[2].Text = firstText

	// The host reuses call_0: a second, large result with the same id.
	history = append(history, callOf("call_0"), taggedResult("call_0", "BBBB", 300))
	for i := 3; i < 6; i++ {
		id := fmt.Sprintf("tail_%d", i)
		history = append(history, callOf(id), taggedResult(id, "tail", 2))
	}
	params.Messages = history

	second := preparedSteps(t, d, params)
	var forSecond map[string]any
	for _, s := range second {
		if s["tool_call_id"] == "call_0" {
			forSecond = s
		}
	}
	if forSecond == nil {
		t.Fatalf("the second call_0 result was never shortened (steps: %v)", second)
	}
	if got, _ := forSecond["occurrence"].(float64); got != 2 {
		t.Errorf("step names occurrence %v, want 2", forSecond["occurrence"])
	}
	text, _ := forSecond["text"].(string)
	if !strings.Contains(text, "BBBB") || strings.Contains(text, "AAAA") {
		t.Errorf("the second result's shortening is not made from the second result:\n%s", text)
	}
}
