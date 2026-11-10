package computer

import (
	"reflect"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

func snapshotFixture() workflow.State {
	return workflow.State{
		RunID: "run", SessionID: "session", Epoch: 1, Phase: workflow.Observing,
		Outputs:        map[string]workflow.Value{"balance": {Type: "money", Integer: 5, Currency: "USD"}},
		RecoveryCounts: map[string]int{"dismiss": 1},
		Observation:    workflow.Observation{ID: "o", Visual: &workflow.VisualMatch{TargetID: "t", Matched: workflow.PixelRect{X: 1, Width: 8, Height: 8}}},
	}
}

// A caller that writes to a snapshot must not be able to change what the run
// decides next: every map and pointer reachable from State has to be its own.
func TestSnapshotSharesNoReferenceWithTheRunsOwnState(t *testing.T) {
	r := &Run{state: snapshotFixture()}
	snap := r.Snapshot()
	snap.Outputs["balance"] = workflow.Value{Type: "money", Integer: 999}
	snap.Outputs["extra"] = workflow.Value{Type: "string", Text: "x"}
	snap.RecoveryCounts["dismiss"] = 99
	snap.RecoveryCounts["extra"] = 1
	snap.Observation.Visual.TargetID = "tampered"

	want := snapshotFixture()
	if !reflect.DeepEqual(r.state, want) {
		t.Fatalf("mutating a snapshot changed the run's own state:\n got %+v\nwant %+v", r.state, want)
	}
	if again := r.Snapshot(); !reflect.DeepEqual(again, want) {
		t.Fatalf("a second snapshot saw the first caller's writes:\n got %+v\nwant %+v", again, want)
	}
}

func TestOwnedStateKeepsAnAbsentRecoveryCountsAbsent(t *testing.T) {
	s := snapshotFixture()
	s.RecoveryCounts = nil
	if got := ownedState(s); got.RecoveryCounts != nil {
		t.Fatalf("RecoveryCounts became %v; traces recorded without it must still round-trip", got.RecoveryCounts)
	}
}
