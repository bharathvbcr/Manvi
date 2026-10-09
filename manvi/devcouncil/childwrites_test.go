package devcouncil

import (
	"fmt"
	"slices"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Both dispatch tools carry their children's changed paths upward through
// childWrites. Before it, each tool had its own copy of this fold over its own
// copy of the merge, with a cap that matched the parent loop's only by a
// comment saying so.
func TestChildWritesFoldsEveryChildInOrderWithoutDuplicates(t *testing.T) {
	results := []agents.Result{
		{Label: "a", Value: map[string]any{"wrote": []string{"x.go", "y.go"}}},
		{Label: "failed", Value: nil},
		{Label: "text", Value: "a child that returned prose, not a result map"},
		{Label: "b", Value: map[string]any{"wrote": []string{"y.go", " ", "z.go"}}},
	}
	wrote, truncated := childWrites(results)
	if !slices.Equal(wrote, []string{"x.go", "y.go", "z.go"}) {
		t.Fatalf("wrote %v, want x.go y.go z.go in dispatch order", wrote)
	}
	if truncated {
		t.Fatal("nothing was cut, yet the fan-out reports a truncated list")
	}
}

// A child that cut its own list must leave the fan-out's list marked cut, even
// when the paths it did report fit.
func TestChildWritesKeepsAChildsTruncation(t *testing.T) {
	results := []agents.Result{
		{Label: "a", Value: map[string]any{"wrote": []string{"x.go"}, "wrote_truncated": true}},
		{Label: "b", Value: map[string]any{"wrote": []string{"y.go"}}},
	}
	if _, truncated := childWrites(results); !truncated {
		t.Fatal("a child's truncated list was reported upward as complete")
	}
}

// The fan-out's set lands in the parent turn's, so it is bounded by the
// parent's cap — one constant, not two that happen to agree.
func TestChildWritesIsBoundedByTheLoopsCap(t *testing.T) {
	var results []agents.Result
	for i := range tools.MaxTrackedWrites + 10 {
		results = append(results, agents.Result{
			Value: map[string]any{"wrote": []string{fmt.Sprintf("f%d.go", i)}},
		})
	}
	wrote, truncated := childWrites(results)
	if len(wrote) != tools.MaxTrackedWrites || !truncated {
		t.Fatalf("carried %d paths (truncated=%v), want the loop's cap of %d, reported as cut",
			len(wrote), truncated, tools.MaxTrackedWrites)
	}
}
