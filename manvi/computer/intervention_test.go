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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if run.Snapshot().Phase == workflow.Paused {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Snapshot().Phase != workflow.Paused {
		t.Fatalf("expected pause, got %s", run.Snapshot().Phase)
	}
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
	if StuckReasonCode(workflow.Paused, "target resolves to 0 elements") != ReasonLadderExhausted {
		t.Fatal("stuck reason mapping")
	}
	if StuckReasonCode(workflow.AwaitingApproval, "") != ReasonApprovalPending {
		t.Fatal("approval reason mapping")
	}
}

func TestTakeoverSetsHumanController(t *testing.T) {
	opts, d := runFixture(t, false)
	d.observation.Nodes = []Node{}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
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
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && run.Controller() != ControllerHuman {
		time.Sleep(10 * time.Millisecond)
	}
	if run.Controller() != ControllerHuman {
		t.Fatalf("controller=%s interventions=%+v", run.Controller(), run.Interventions())
	}
	if len(run.Interventions()) == 0 || run.Interventions()[0].Status != InterventionRequested {
		t.Fatalf("interventions=%+v", run.Interventions())
	}
}
