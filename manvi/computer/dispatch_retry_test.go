package computer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

type changedBeforeDispatchDesktop struct {
	*testDesktop
	retryMu                             sync.Mutex
	attempts                            []Action
	captures, resolves, focuses, grants int
	failures                            int
	code, delivery                      string
	receiptDelivery                     string
	blockFocus                          bool
	focusEntered                        chan struct{}
	resumes                             int
}

func (d *changedBeforeDispatchDesktop) Observe(ctx context.Context, s Session) (Observation, error) {
	o, err := d.testDesktop.Observe(ctx, s)
	d.retryMu.Lock()
	d.captures++
	o.ID = fmt.Sprintf("capture-%d", d.captures)
	d.retryMu.Unlock()
	return o, err
}
func (d *changedBeforeDispatchDesktop) Resolve(ctx context.Context, s Session, id string, selector Selector) (Node, error) {
	d.retryMu.Lock()
	d.resolves++
	d.retryMu.Unlock()
	return d.testDesktop.Resolve(ctx, s, id, selector)
}
func (d *changedBeforeDispatchDesktop) Focus(ctx context.Context, s Session) (Session, error) {
	d.retryMu.Lock()
	d.focuses++
	block, entered := d.blockFocus, d.focusEntered
	d.retryMu.Unlock()
	if block {
		close(entered)
		<-ctx.Done()
		return s, ctx.Err()
	}
	return d.testDesktop.Focus(ctx, s)
}
func (d *changedBeforeDispatchDesktop) Resume(ctx context.Context, s Session, id string) (Session, error) {
	d.retryMu.Lock()
	d.resumes++
	d.retryMu.Unlock()
	return d.testDesktop.Resume(ctx, s, id)
}
func (d *changedBeforeDispatchDesktop) Approve(ctx context.Context, s Session, a Action) (string, error) {
	d.retryMu.Lock()
	d.grants++
	d.retryMu.Unlock()
	return d.testDesktop.Approve(ctx, s, a)
}
func (d *changedBeforeDispatchDesktop) Act(ctx context.Context, s Session, a Action) (Receipt, error) {
	d.retryMu.Lock()
	d.attempts = append(d.attempts, ownedAction(a))
	failed := len(d.attempts) <= d.failures
	d.retryMu.Unlock()
	if failed {
		return Receipt{ActionID: a.ActionID, Delivery: d.receiptDelivery}, &BrokerError{Code: d.code, Delivery: d.delivery}
	}
	return d.testDesktop.Act(ctx, s, a)
}

func TestNotSentRetriesPauseAtBoundAndCannotResumePastBudget(t *testing.T) {
	opts, base := runFixture(t, false)
	d := &changedBeforeDispatchDesktop{testDesktop: base, failures: 100, code: "window_changed", delivery: "not_sent"}
	opts.Desktop = d
	refused := make(chan struct{}, 1)
	opts.OnRecord = func(r Record) error {
		if r.Kind == "control_refused" {
			select {
			case refused <- struct{}{}:
			default:
			}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	var state workflow.State
	for {
		state = run.Snapshot()
		if state.Phase == workflow.Paused && state.Epoch > 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("retry bound did not pause and fence")
		case <-time.After(time.Millisecond):
		}
	}
	if state.ActionAttempt != opts.Program.Capability().Limits.ObservationAttempts {
		t.Fatalf("wrong attempt count: %+v", state)
	}
	if err := run.Send(ctx, Control{Kind: "resume", Epoch: state.Epoch}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-refused:
	case <-ctx.Done():
		t.Fatal("exhausted resume did not refuse")
	}
	d.retryMu.Lock()
	if len(d.attempts) != 3 || d.resumes != 0 || d.focuses != 2 {
		t.Errorf("attempt cap bypassed: %d attempts, %d resumes, %d focus", len(d.attempts), d.resumes, d.focuses)
	}
	d.retryMu.Unlock()
	if err := run.Send(ctx, Control{Kind: "cancel", Epoch: state.Epoch}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUncertainOrUnclassifiedDispatchFailuresNeverRetry(t *testing.T) {
	for _, tc := range []struct {
		name, code, delivery, receipt string
		phase                         workflow.Phase
	}{
		{"unknown", "state_changed", "unknown", "", workflow.Unknown},
		{"unclassified", "permission_denied", "not_sent", "", workflow.Failed},
		{"sent_receipt_overrides_error", "state_changed", "not_sent", "sent", workflow.Unknown},
		{"unknown_receipt_overrides_error", "window_changed", "not_sent", "unknown", workflow.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, base := runFixture(t, false)
			d := &changedBeforeDispatchDesktop{testDesktop: base, failures: 100, code: tc.code, delivery: tc.delivery, receiptDelivery: tc.receipt}
			opts.Desktop = d
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			run, err := StartRun(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			result, err := run.Wait(ctx)
			if err != nil || result.State.Phase != tc.phase {
				t.Fatalf("phase=%s err=%v", result.State.Phase, err)
			}
			d.retryMu.Lock()
			defer d.retryMu.Unlock()
			if len(d.attempts) != 1 || d.focuses != 0 {
				t.Fatal("uncertain or unrelated failure retried")
			}
		})
	}
}

func TestCancellationDuringRecoveryFocusDoesNotDispatchAgain(t *testing.T) {
	opts, base := runFixture(t, false)
	d := &changedBeforeDispatchDesktop{testDesktop: base, failures: 1, code: "window_changed", delivery: "not_sent", blockFocus: true, focusEntered: make(chan struct{})}
	opts.Desktop = d
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.focusEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery focus not attempted")
	}
	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	result, err := run.Wait(waitCtx)
	if errors.Is(err, context.DeadlineExceeded) || !result.State.Terminal() {
		t.Fatalf("cancelled recovery did not retire: %s %v", result.State.Phase, err)
	}
	d.retryMu.Lock()
	defer d.retryMu.Unlock()
	if len(d.attempts) != 1 {
		t.Fatal("cancelled recovery dispatched again")
	}
}

func TestNotSentStateChangeReobservesAndUsesNewAttemptID(t *testing.T) {
	for _, code := range []string{"window_changed", "state_changed"} {
		t.Run(code, func(t *testing.T) {
			opts, base := runFixture(t, false)
			d := &changedBeforeDispatchDesktop{testDesktop: base, failures: 1, code: code, delivery: "not_sent"}
			opts.Desktop = d
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			run, err := StartRun(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			result, err := run.Wait(ctx)
			if err != nil || result.State.Phase != workflow.Completed {
				t.Fatalf("safe pre-dispatch recovery aborted: phase=%s err=%v", result.State.Phase, err)
			}
			d.retryMu.Lock()
			defer d.retryMu.Unlock()
			if len(d.attempts) != 2 || d.attempts[0].ActionID == d.attempts[1].ActionID || d.attempts[0].ObservationID == d.attempts[1].ObservationID || d.focuses != 1 || d.resolves != 3 {
				t.Fatalf("missing fresh focus/resolve/attempt: attempts=%+v focus=%d resolves=%d", d.attempts, d.focuses, d.resolves)
			}
			base.mu.Lock()
			defer base.mu.Unlock()
			if base.acts != 1 {
				t.Fatalf("native successful dispatch count %d", base.acts)
			}
			initial, err := workflow.NewState(opts.Program, "run", "session", 1)
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := workflow.Replay(opts.Program, initial, result.Events, nil)
			if err != nil || replayed.Phase != result.State.Phase || replayed.Sequence != result.State.Sequence {
				t.Fatalf("retry journal not deterministic: %+v %v", replayed, err)
			}
		})
	}
}

func TestNotSentChangeRequiresAnotherHumanApproval(t *testing.T) {
	opts, base := runFixture(t, true)
	d := &changedBeforeDispatchDesktop{testDesktop: base, failures: 1, code: "state_changed", delivery: "not_sent"}
	opts.Desktop = d
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	first := waitPhase(t, ctx, run, workflow.AwaitingApproval)
	if err := run.Send(ctx, Control{Kind: "approve", Epoch: first.Epoch, ActionID: first.ActionID(opts.Program), ObservationID: first.Observation.ID}); err != nil {
		t.Fatal(err)
	}
	var next workflow.State
	for {
		next = run.Snapshot()
		if next.Terminal() {
			t.Fatalf("recoverable refusal became terminal: %+v", next)
		}
		if next.Phase == workflow.AwaitingApproval && next.Sequence > first.Sequence {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("fresh approval not requested")
		case <-time.After(time.Millisecond):
		}
	}
	d.retryMu.Lock()
	if len(d.attempts) != 1 || d.grants != 1 {
		t.Errorf("retry dispatched before human approval: %d attempts %d grants", len(d.attempts), d.grants)
	}
	d.retryMu.Unlock()
	if next.ActionID(opts.Program) == first.ActionID(opts.Program) || next.Observation.ID == first.Observation.ID {
		t.Fatal("approval was not rebound")
	}
	// The reducer also refuses the old token even if a caller queues it again.
	stale := workflow.Event{Kind: "approve", RunID: next.RunID, SessionID: next.SessionID, Epoch: next.Epoch, Sequence: next.Sequence + 1, ElapsedMillis: next.ElapsedMillis, ActionID: first.ActionID(opts.Program), Observation: workflow.Observation{ID: first.Observation.ID}}
	if _, _, err := workflow.Reduce(opts.Program, next, stale, nil); err == nil {
		t.Fatal("stale approval replay accepted")
	}
	if err := run.Send(ctx, Control{Kind: "approve", Epoch: next.Epoch, ActionID: next.ActionID(opts.Program), ObservationID: next.Observation.ID}); err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatalf("freshly approved recovery failed: %s %v", result.State.Phase, err)
	}
}
