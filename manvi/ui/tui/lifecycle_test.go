package tui

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
	"github.com/bharathvbcr/Manvi/manvi/ui/term"
)

// lifecycleRunner is a Runner with a small queue, a done channel and a
// terminal writing to a scratch file: enough for the effects that send to the
// loop's own queue.
func lifecycleRunner(t *testing.T, queue int, host Host) *Runner {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close() })
	tm := term.New(os.Stdin, out)
	return &Runner{
		cfg:     Config{Host: host},
		term:    tm,
		painter: render.NewPainter(10, 5, tm.Profile()),
		actions: make(chan Action, queue),
		cancels: map[string]context.CancelFunc{},
		done:    make(chan struct{}),
	}
}

func fill(r *Runner) {
	sink := r.Sink("S2")
	for len(r.actions) < cap(r.actions) {
		sink.Emit(ui.Event{Text: "stream"})
	}
}

// within runs f and fails if it does not return in time, naming what blocked.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	finished := make(chan struct{})
	go func() { f(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(d):
		t.Fatalf("%s blocked", what)
	}
}

// The loop goroutine is the only consumer of r.actions, so anything it sends
// to that queue with a blocking send deadlocks the moment other sessions'
// streams have filled it. Every send the loop makes to itself must complete.
func TestLoopSelfSendsNeverBlockOnAFullQueue(t *testing.T) {
	t.Run("submit", func(t *testing.T) {
		r := lifecycleRunner(t, 8, &stubHost{})
		fill(r)
		within(t, 2*time.Second, "runEffect(EffectSubmit) on a full queue", func() {
			r.runEffect(context.Background(), EffectSubmit{SessionID: "S1"})
		})
		// The bookkeeping the send carried is not lost: the loop gets it next.
		if got := r.localQueued(); got != 1 {
			t.Fatalf("%d loop-local actions pending, want the turn-started notification", got)
		}
	})
	t.Run("second turn refusal", func(t *testing.T) {
		r := lifecycleRunner(t, 8, &stubHost{})
		release := make(chan struct{})
		r.startTurn(context.Background(), "S1", func(context.Context) error { <-release; return nil })
		fill(r)
		within(t, 2*time.Second, "the refusal notice on a full queue", func() {
			r.startTurn(context.Background(), "S1", func(context.Context) error { return nil })
		})
		close(release)
	})
	t.Run("copy over the clipboard limit", func(t *testing.T) {
		r := lifecycleRunner(t, 8, &stubHost{})
		fill(r)
		within(t, 2*time.Second, "the truncation notice on a full queue", func() {
			r.runEffect(context.Background(), EffectCopy{Text: strings.Repeat("x", osc52Limit+1)})
		})
		if got := r.localQueued(); got != 1 {
			t.Fatalf("%d loop-local actions pending, want the truncation notice", got)
		}
	})
}

// A producer outside the loop must give up when the runner stops; after quit
// nothing drains the queue, and a goroutine parked on it lives forever.
func TestProducersReturnOnceTheRunnerHasStopped(t *testing.T) {
	r := lifecycleRunner(t, 4, &stubHost{})
	close(r.done)
	fill(r)
	within(t, 2*time.Second, "Sink.Emit after stop", func() {
		r.Sink("S1").Emit(ui.Event{Text: "late"})
	})
	within(t, 2*time.Second, "Send after stop", func() { r.Send(ActionTick{}) })
	within(t, 2*time.Second, "the turn goroutine's final send after stop", func() {
		var wg sync.WaitGroup
		wg.Add(1)
		r.startTurn(context.Background(), "S1", func(context.Context) error { wg.Done(); return nil })
		wg.Wait()
		r.waitTurns(2 * time.Second)
	})
}

// While the runner is live nothing is dropped: the blocking send stays, so a
// slow loop slows its producers instead of losing their events.
func TestNoHarnessEventIsDroppedWhileTheRunnerIsLive(t *testing.T) {
	const n = 5000
	r := lifecycleRunner(t, 8, &stubHost{})
	var applied atomic.Int64
	consumed := make(chan struct{})
	go func() {
		for range r.actions {
			if applied.Add(1) == n {
				close(consumed)
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sink := r.Sink("S1")
			for i := 0; i < n/4; i++ {
				sink.Emit(ui.Event{Text: "x"})
			}
		}()
	}
	wg.Wait()
	select {
	case <-consumed:
	case <-time.After(5 * time.Second):
		t.Fatalf("%d of %d events applied", applied.Load(), n)
	}
}

// ctxHost's turns end when their context does, and then do the work a real
// turn does after cancellation: the log is saved by Host.Submit's own tail.
type ctxHost struct {
	stubHost
	mu        sync.Mutex
	cancelled atomic.Bool
	persisted atomic.Bool
	ignoreCtx chan struct{}
}

func (h *ctxHost) Submit(ctx context.Context, _, _ string) error {
	if h.ignoreCtx != nil {
		<-h.ignoreCtx
		return nil
	}
	<-ctx.Done()
	h.cancelled.Store(true)
	time.Sleep(50 * time.Millisecond)
	h.persisted.Store(true)
	return ctx.Err()
}

// Quitting with a turn running cancels it and waits for it, so what the turn
// does on its way out (persisting its log) finishes before Run returns and
// before the caller releases the session's lease.
func TestShutdownCancelsRunningTurnsAndWaitsForThem(t *testing.T) {
	h := &ctxHost{}
	r := lifecycleRunner(t, 8, h)
	r.runEffect(context.Background(), EffectSubmit{SessionID: "S1", Text: "work"})
	if len(r.Busy("")) != 1 {
		t.Fatal("the turn did not start")
	}
	if err := r.shutdownTurns(2 * time.Second); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if !h.cancelled.Load() {
		t.Fatal("the turn's context was never cancelled")
	}
	if !h.persisted.Load() {
		t.Fatal("shutdown returned before the cancelled turn finished persisting")
	}
}

// The wait is bounded, and a turn that ignores cancellation is reported, not
// waited on forever.
func TestShutdownWaitIsBounded(t *testing.T) {
	h := &ctxHost{ignoreCtx: make(chan struct{})}
	defer close(h.ignoreCtx)
	r := lifecycleRunner(t, 8, h)
	r.runEffect(context.Background(), EffectSubmit{SessionID: "S1", Text: "work"})
	start := time.Now()
	err := r.shutdownTurns(150 * time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "1 turn") {
		t.Fatalf("a turn that outlived the bound was not reported: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("shutdown took %s with a 150ms bound", took)
	}
}
