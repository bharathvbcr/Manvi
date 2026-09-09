package computer

import (
	"context"
	"github.com/bharathvbcr/Manvi/manvi/workflow"
	"testing"
	"time"
)

func waitPhase(t *testing.T, ctx context.Context, r *Run, phase workflow.Phase) workflow.State {
	t.Helper()
	for {
		s := r.Snapshot()
		if s.Phase == phase {
			return s
		}
		if s.Terminal() {
			t.Fatalf("unexpected terminal phase %s: %s", s.Phase, s.Reason)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}
func TestAssistedRecoveryIsOptInBoundedAndRecorded(t *testing.T) {
	opts, d := runFixture(t, true)
	opts.Assisted = true
	d.observation.Nodes = append(d.observation.Nodes, Node{ID: "back", Name: "Back", Role: "button", Enabled: true})
	records := make(chan Record, 64)
	opts.OnRecord = func(r Record) error { records <- r; return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	s := waitPhase(t, ctx, run, workflow.AwaitingApproval)
	if err := run.Send(ctx, Control{Kind: "pause", Epoch: s.Epoch}); err != nil {
		t.Fatal(err)
	}
	s = waitPhase(t, ctx, run, workflow.Paused)
	if err := run.Send(ctx, Control{Kind: "refresh", Epoch: s.Epoch}); err != nil {
		t.Fatal(err)
	}
	for {
		r := <-records
		if r.Kind == "observation" && r.Stage == "refresh" {
			break
		}
	}
	o := run.Observation()
	a := Action{ActionID: "recovery-1", ObservationID: o.ID, TargetID: "back", Kind: "press", Effect: "read"}
	c := Control{Kind: "assisted_action", Epoch: s.Epoch, ObservationID: o.ID, ActionID: a.ActionID, Action: a}
	if err := run.Send(ctx, c); err != nil {
		t.Fatal(err)
	}
	started, received := false, false
	for {
		select {
		case r := <-records:
			if r.Actor != "model_recovery" {
				continue
			}
			if r.Kind == "action_started" {
				started = true
			}
			if r.Kind == "human_action" {
				received = true
			}
			if r.Kind == "observation" && r.Stage == "human_after" {
				if !started || !received {
					t.Fatal("missing recovery evidence")
				}
				goto finished
			}
		case <-ctx.Done():
			t.Fatal("recovery timed out")
		}
	}
finished:
	if run.Snapshot().Phase != workflow.Paused {
		t.Fatal("recovery resumed automation")
	}
	if err := run.Send(ctx, c); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case r := <-records:
			if r.Kind == "control_refused" && r.Actor == "model_recovery" {
				goto refused
			}
		case <-ctx.Done():
			t.Fatal("second recovery not refused")
		}
	}
refused:
	d.mu.Lock()
	acts := d.acts
	d.mu.Unlock()
	if acts != 1 {
		t.Fatalf("recovery dispatched %d times", acts)
	}
	if err := run.Send(ctx, Control{Kind: "cancel", Epoch: run.Snapshot().Epoch}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestRecoveryRejectsUnboundOrMutatingActions(t *testing.T) {
	o := Observation{ID: "o", Epoch: 2, Nodes: []Node{{ID: "back", Name: "Back", Role: "button", Enabled: true}}}
	base := Control{Kind: "assisted_action", ActionID: "a", ObservationID: "o", Epoch: 2, Action: Action{ActionID: "a", ObservationID: "o", TargetID: "back", Kind: "press", Effect: "read"}}
	if _, err := validateRecovery(base, &o, false, false); err == nil {
		t.Fatal("recovery enabled implicitly")
	}
	if _, err := validateRecovery(base, &o, true, true); err == nil {
		t.Fatal("recovery reused")
	}
	for _, mutate := range []func(*Control){func(c *Control) { c.Epoch++ }, func(c *Control) { c.Action.ObservationID = "old" }, func(c *Control) { c.Action.Effect = "change" }, func(c *Control) { v := "x"; c.Action.Text = &v }, func(c *Control) { c.Action.TargetID = "confirm" }, func(c *Control) { c.Action.Kind = "type_text" }, func(c *Control) { c.ActionID = "other" }} {
		c := base
		mutate(&c)
		if _, err := validateRecovery(c, &o, true, false); err == nil {
			t.Fatal("unsafe recovery admitted")
		}
	}
}
