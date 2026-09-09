package computer

import (
	"context"
	"fmt"
	"github.com/bharathvbcr/Manvi/manvi/workflow"
	"testing"
	"time"
)

type revalidationDesktop struct {
	*testDesktop
	captures      int
	grants        []Action
	delivered     []Action
	changeOnFocus bool
}

func (d *revalidationDesktop) Observe(ctx context.Context, s Session) (Observation, error) {
	o, err := d.testDesktop.Observe(ctx, s)
	if err != nil {
		return o, err
	}
	d.mu.Lock()
	d.captures++
	o.ID = fmt.Sprintf("capture-%d", d.captures)
	d.mu.Unlock()
	return o, nil
}
func (d *revalidationDesktop) Focus(_ context.Context, s Session) (Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.observation.Window.Foreground = true
	d.observation.Window.Bounds.X++
	if d.changeOnFocus {
		changed := "new private value"
		d.observation.Nodes[len(d.observation.Nodes)-1].Value = &changed
	}
	return s, nil
}
func (d *revalidationDesktop) Approve(_ context.Context, _ Session, a Action) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.grants = append(d.grants, ownedAction(a))
	return "approval", nil
}
func (d *revalidationDesktop) Act(ctx context.Context, s Session, a Action) (Receipt, error) {
	d.mu.Lock()
	d.delivered = append(d.delivered, ownedAction(a))
	d.mu.Unlock()
	return d.testDesktop.Act(ctx, s, a)
}
func TestApprovalRevalidatesEquivalentFormAgainstFreshObservation(t *testing.T) {
	opts, base := runFixture(t, true)
	d := &revalidationDesktop{testDesktop: base}
	opts.Desktop = d
	records := make(chan Record, 64)
	opts.OnRecord = func(r Record) error { records <- r; return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	s := waitPhase(t, ctx, run, workflow.AwaitingApproval)
	if err := run.Send(ctx, Control{Kind: "approve", Epoch: s.Epoch, ActionID: s.ActionID(opts.Program), ObservationID: s.Observation.ID}); err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatal("equivalent approval failed", err, result.State)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.grants) != 1 || len(d.delivered) != 1 || d.grants[0].ObservationID == s.Observation.ID || d.grants[0].ObservationID != d.delivered[0].ObservationID {
		t.Fatalf("approval not bound to fresh observation: grants=%+v delivered=%+v", d.grants, d.delivered)
	}
	revalidated := 0
	for len(records) > 0 {
		r := <-records
		if r.Stage == "approval_revalidation" {
			revalidated++
		}
		if r.Kind == "event" && r.Event.Kind == "receipt" && revalidated != 1 {
			t.Fatal("receipt precedes unique revalidation record")
		}
	}
}
func TestApprovalDetectsChangedPrivateValueBehindRedaction(t *testing.T) {
	opts, base := runFixture(t, true)
	old := "old private value"
	bounds := Bounds{X: 10, Y: 10, Width: 20, Height: 10}
	base.observation.Nodes = append(base.observation.Nodes, Node{ID: "member", Role: "text_field", Name: "Member ID", Value: &old, Bounds: &bounds})
	opts.Privacy.SensitiveTargets = []Selector{{Name: "Member ID"}}
	d := &revalidationDesktop{testDesktop: base, changeOnFocus: true}
	opts.Desktop = d
	changed := make(chan struct{}, 1)
	opts.OnRecord = func(r Record) error {
		if r.Event != nil && r.Event.Kind == "approval_changed" {
			changed <- struct{}{}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	s := waitPhase(t, ctx, run, workflow.AwaitingApproval)
	if err := run.Send(ctx, Control{Kind: "approve", Epoch: s.Epoch, ActionID: s.ActionID(opts.Program), ObservationID: s.Observation.ID}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("private semantic change was not detected")
	}
	s = waitPhase(t, ctx, run, workflow.AwaitingApproval)
	d.mu.Lock()
	n := len(d.grants) + len(d.delivered)
	d.mu.Unlock()
	if n != 0 {
		t.Fatal("changed private form was approved or dispatched")
	}
	if err := run.Send(ctx, Control{Kind: "cancel", Epoch: s.Epoch}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
