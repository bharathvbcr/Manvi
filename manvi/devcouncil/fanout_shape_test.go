package devcouncil

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

// fullRunner is a child that completes with every optional field of its
// result set, and takes a lease of its own on the way, so each dispatch tool's
// projection of a completed child can be pinned field by field.
type fullRunner struct{}

func (fullRunner) RunSubAgent(_ context.Context, req SubAgentRequest) (SubAgentResult, error) {
	if req.Leases != nil {
		req.Leases.HoldLease("own-task", "own-token")
	}
	return SubAgentResult{
		Summary:        "did the work",
		Steps:          3,
		Usage:          SubAgentUsage{InputTokens: 10, OutputTokens: 4},
		Wrote:          []string{"a.go"},
		WroteTruncated: true,
		// Passed while carrying a finding: a contradiction, which must reach
		// the caller already reconciled to not-passed.
		Verdict: SubAgentVerdict{Judged: true, Passed: true, Findings: []string{"nit"}},
	}, nil
}

// The two dispatch tools share one fan-out core and differ only in the fields
// each adds. This pins those fields exactly — the keys of the payload and of a
// completed child's outcome, per tool — so a change to the shared core that
// moves a field from one tool's shape to the other's, or drops one, fails
// here rather than reaching a dispatching model as a silently different
// answer.
func TestEachDispatchToolKeepsItsOwnOutputShape(t *testing.T) {
	shared := []string{"label", "status", "summary", "steps", "usage", "wrote", "wrote_truncated", "verdict"}
	want := map[string]struct {
		payload []string
		child   []string
		// leases is whether the child's own lease is reported on its
		// outcome. spawn_subagents reports what its pool released or
		// orphaned; invoke_subagent never has.
		leases bool
	}{
		"invoke_subagent": {
			payload: []string{"dispatched", "report", "clean", "results", "usage"},
			child:   append([]string{"conversation_id", "role", "type"}, shared...),
		},
		"spawn_subagents": {
			payload: []string{"report", "clean", "results", "usage"},
			child:   append([]string{"task_id"}, shared...),
			leases:  true,
		},
	}

	for tool, res := range dispatchBoth(t, fullRunner{}) {
		if res.IsError {
			t.Fatalf("%s: a completed child was reported as an error: %s", tool, res.Text)
		}
		if !slices.Equal(res.Wrote, []string{"a.go"}) || len(res.Degraded) == 0 {
			t.Errorf("%s: Wrote=%v Degraded=%v, want a.go marked incomplete", tool, res.Wrote, res.Degraded)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
			t.Fatalf("%s: unreadable payload %q: %v", tool, res.Text, err)
		}
		if got := slices.Sorted(maps.Keys(payload)); !slices.Equal(got, slices.Sorted(slices.Values(want[tool].payload))) {
			t.Errorf("%s: payload keys = %v, want %v", tool, got, slices.Sorted(slices.Values(want[tool].payload)))
		}

		var results []map[string]json.RawMessage
		if err := json.Unmarshal(payload["results"], &results); err != nil || len(results) != 1 {
			t.Fatalf("%s: unreadable results %s: %v", tool, payload["results"], err)
		}
		child := results[0]
		_, released := child["released_leases"]
		_, orphaned := child["orphaned_leases"]
		if leases := released || orphaned; leases != want[tool].leases {
			t.Errorf("%s: the child's own lease reported on its outcome = %v, want %v", tool, leases, want[tool].leases)
		}
		delete(child, "released_leases")
		delete(child, "orphaned_leases")
		if got := slices.Sorted(maps.Keys(child)); !slices.Equal(got, slices.Sorted(slices.Values(want[tool].child))) {
			t.Errorf("%s: child outcome keys = %v, want %v", tool, got, slices.Sorted(slices.Values(want[tool].child)))
		}

		var verdict SubAgentVerdict
		if err := json.Unmarshal(child["verdict"], &verdict); err != nil {
			t.Fatalf("%s: unreadable verdict %s: %v", tool, child["verdict"], err)
		}
		if !verdict.Judged || verdict.Passed || !slices.Equal(verdict.Findings, []string{"nit"}) {
			t.Errorf("%s: verdict = %+v, want judged, not passed, finding \"nit\"", tool, verdict)
		}
		if string(child["status"]) != `"completed"` || string(child["wrote_truncated"]) != "true" {
			t.Errorf("%s: status=%s wrote_truncated=%s", tool, child["status"], child["wrote_truncated"])
		}
	}
}
