package agent

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// TestAFailedWriteThatNamesItsPathCountsAsMutated: a write that failed at its
// last durability step has already replaced the file, and the end-of-turn
// check only runs for a turn that mutated.
func TestAFailedWriteThatNamesItsPathCountsAsMutated(t *testing.T) {
	p := &progressTracker{mutating: map[string]bool{"write_file": true}}
	for _, tc := range []struct {
		name   string
		result tools.Result
		want   bool
	}{
		{"landed", tools.Result{Wrote: []string{"a.go"}}, true},
		{"failed, nothing changed", tools.Result{IsError: true}, false},
		{"failed after the bytes landed", tools.Result{IsError: true, Wrote: []string{"a.go"}}, true},
		// A refusal is Blocked; a Rule alone can sit on a call that ran (a
		// protected-file warning, a demotion, a grant), so it does not mean
		// nothing changed (ft-0fcd6c4c).
		{"refused", tools.Result{IsError: true, Blocked: true, Rule: "r"}, false},
		{"ran under a rule", tools.Result{Wrote: []string{"a.go"}, Rule: "r"}, true},
	} {
		if got := p.mutated("write_file", tc.result); got != tc.want {
			t.Errorf("%s: mutated = %v, want %v", tc.name, got, tc.want)
		}
	}
	if p.mutated("read_file", tools.Result{IsError: true, Wrote: []string{"a.go"}}) {
		t.Error("a tool that is not registered as mutating counted as a mutation")
	}
}
