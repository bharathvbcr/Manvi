package computer

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/bharathvbcr/Manvi/manvi/workflow"
	"strings"
	"testing"
	"time"
)

func TestRecorderCannotRewriteRetainedEventsAndObservations(t *testing.T) {
	opts, _ := runFixture(t, false)
	opts.OnRecord = func(r Record) error {
		if r.Event != nil {
			r.Event.Reason = "injected"
		}
		if r.Observation != nil && len(r.Observation.Nodes) > 0 {
			r.Observation.Nodes[0].Name = "injected"
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range result.Events {
		if e.Reason == "injected" {
			t.Fatal("record callback changed retained event")
		}
	}
	if run.Observation().Nodes[0].Name == "injected" {
		t.Fatal("record callback changed retained observation")
	}
}

func TestReceiptJournalFailureFencesAndLeavesUnknownOutcome(t *testing.T) {
	opts, d := runFixture(t, false)
	opts.OnRecord = func(r Record) error {
		if r.Event != nil && r.Event.Kind == "receipt" {
			return errors.New("journal disk full")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err == nil || !strings.Contains(err.Error(), "disk full") || result.State.Phase != workflow.Unknown {
		t.Fatalf("state=%+v err=%v", result.State, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.paused || d.acts != 1 {
		t.Fatalf("journal failure did not fence: paused=%v acts=%d", d.paused, d.acts)
	}
}

func TestMoneyRejectsOverflowAndMalformedSigns(t *testing.T) {
	for _, text := range []string{"92233720368547758.08", "-92233720368547758.09", "1.+1", "+1.00", "$1.00 EUR", "1,00.00", "1,000.0+"} {
		if _, err := ParseValue(text, "money", "USD"); err == nil {
			t.Errorf("accepted invalid money %q", text)
		}
	}
	for text, want := range map[string]int64{"92233720368547758.07": 9223372036854775807, "-92233720368547758.08": -9223372036854775808, "-1,250.00": -125000} {
		v, err := ParseValue(text, "money", "USD")
		if err != nil || v.Integer != want {
			t.Errorf("valid boundary %s: %v %v", text, v, err)
		}
	}
}

type flakyObservationDesktop struct {
	*testDesktop
	failures     int
	observations int
	code         string
}

func (d *flakyObservationDesktop) Observe(ctx context.Context, s Session) (Observation, error) {
	d.observations++
	if d.observations <= d.failures {
		code := d.code
		if code == "" {
			code = "observation_inconsistent"
		}
		return Observation{}, &BrokerError{Code: code, Delivery: "not_sent"}
	}
	return d.testDesktop.Observe(ctx, s)
}
func TestTransientCaptureFailureRetriesBeforeDispatch(t *testing.T) {
	for _, code := range []string{"observation_inconsistent", "window_ambiguous", "window_occluded"} {
		t.Run(code, func(t *testing.T) {
			opts, base := runFixture(t, false)
			d := &flakyObservationDesktop{testDesktop: base, failures: 2, code: code}
			opts.Desktop = d
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			run, err := StartRun(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			result, err := run.Wait(ctx)
			if err != nil || result.State.Phase != workflow.Completed {
				t.Fatalf("transient capture aborted: phase=%s err=%v", result.State.Phase, err)
			}
			base.mu.Lock()
			defer base.mu.Unlock()
			if base.acts != 1 {
				t.Fatalf("dispatch count %d", base.acts)
			}
		})
	}
}
func TestFinalFenceUpdatesSnapshotEpoch(t *testing.T) {
	opts, base := runFixture(t, false)
	opts.OnRecord = func(r Record) error {
		if r.Kind == "action_started" {
			return errors.New("journal failed")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err == nil {
		t.Fatal("journal failure lost")
	}
	if result.State.Epoch <= opts.Session.Epoch || run.Snapshot().Epoch != result.State.Epoch {
		t.Fatalf("final fence epoch stale: %+v", result.State)
	}
	base.mu.Lock()
	defer base.mu.Unlock()
	if !base.paused || base.acts != 0 {
		t.Fatal("admission failure dispatched input or did not fence")
	}
}
func TestPersistentOcclusionPausesWithoutDispatch(t *testing.T) {
	opts, base := runFixture(t, false)
	opts.Desktop = &flakyObservationDesktop{testDesktop: base, failures: 100, code: "window_occluded"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	s := waitPhase(t, ctx, run, workflow.Paused)
	if s.Attempts != opts.Program.Capability().Limits.ObservationAttempts {
		t.Fatalf("occlusion attempts not bounded: %+v", s)
	}
	base.mu.Lock()
	acts := base.acts
	base.mu.Unlock()
	if acts != 0 {
		t.Fatal("occluded window received input")
	}
	if err = run.Send(ctx, Control{Kind: "cancel", Epoch: s.Epoch}); err != nil {
		t.Fatal(err)
	}
	if _, err = run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHumanReceiptJournalFailureIsUnknown(t *testing.T) {
	opts, d := runFixture(t, true)
	records := make(chan Record, 32)
	opts.OnRecord = func(r Record) error {
		if r.Kind == "human_action" {
			return errors.New("human journal failed")
		}
		records <- r
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
	if err := run.Send(ctx, Control{Kind: "human_act", Epoch: s.Epoch, Action: Action{ActionID: "human", ObservationID: run.Observation().ID, TargetID: "search", Kind: "press", Effect: "read"}}); err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err == nil || result.State.Phase != workflow.Unknown {
		t.Fatalf("human receipt failure phase=%s err=%v", result.State.Phase, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.paused || d.acts != 1 {
		t.Fatal("human uncertain input was not fenced")
	}
}

func TestProtectedNumericExtractionCannotEscapeThroughTypedOutput(t *testing.T) {
	opts, d := runFixture(t, false)
	c := opts.Program.Capability()
	c.Steps = []workflow.Step{{ID: "private", Kind: "extract", Target: "balance", Effect: "read", Output: "private", OutputType: "integer"}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	opts.Program, err = workflow.Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	sensitive := "123456"
	d.observation.Nodes[1].Value = &sensitive
	d.observation.Nodes[1].Bounds = &Bounds{10, 10, 20, 10}
	opts.Privacy.SensitiveTargets = []Selector{{Name: "Balance"}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Phase == workflow.Completed || result.State.Outputs["private"].Integer == 123456 {
		t.Fatal("protected numeric value escaped into typed output")
	}
}
