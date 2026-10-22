package computer

import (
	"context"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

func TestInterventionEmittedOnPause(t *testing.T) {
	opts, d := runFixture(t, false)
	d.observation.Nodes = []Node{} // force resolve failure → pause
	ctx := safetyContext(t)
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, ctx, run, workflow.Paused)
	list := run.Interventions()
	if len(list) == 0 {
		t.Fatal("expected InterventionRequest")
	}
	req := list[0]
	if req.Run != "run" || req.Status != InterventionRequested || req.ReasonCode == "" {
		t.Fatalf("%+v", req)
	}
	if req.Controller != ControllerAutomation {
		t.Fatalf("controller=%s", req.Controller)
	}
	if StuckReasonCode(workflow.State{Phase: workflow.Paused, Cause: workflow.FailureAmbiguousTarget, Reason: "target resolves to 0 elements"}) != ReasonLadderExhausted {
		t.Fatal("stuck reason mapping")
	}
	if StuckReasonCode(workflow.State{Phase: workflow.AwaitingApproval}) != ReasonApprovalPending {
		t.Fatal("approval reason mapping")
	}
}

func TestTakeoverSetsHumanController(t *testing.T) {
	opts, d := runFixture(t, false)
	d.observation.Nodes = []Node{}
	// One bound for the whole test. The takeover is applied by the run's own
	// loop after Send returns, so the wait for it shares this deadline rather
	// than a separate one-second window that a loaded machine overran.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, ctx, run, workflow.Paused)
	s := run.Snapshot()
	if err := run.Send(ctx, Control{Kind: "takeover", Epoch: s.Epoch}); err != nil {
		t.Fatal(err)
	}
	for run.Controller() != ControllerHuman {
		select {
		case <-ctx.Done():
			t.Fatalf("controller=%s interventions=%+v: %v", run.Controller(), run.Interventions(), ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if len(run.Interventions()) == 0 || run.Interventions()[0].Status != InterventionRequested {
		t.Fatalf("interventions=%+v", run.Interventions())
	}
}

// The reason code comes from the cause the reducer recorded, so native text
// that mentions another cause cannot change it.
func TestStuckReasonCodeComesFromTheCauseNotTheText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state workflow.State
		want  string
	}{
		{"deadline", workflow.State{Phase: workflow.Paused, Cause: workflow.FailureDeadline}, ReasonDeadlineNear},
		{"native text mentioning a deadline", workflow.State{Phase: workflow.Paused, Cause: workflow.FailureIncompleteObserve, Reason: "incomplete observation: deadline banner visible"}, ReasonObservationBudget},
		{"native text mentioning recovery or a ladder", workflow.State{Phase: workflow.Paused, Reason: "recovery ladder dialog"}, ReasonObservationBudget},
		{"not actionable", workflow.State{Phase: workflow.Paused, Cause: workflow.FailureNotActionable}, ReasonNotActionable},
		{"ambiguous target", workflow.State{Phase: workflow.Paused, Cause: workflow.FailureAmbiguousTarget}, ReasonLadderExhausted},
		{"external interaction", workflow.State{Phase: workflow.Paused, Cause: workflow.FailureExternalInteraction, Reason: "external interaction requires human reconciliation: system dialog"}, ReasonExternalInteraction},
		{"human takeover", workflow.State{Phase: workflow.Paused, Cause: workflow.FailureHumanTakeover, Reason: "human takeover"}, ReasonHumanTakeover},
		{"approval wins over any cause", workflow.State{Phase: workflow.AwaitingApproval, Cause: workflow.FailureDeadline}, ReasonApprovalPending},
		{"no cause recorded", workflow.State{Phase: workflow.Paused, Reason: "anything"}, ReasonObservationBudget},
	} {
		if got := StuckReasonCode(tc.state); got != tc.want {
			t.Errorf("%s: StuckReasonCode = %q, want %q", tc.name, got, tc.want)
		}
	}
}
