package computer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

// fenceProbeDesktop drives an automation-initiated pause (the first Observe
// reports an external interaction) and, whenever the run fences the desktop
// with Control("pause"), reports the state the run has published at that very
// moment. The fence call is the window between "the run decided to pause" and
// "the epoch moved", so what is published inside it is exactly what a client
// could read and act on before the fence is final.
type fenceProbeDesktop struct {
	*changedBeforeDispatchDesktop
	runReady chan struct{}
	run      *Run
	// in is signalled when a pause fence begins; gate, when non-nil, holds the
	// fence open until it is closed or the fence context ends.
	in   chan struct{}
	gate chan struct{}

	mu     sync.Mutex
	during []workflow.State
}

func newFenceProbe(base *testDesktop) *fenceProbeDesktop {
	return &fenceProbeDesktop{
		changedBeforeDispatchDesktop: &changedBeforeDispatchDesktop{testDesktop: base, failures: 1, code: "external_interaction", delivery: "not_sent"},
		runReady:                     make(chan struct{}),
		in:                           make(chan struct{}, 8),
	}
}

func (d *fenceProbeDesktop) start(ctx context.Context, t *testing.T, opts RunOptions) *Run {
	t.Helper()
	opts.Desktop = d
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	d.run = run
	close(d.runReady)
	return run
}

func (d *fenceProbeDesktop) Control(ctx context.Context, s Session, op string) (Session, error) {
	if op == "pause" {
		select {
		case <-d.runReady:
		case <-ctx.Done():
			return s, ctx.Err()
		}
		d.mu.Lock()
		d.during = append(d.during, d.run.Snapshot())
		d.mu.Unlock()
		d.in <- struct{}{}
		if d.gate != nil {
			select {
			case <-d.gate:
			case <-ctx.Done():
				return s, ctx.Err()
			}
		}
	}
	return d.testDesktop.Control(ctx, s, op)
}

func (d *fenceProbeDesktop) duringFence() []workflow.State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]workflow.State(nil), d.during...)
}

// A snapshot must never show Paused with an epoch the pause is about to retire.
// Without this a client that resumes with the epoch it just read is refused as
// stale, and nothing tells it.
func TestAutomationPauseNeverPublishesPreFenceEpoch(t *testing.T) {
	opts, base := runFixture(t, false)
	d := newFenceProbe(base)
	ctx := safetyContext(t)
	run := d.start(ctx, t, opts)

	s := waitPhase(t, ctx, run, workflow.Paused)
	if s.Epoch <= opts.Session.Epoch {
		t.Fatalf("published Paused with the pre-fence epoch %d (session started at %d): %+v", s.Epoch, opts.Session.Epoch, s)
	}
	during := d.duringFence()
	if len(during) == 0 {
		t.Fatal("the pause never fenced the desktop")
	}
	for _, snap := range during {
		if snap.Phase == workflow.Paused {
			t.Fatalf("Paused was published before the fence completed (epoch %d): %+v", snap.Epoch, snap)
		}
	}
	// What the client read is what the run accepts.
	if err := run.Send(ctx, Control{Kind: "resume", Epoch: s.Epoch}); err != nil {
		t.Fatalf("resume with the epoch read from the Paused snapshot was refused: %v", err)
	}
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatalf("%+v %v", result.State, err)
	}
}

// A control addressed to a retired epoch is dropped, but the caller is told so
// and the run stays controllable with the current epoch.
func TestStaleEpochControlIsRefusedToTheCaller(t *testing.T) {
	opts, base := runFixture(t, false)
	d := newFenceProbe(base)
	ctx := safetyContext(t)
	run := d.start(ctx, t, opts)
	s := waitPhase(t, ctx, run, workflow.Paused)

	err := run.Send(ctx, Control{Kind: "resume", Epoch: opts.Session.Epoch})
	if !errors.Is(err, ErrStaleControlEpoch) {
		t.Fatalf("a control with a retired epoch returned %v, want ErrStaleControlEpoch", err)
	}
	if got := run.Snapshot(); got.Phase != workflow.Paused || got.Epoch != s.Epoch {
		t.Fatalf("a refused control changed the run: %+v", got)
	}
	if err = run.Send(ctx, Control{Kind: "resume", Epoch: s.Epoch}); err != nil {
		t.Fatalf("the run no longer accepts the current epoch: %v", err)
	}
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatalf("%+v %v", result.State, err)
	}
}

// A human pause sent while the automation pause is still fencing is queued
// behind it. It was addressed to the pre-fence epoch, so it is refused rather
// than silently lost, and the automation pause still lands intact.
func TestHumanPauseRacingAutomationFenceIsRefused(t *testing.T) {
	opts, base := runFixture(t, false)
	d := newFenceProbe(base)
	d.gate = make(chan struct{})
	ctx := safetyContext(t)
	run := d.start(ctx, t, opts)

	<-d.in
	sent := make(chan error, 1)
	go func() { sent <- run.Send(ctx, Control{Kind: "pause", Epoch: opts.Session.Epoch}) }()
	// The loop is inside the fence, so the control cannot be answered yet.
	select {
	case err := <-sent:
		t.Fatalf("a control was answered while the run was mid-fence: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(d.gate)
	if err := <-sent; !errors.Is(err, ErrStaleControlEpoch) {
		t.Fatalf("racing human pause returned %v, want ErrStaleControlEpoch", err)
	}
	s := waitPhase(t, ctx, run, workflow.Paused)
	if s.Epoch <= opts.Session.Epoch {
		t.Fatalf("pause landed with the pre-fence epoch: %+v", s)
	}
	if err := run.Send(ctx, Control{Kind: "resume", Epoch: s.Epoch}); err != nil {
		t.Fatal(err)
	}
	if result, err := run.Wait(ctx); err != nil || result.State.Phase != workflow.Completed {
		t.Fatalf("%+v %v", result.State, err)
	}
}

// Cancelling the caller's context while the pause fence is in flight ends the
// run, never publishes the unfenced Paused state, and releases every Send.
func TestCancelMidFenceEndsRunWithoutPublishingPaused(t *testing.T) {
	opts, base := runFixture(t, false)
	d := newFenceProbe(base)
	d.gate = make(chan struct{})
	ctx := safetyContext(t)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	run := d.start(runCtx, t, opts)

	<-d.in
	sent := make(chan error, 1)
	go func() { sent <- run.Send(ctx, Control{Kind: "resume", Epoch: opts.Session.Epoch}) }()
	cancel()
	// The run's own terminal fence is the next pause; release it so the run can
	// finish instead of waiting out its bound.
	<-d.in
	close(d.gate)
	if err := <-sent; err == nil {
		t.Fatal("a control queued behind a cancelled fence was reported as accepted")
	}
	result, err := run.Wait(ctx)
	if err == nil {
		t.Fatalf("a run cancelled mid-fence reported success: %+v", result.State)
	}
	if result.State.Phase == workflow.Paused && result.State.Epoch <= opts.Session.Epoch {
		t.Fatalf("final state is Paused with the pre-fence epoch: %+v", result.State)
	}
	for _, snap := range d.duringFence() {
		if snap.Phase == workflow.Paused {
			t.Fatalf("Paused was published mid-fence: %+v", snap)
		}
	}
}

// A resume and a second pause admitted at the same epoch must both be answered
// and leave a run that still ends; neither may wedge it.
func TestResumeRacingSecondPauseCannotWedgeRun(t *testing.T) {
	for i := 0; i < 20; i++ {
		opts, base := runFixture(t, false)
		d := newFenceProbe(base)
		ctx := safetyContext(t)
		run := d.start(ctx, t, opts)
		s := waitPhase(t, ctx, run, workflow.Paused)

		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, kind := range []string{"resume", "pause"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- run.Send(ctx, Control{Kind: kind, Epoch: s.Epoch})
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil && !errors.Is(err, ErrStaleControlEpoch) && !errors.Is(err, ErrRunEnded) {
				t.Fatalf("iteration %d: %v", i, err)
			}
		}
		// Whatever the interleaving, driving the run with the epoch it
		// publishes must finish it.
		for !run.Snapshot().Terminal() {
			cur := run.Snapshot()
			if cur.Phase == workflow.Paused {
				if err := run.Send(ctx, Control{Kind: "resume", Epoch: cur.Epoch}); err != nil && !errors.Is(err, ErrStaleControlEpoch) && !errors.Is(err, ErrRunEnded) {
					t.Fatalf("iteration %d: %v", i, err)
				}
			}
			select {
			case <-run.Done():
			case <-ctx.Done():
				t.Fatalf("iteration %d: run wedged in %+v: %v", i, cur, context.Cause(ctx))
			case <-time.After(time.Millisecond):
			}
		}
		if result, err := run.Wait(ctx); err != nil || result.State.Phase != workflow.Completed {
			t.Fatalf("iteration %d: %+v %v", i, result.State, err)
		}
	}
}
