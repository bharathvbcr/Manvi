package computer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

// safetyTimeout is not a pacing deadline. Every test in this package waits on
// the event it expects (a phase, run.Done, a control send); this bound exists
// only so a real hang fails instead of stalling the suite, and it is far above
// anything a loaded machine adds to a sub-second run.
const safetyTimeout = 30 * time.Second

// safetyContext is the context a test hands to StartRun and its waits. It is
// also the run's own lifetime, so a tight bound turned scheduler starvation
// into a spurious "context deadline exceeded". The cause names a real hang.
func safetyContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeoutCause(context.Background(), safetyTimeout,
		errors.New("no progress within the safety timeout: the expected event never arrived"))
	t.Cleanup(cancel)
	t.Cleanup(func() {
		if t.Failed() && ctx.Err() != nil {
			t.Logf("safety context ended: %v", context.Cause(ctx))
		}
	})
	return ctx
}

func TestExternalInteractionSuspendsAndExplicitResumeUsesFreshAttempt(t *testing.T) {
	opts, base := runFixture(t, false)
	d := &changedBeforeDispatchDesktop{testDesktop: base, failures: 1, code: "external_interaction", delivery: "not_sent"}
	opts.Desktop = d
	ctx := safetyContext(t)
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	s := waitFencedPause(t, ctx, run, opts.Session.Epoch)
	if s.Epoch <= opts.Session.Epoch || s.ActionAttempt != 1 {
		t.Fatalf("external interaction was not fenced with a fresh attempt: %+v", s)
	}
	d.retryMu.Lock()
	calls := len(d.attempts)
	d.retryMu.Unlock()
	if calls != 1 {
		t.Fatal("external interaction caused automatic input retry")
	}
	if err = run.Send(ctx, Control{Kind: "resume", Epoch: s.Epoch}); err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatalf("explicit resume failed: %+v %v", result.State, err)
	}
	d.retryMu.Lock()
	defer d.retryMu.Unlock()
	if len(d.attempts) != 2 || d.attempts[0].ActionID == d.attempts[1].ActionID {
		t.Fatal("explicit resume reused refused native action identity")
	}
}

type interactionAfterDispatchDesktop struct {
	*testDesktop
	interrupted bool
}

func (d *interactionAfterDispatchDesktop) Observe(ctx context.Context, s Session) (Observation, error) {
	d.mu.Lock()
	acted := d.acts > 0
	d.mu.Unlock()
	if acted && !d.interrupted {
		d.interrupted = true
		return Observation{}, &BrokerError{Code: "external_interaction", Delivery: "not_sent"}
	}
	return d.testDesktop.Observe(ctx, s)
}
func TestExternalInteractionAfterSentActionOnlyResumesObservation(t *testing.T) {
	opts, base := runFixture(t, false)
	opts.Desktop = &interactionAfterDispatchDesktop{testDesktop: base}
	ctx := safetyContext(t)
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	s := waitFencedPause(t, ctx, run, opts.Session.Epoch)
	if s.ResumePhase != workflow.PostAction {
		t.Fatalf("sent action reconciliation phase lost: %+v", s)
	}
	if err = run.Send(ctx, Control{Kind: "resume", Epoch: s.Epoch}); err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatal(result.State, err)
	}
	base.mu.Lock()
	defer base.mu.Unlock()
	if base.acts != 1 {
		t.Fatal("delivered action replayed on external-input resume")
	}
}
func TestExternalInteractionCannotDowngradePossibleDelivery(t *testing.T) {
	opts, base := runFixture(t, false)
	d := &changedBeforeDispatchDesktop{testDesktop: base, failures: 1, code: "external_interaction", delivery: "not_sent", receiptDelivery: "sent"}
	opts.Desktop = d
	ctx := safetyContext(t)
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Unknown {
		t.Fatalf("possible delivery lost: %+v %v", result.State, err)
	}
	d.retryMu.Lock()
	defer d.retryMu.Unlock()
	if len(d.attempts) != 1 {
		t.Fatal("possibly delivered action retried")
	}
}
