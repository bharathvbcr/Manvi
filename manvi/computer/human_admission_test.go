package computer

import (
	"context"
	"errors"
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
	onFocus              func(*Observation)
	approved             Action
	dispatched           Action
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
	if d.onFocus != nil {
		d.onFocus(&d.observation)
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
func (d *humanAdmissionDesktop) Approve(_ context.Context, _ Session, a Action) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.grants++
	d.approved = ownedAction(a)
	return "fixture-only-grant", nil
}
func (d *humanAdmissionDesktop) HumanAct(_ context.Context, _ Session, a Action) (Receipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.humanAttempts++
	d.dispatched = ownedAction(a)
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
	if d.approved.ObservationID == observed.ID || d.approved.ObservationID != d.dispatched.ObservationID || d.approved.ActionID != "manual-proof" || d.dispatched.ApprovalID != "fixture-only-grant" {
		t.Fatal("human action approval was not bound to the fresh observation and original action")
	}
}

func TestHumanRevalidationRefusesChangesAndLateInput(t *testing.T) {
	for _, scenario := range []string{"private_value", "pixels", "window_geometry", "target_geometry", "target_name", "late_input", "journal_failure", "cancelled_focus", "stale_frame", "foreign_target", "unadvertised_action", "supplied_grant", "fresh_capture_ids", "empty_fresh_target", "duplicate_fresh_target"} {
		t.Run(scenario, func(t *testing.T) {
			opts, base := runFixture(t, true)
			base.observation.Nodes[0].Bounds = &Bounds{X: 5, Y: 5, Width: 20, Height: 10}
			base.observation.Nodes[0].NativePath = []uint32{0, 1}
			private := "private original value"
			base.observation.Nodes = append(base.observation.Nodes, Node{ID: "private", Role: "text_field", Name: "Member ID", Value: &private, Bounds: &Bounds{X: 40, Y: 20, Width: 20, Height: 10}})
			opts.Privacy.SensitiveTargets = []Selector{{Name: "Member ID"}}
			d := &humanAdmissionDesktop{testDesktop: base}
			opts.Desktop = d
			records := make(chan Record, 64)
			opts.OnRecord = func(r Record) error {
				if scenario == "journal_failure" && r.Stage == "human_revalidation" {
					return errors.New("revalidation journal unavailable")
				}
				records <- r
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			run, err := StartRun(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanup, finish := context.WithTimeout(context.Background(), time.Second)
				defer finish()
				if !run.Snapshot().Terminal() {
					if err := run.Send(cleanup, Control{Kind: "cancel", Epoch: run.Snapshot().Epoch}); err != nil {
						t.Error(err)
					}
				}
				if _, err := run.Wait(cleanup); err != nil && scenario != "cancelled_focus" {
					t.Error(err)
				}
			}()
			s := waitPhase(t, ctx, run, workflow.AwaitingApproval)
			if err := run.Send(ctx, Control{Kind: "pause", Epoch: s.Epoch}); err != nil {
				t.Fatal(err)
			}
			s = waitPhase(t, ctx, run, workflow.Paused)
			if err := run.Send(ctx, Control{Kind: "refresh", Epoch: s.Epoch}); err != nil {
				t.Fatal(err)
			}
			for {
				select {
				case record := <-records:
					if record.Kind == "observation" && record.Stage == "refresh" {
						goto refreshed
					}
				case <-ctx.Done():
					t.Fatal("paused observation missing")
				}
			}
		refreshed:
			observed := run.Observation()
			control := Control{Kind: "human_act", Epoch: s.Epoch, Action: Action{ActionID: "manual-adversary", ObservationID: observed.ID, TargetID: "search", Kind: "press", Effect: "change"}}
			changedPixels := changePixel(t, base.observation).Screenshot
			d.mu.Lock()
			focusBefore := d.focuses
			d.hid++ // The trusted workbench control click occurs before admission.
			switch scenario {
			case "private_value":
				d.mutatePrivateOnFocus = true
			case "target_name":
				d.mutateTargetOnFocus = true
			case "pixels":
				d.onFocus = func(o *Observation) { o.Screenshot = changedPixels }
			case "window_geometry":
				d.onFocus = func(o *Observation) { o.Window.Bounds.X++ }
			case "target_geometry":
				d.onFocus = func(o *Observation) { o.Nodes[0].Bounds.X++ }
			case "late_input":
				d.interfereAtDispatch = true
			case "cancelled_focus":
				d.onFocus = func(*Observation) { cancel() }
			case "stale_frame":
				control.Action.ObservationID = "old"
			case "foreign_target":
				control.Action.TargetID = "foreign"
			case "unadvertised_action":
				control.Action.Kind = "scroll"
			case "supplied_grant":
				control.Action.ApprovalID = "untrusted-grant"
			case "fresh_capture_ids":
				d.onFocus = func(o *Observation) { o.Nodes[0].ID = "fresh-node" }
			case "empty_fresh_target":
				d.onFocus = func(o *Observation) { o.Nodes[0].ID = "" }
			case "duplicate_fresh_target":
				d.onFocus = func(o *Observation) { o.Nodes[1].ID = o.Nodes[0].ID }
			}
			d.mu.Unlock()
			if err := run.Send(ctx, control); err != nil {
				t.Fatal(err)
			}
			if scenario == "cancelled_focus" {
				cleanup, finish := context.WithTimeout(context.Background(), time.Second)
				defer finish()
				_, _ = run.Wait(cleanup)
			} else {
				for {
					select {
					case record := <-records:
						if record.Kind == "control_refused" || (scenario == "fresh_capture_ids" && record.Kind == "human_action") {
							goto checked
						}
					case <-ctx.Done():
						t.Fatal("human action did not settle")
					}
				}
			}
		checked:
			d.mu.Lock()
			defer d.mu.Unlock()
			wantGrants, wantAttempts, wantSent := 0, 0, 0
			if scenario == "late_input" {
				wantGrants, wantAttempts = 1, 1
			}
			if scenario == "fresh_capture_ids" {
				wantGrants, wantAttempts, wantSent = 1, 1, 1
				if d.dispatched.TargetID != "fresh-node" || d.dispatched.ObservationID == observed.ID || d.approved.ObservationID != d.dispatched.ObservationID {
					t.Fatal("changed capture-local identity was not bound to the same reviewed target")
				}
			}
			if d.grants != wantGrants || d.humanAttempts != wantAttempts || d.humanSent != wantSent {
				t.Fatalf("unexpected grant/input: grants=%d attempted=%d sent=%d", d.grants, d.humanAttempts, d.humanSent)
			}
			if scenario == "stale_frame" || scenario == "foreign_target" || scenario == "unadvertised_action" || scenario == "supplied_grant" {
				if d.focuses != focusBefore {
					t.Fatal("invalid human action reset the native observation baseline")
				}
			}
		})
	}
}
