package computer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

type testDesktop struct {
	mu          sync.Mutex
	observation Observation
	acts        int
	block       bool
	entered     chan struct{}
	paused      bool
}

func (d *testDesktop) Observe(ctx context.Context, s Session) (Observation, error) {
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	o := ownedObservation(d.observation)
	o.Epoch = s.Epoch
	return o, nil
}
func (d *testDesktop) Resolve(_ context.Context, _ Session, _ string, selector Selector) (Node, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, n := range d.observation.Nodes {
		if n.Name == selector.Name {
			return n, nil
		}
	}
	return Node{}, &BrokerError{Code: "target_missing", Delivery: "not_sent"}
}
func (d *testDesktop) Act(ctx context.Context, _ Session, a Action) (Receipt, error) {
	d.mu.Lock()
	d.acts++
	block := d.block
	entered := d.entered
	d.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block {
		<-ctx.Done()
		return Receipt{}, ctx.Err()
	}
	return Receipt{ActionID: a.ActionID, Delivery: "sent"}, nil
}
func (d *testDesktop) Approve(_ context.Context, _ Session, _ Action) (string, error) {
	return "approval", nil
}
func (d *testDesktop) Control(_ context.Context, s Session, op string) (Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paused = op == "pause"
	s.Epoch++
	return s, nil
}
func (d *testDesktop) Resume(_ context.Context, s Session, _ string) (Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.paused {
		return s, errors.New("not paused")
	}
	d.paused = false
	s.Epoch++
	return s, nil
}
func (d *testDesktop) HumanAct(ctx context.Context, s Session, a Action) (Receipt, error) {
	return d.Act(ctx, s, a)
}

func (d *testDesktop) Focus(_ context.Context, s Session) (Session, error) { return s, nil }

func runFixture(t *testing.T, change bool) (RunOptions, *testDesktop) {
	t.Helper()
	o := observationFixture(t)
	balance := "$1,250.00"
	o.Nodes = []Node{{ID: "search", Role: "button", Name: "Search", Enabled: true}, {ID: "balance", Role: "text_field", Name: "Balance", Value: &balance, Enabled: true}}
	effect := "read"
	if change {
		effect = "change"
	}
	spec := workflow.Capability{SchemaVersion: 1, ID: "bank", Revision: "1", Application: "bank", Targets: map[string]workflow.Selector{"search": {Name: "Search"}, "balance": {Name: "Balance"}}, Limits: workflow.DefaultLimits(), Steps: []workflow.Step{{ID: "search", Kind: "press", Target: "search", Effect: effect}, {ID: "read", Kind: "extract", Target: "balance", Effect: "read", Output: "balance", OutputType: "money", Currency: "USD"}}}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	p, err := workflow.Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := &testDesktop{observation: o}
	return RunOptions{Program: p, Desktop: d, Session: Session{ID: "session", RunID: "run", Epoch: 1, Window: o.Window}}, d
}

func TestRunnerCompletesWithFreshObservationAndNoModel(t *testing.T) {
	opts, d := runFixture(t, false)
	var records []Record
	opts.OnRecord = func(r Record) error { records = append(records, r); return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Completed || result.State.Outputs["balance"].Integer != 125000 {
		t.Fatalf("%+v %v", result, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.acts != 1 {
		t.Fatal("unexpected input dispatch count")
	}
	post := false
	for _, r := range records {
		if r.Stage == "observe_after" {
			post = true
		}
	}
	if !post {
		t.Fatal("missing post-action capture")
	}
}

func TestPauseRemainsResponsiveDuringBlockedAction(t *testing.T) {
	opts, d := runFixture(t, false)
	d.block = true
	d.entered = make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.entered:
	case <-ctx.Done():
		t.Fatal("action did not start")
	}
	if err := run.Send(ctx, Control{Kind: "pause", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Phase != workflow.Unknown {
		t.Fatal("possible input was not classified uncertain", result.State.Phase)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.paused || d.acts != 1 {
		t.Fatal("broker was not fenced")
	}
}

func TestApprovalIsRequiredAndOldEpochCannotDispatch(t *testing.T) {
	opts, d := runFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	for run.Snapshot().Phase != workflow.AwaitingApproval {
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			t.Fatal("approval not reached")
		}
	}
	s := run.Snapshot()
	if err := run.Send(ctx, Control{Kind: "approve", Epoch: 0, ActionID: s.ActionID(opts.Program), ObservationID: s.Observation.ID}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	d.mu.Lock()
	acts := d.acts
	d.mu.Unlock()
	if acts != 0 {
		t.Fatal("stale approval dispatched")
	}
	if err := run.Send(ctx, Control{Kind: "approve", Epoch: s.Epoch, ActionID: s.ActionID(opts.Program), ObservationID: s.Observation.ID}); err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatal("approved run failed", err, result.State)
	}
}

func TestMoneyParsingUsesIntegerMinorUnits(t *testing.T) {
	for text, want := range map[string]int64{"$1,250.00": 125000, "840.50 USD": 84050, "-1.25": -125} {
		got, err := ParseValue(text, "money", "USD")
		if err != nil || got.Integer != want {
			t.Fatalf("%s: %+v %v", text, got, err)
		}
	}
	for _, text := range []string{"1.234", "1e4", "12,34.00", "NaN", "9223372036854775807.00"} {
		if _, err := ParseValue(text, "money", "USD"); err == nil {
			t.Fatalf("accepted %s", text)
		}
	}
}
