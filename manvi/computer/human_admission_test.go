package computer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

// This fixture models the native broker's private HID baseline, not OS input.
// Explicit Focus invalidates it, Observe pins it, and HumanAct refuses a later
// hardware change. No operating-system event or real approval is generated.
type humanAdmissionDesktop struct {
	*testDesktop
	hid                  uint64
	baseline             *uint64
	captures             int
	focuses              int
	grants               int
	humanAttempts        int
	humanSent            int
	mutatePrivateOnFocus bool
	mutateTargetOnFocus  bool
	interfereAtDispatch  bool
}

func (d *humanAdmissionDesktop) Observe(ctx context.Context, s Session) (Observation, error) {
	o, err := d.testDesktop.Observe(ctx, s)
	if err != nil {
		return o, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.paused && d.baseline != nil && *d.baseline != d.hid {
		return Observation{}, &BrokerError{Code: "external_interaction", Delivery: "not_sent"}
	}
	d.captures++
	o.ID = fmt.Sprintf("human-capture-%d", d.captures)
	current := d.hid
	d.baseline = &current
	return o, nil
}
func (d *humanAdmissionDesktop) Focus(_ context.Context, s Session) (Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.focuses++
	d.baseline = nil
	if d.mutatePrivateOnFocus {
		value := "changed private form"
		d.observation.Nodes[len(d.observation.Nodes)-1].Value = &value
	}
	if d.mutateTargetOnFocus {
		d.observation.Nodes[0].Name = "Different business action"
	}
	return s, nil
}
func (d *humanAdmissionDesktop) Control(ctx context.Context, s Session, op string) (Session, error) {
	updated, err := d.testDesktop.Control(ctx, s, op)
	d.mu.Lock()
	d.baseline = nil
	d.mu.Unlock()
	return updated, err
}
func (d *humanAdmissionDesktop) Approve(_ context.Context, _ Session, _ Action) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.grants++
	return "fixture-only-grant", nil
}
func (d *humanAdmissionDesktop) HumanAct(_ context.Context, _ Session, a Action) (Receipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.humanAttempts++
	if d.interfereAtDispatch {
		d.hid++
	}
	if d.baseline == nil || *d.baseline != d.hid {
		return Receipt{ActionID: a.ActionID, Delivery: "not_sent"}, &BrokerError{Code: "external_interaction", Delivery: "not_sent"}
	}
	d.humanSent++
	return Receipt{ActionID: a.ActionID, Delivery: "sent"}, nil
}

func TestHumanControlClickReobservesUnchangedFormBeforeInput(t *testing.T) {
	opts, base := runFixture(t, true)
	base.observation.Nodes[0].Bounds = &Bounds{X: 5, Y: 5, Width: 20, Height: 10}
	base.observation.Nodes[0].Actions = []string{"press"}
	d := &humanAdmissionDesktop{testDesktop: base}
	opts.Desktop = d
	records := make(chan Record, 64)
	opts.OnRecord = func(r Record) error { records <- r; return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		s := run.Snapshot()
		_ = run.Send(ctx, Control{Kind: "cancel", Epoch: s.Epoch})
		_, _ = run.Wait(ctx)
	}()
	s := waitPhase(t, ctx, run, workflow.AwaitingApproval)
	if err = run.Send(ctx, Control{Kind: "pause", Epoch: s.Epoch}); err != nil {
		t.Fatal(err)
	}
	s = waitPhase(t, ctx, run, workflow.Paused)
	if err = run.Send(ctx, Control{Kind: "refresh", Epoch: s.Epoch}); err != nil {
		t.Fatal(err)
	}
	refreshed := false
	for !refreshed {
		select {
		case record := <-records:
			refreshed = record.Kind == "observation" && record.Stage == "refresh"
		case <-ctx.Done():
			t.Fatal("paused capture missing")
		}
	}
	observed := run.Observation()
	if observed == nil {
		t.Fatal("paused observation missing")
	}
	d.mu.Lock()
	d.hid++
	focusBefore, capturesBefore := d.focuses, d.captures
	d.mu.Unlock()
	if err = run.Send(ctx, Control{Kind: "human_act", Epoch: s.Epoch, Action: Action{ActionID: "manual-proof", ObservationID: observed.ID, TargetID: "search", Kind: "press", Effect: "change"}}); err != nil {
		t.Fatal(err)
	}
	var receipt *Receipt
	for receipt == nil {
		select {
		case record := <-records:
			if record.Kind == "human_action" {
				receipt = record.Receipt
			}
		case <-ctx.Done():
			t.Fatal("human control receipt missing")
		}
	}
	if receipt.Delivery != "sent" {
		t.Fatalf("trusted control click was refused against the stale HID baseline: %+v", receipt)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.focuses != focusBefore+1 || d.captures <= capturesBefore || d.grants != 1 || d.humanSent != 1 {
		t.Fatalf("manual admission skipped focus/fresh capture: focus=%d captures=%d grants=%d sent=%d", d.focuses, d.captures, d.grants, d.humanSent)
	}
}
