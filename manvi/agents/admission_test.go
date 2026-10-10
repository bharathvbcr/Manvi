package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// errCapRefused stands in for the ledger's own exhaustion error.
var errCapRefused = errors.New("budget exhausted")

// scriptedBudget answers Headroom from a script, one value per call, so a test
// decides exactly what the cap looks like at each admission. A call past the
// end of the script repeats its last value. before, when set, runs ahead of
// the answer to call n, which is how a test holds an admission until a child
// it started has finished.
type scriptedBudget struct {
	mu       sync.Mutex
	headroom []int64
	calls    int
	err      error
	before   map[int]func()
}

func (b *scriptedBudget) Headroom(context.Context) (int64, error) {
	b.mu.Lock()
	n := b.calls
	b.calls++
	wait := b.before[n]
	b.mu.Unlock()
	if wait != nil {
		wait()
	}
	if b.err != nil {
		return 0, b.err
	}
	return b.headroom[min(n, len(b.headroom)-1)], nil
}

func (b *scriptedBudget) Exhausted(err error) bool   { return errors.Is(err, errCapRefused) }
func (b *scriptedBudget) Render(amount int64) string { return fmt.Sprintf("%dn", amount) }

func meteredTasks(n int, reserve int64, ran *atomic.Int32) []Task {
	tasks := make([]Task, n)
	for i := range tasks {
		tasks[i] = Task{
			Label:   fmt.Sprintf("child-%d", i),
			Reserve: reserve,
			Run: func(context.Context, *Holder) (any, error) {
				ran.Add(1)
				return "done", nil
			},
		}
	}
	return tasks
}

// TestAFanOutWhoseFirstStepsCannotFitIsRefusedBeforeAnyChildStarts.
//
// Pool.Run started every child at once and left the cap to refuse them one
// request at a time. Four children each needing 30 against a headroom of 100
// got three first requests through and a fourth refused — after the first
// three were already working, and possibly writing. Asking up front, with the
// arithmetic in the refusal, is the only point at which "do not start" is
// still free.
func TestAFanOutWhoseFirstStepsCannotFitIsRefusedBeforeAnyChildStarts(t *testing.T) {
	var ran atomic.Int32
	b := &scriptedBudget{headroom: []int64{100}}
	p, err := New(1, 8, newRecorder())
	if err != nil {
		t.Fatal(err)
	}
	p.Budget = b

	results, err := p.Run(context.Background(), meteredTasks(4, 30, &ran))
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("Run = %v, %v; want ErrBudget — four children needing 30 cannot fit in 100", results, err)
	}
	if ran.Load() != 0 {
		t.Errorf("%d children ran; a refused fan-out must start none", ran.Load())
	}
	for _, want := range []string{"4 children", "120n", "100n", "child-0 30n", "child-3 30n"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not carry %q; the operator needs the numbers that decided it", err, want)
		}
	}
}

// TestAFanOutThatFitsRunsEveryChild is the other side of the refusal: the check
// must not narrow a fan-out the cap can afford.
func TestAFanOutThatFitsRunsEveryChild(t *testing.T) {
	var ran atomic.Int32
	p, err := New(1, 8, newRecorder())
	if err != nil {
		t.Fatal(err)
	}
	p.Budget = &scriptedBudget{headroom: []int64{120}}

	results, err := p.Run(context.Background(), meteredTasks(4, 30, &ran))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() != 4 {
		t.Errorf("%d of 4 children ran under a headroom that fits exactly", ran.Load())
	}
	if rep := Summarise(results); !rep.Clean() || rep.NotStarted != 0 {
		t.Errorf("report = %+v, want every child started and clean", rep)
	}
}

// TestAnUnreadableCapRefusesTheFanOut. A headroom that could not be read is not
// a headroom of "enough": admitting on it would be the check that could not run
// reporting the same thing as the check that passed.
func TestAnUnreadableCapRefusesTheFanOut(t *testing.T) {
	var ran atomic.Int32
	p, err := New(1, 8, newRecorder())
	if err != nil {
		t.Fatal(err)
	}
	p.Budget = &scriptedBudget{err: errors.New("ledger locked")}

	if _, err := p.Run(context.Background(), meteredTasks(2, 1, &ran)); err == nil || !strings.Contains(err.Error(), "ledger locked") {
		t.Fatalf("Run = %v; want a refusal naming the unreadable ledger", err)
	}
	if ran.Load() != 0 {
		t.Errorf("%d children ran against a cap nobody could read", ran.Load())
	}
}

// TestUnmeteredChildrenNeedNoHeadroom. A child placed on a provider the cap does
// not meter reserves nothing, so a fan-out of them is never asked about the cap
// at all — not even to read it.
func TestUnmeteredChildrenNeedNoHeadroom(t *testing.T) {
	var ran atomic.Int32
	b := &scriptedBudget{headroom: []int64{0}}
	p, err := New(1, 8, newRecorder())
	if err != nil {
		t.Fatal(err)
	}
	p.Budget = b

	if _, err := p.Run(context.Background(), meteredTasks(3, 0, &ran)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() != 3 || b.calls != 0 {
		t.Errorf("ran %d, headroom read %d times; want 3 and 0", ran.Load(), b.calls)
	}
}

// TestBudgetExhaustedMidFanOutStopsStartingChildren.
//
// Two ways the cap can run out after the up-front check passed: another session
// on the same ledger spends the headroom, or a started child's own request is
// refused. Either way the children not yet started must stay not started —
// reported as such, carrying no writes — and the children that did start keep
// every guarantee they had, their leases released included.
func TestBudgetExhaustedMidFanOutStopsStartingChildren(t *testing.T) {
	t.Run("a started child is refused by the cap", func(t *testing.T) {
		released := make(chan struct{})
		rec := &signallingReleaser{recorder: newRecorder(), released: released}
		b := &scriptedBudget{
			headroom: []int64{100},
			// Call 0 is the up-front check and call 1 admits child-0. Call 2,
			// the admission of child-1, waits until child-0's lease has been
			// handed back — which the pool does only after it has recorded
			// the child's failure — so the order is fixed by the test, not by
			// the scheduler.
			before: map[int]func(){2: func() { <-released }},
		}
		p, err := New(1, 8, rec)
		if err != nil {
			t.Fatal(err)
		}
		p.Budget = b

		var ran atomic.Int32
		tasks := meteredTasks(3, 30, &ran)
		tasks[0].Run = func(_ context.Context, h *Holder) (any, error) {
			ran.Add(1)
			h.Add(Lease{TaskID: "TASK-0", Token: "tok"})
			return map[string]any{"wrote": []string{"a.go"}}, fmt.Errorf("step 2: %w", errCapRefused)
		}

		results, err := p.Run(context.Background(), tasks)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if ran.Load() != 1 {
			t.Fatalf("%d children ran; after the cap refused child-0 no further child may start", ran.Load())
		}
		if got := results[0].ReleasedLeases; len(got) != 1 || got[0] != "TASK-0" {
			t.Errorf("child-0 released %v, want [TASK-0]; a child the cap stopped still hands its lease back", got)
		}
		if results[0].NotStarted != "" || results[0].Err == nil {
			t.Errorf("child-0 = %+v; it started and failed, and must be reported as failed", results[0])
		}
		for _, r := range results[1:] {
			assertNotStarted(t, r)
		}
		if !strings.Contains(results[1].Error, "sibling's request was refused") ||
			!strings.Contains(results[2].Error, "child-1, started before it, was held back") {
			t.Errorf("reasons = %q / %q; each held-back child must say why it, not another, was held",
				results[1].Error, results[2].Error)
		}
		rep := Summarise(results)
		if rep.Failed != 1 || rep.NotStarted != 2 || rep.Clean() {
			t.Errorf("report = %+v, want 1 failed, 2 not started, not clean", rep)
		}
	})

	t.Run("another session spends the headroom", func(t *testing.T) {
		rec := newRecorder()
		// Up front 100, enough for three at 30. By child-2's admission the
		// ledger shows 10 left: something outside this fan-out spent it.
		b := &scriptedBudget{headroom: []int64{100, 100, 100, 10}}
		p, err := New(1, 8, rec)
		if err != nil {
			t.Fatal(err)
		}
		p.Budget = b

		var ran atomic.Int32
		tasks := meteredTasks(3, 30, &ran)
		for i := range tasks {
			id := fmt.Sprintf("TASK-%d", i)
			tasks[i].Run = func(_ context.Context, h *Holder) (any, error) {
				ran.Add(1)
				h.Add(Lease{TaskID: id, Token: "tok"})
				return "done", nil
			}
		}

		results, err := p.Run(context.Background(), tasks)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if ran.Load() != 2 {
			t.Fatalf("%d children ran; want 2 — child-2 no longer fits", ran.Load())
		}
		assertNotStarted(t, results[2])
		if !strings.Contains(results[2].Error, "10n") || !strings.Contains(results[2].Error, "30n") {
			t.Errorf("not-started reason %q does not carry the numbers", results[2].Error)
		}
		if got := rec.got(); len(got) != 2 {
			t.Errorf("released %v; want the two started children's leases back", got)
		}
	})
}

func assertNotStarted(t *testing.T, r Result) {
	t.Helper()
	if r.NotStarted != NotStartedBudget {
		t.Errorf("%s: NotStarted = %q, want %q", r.Label, r.NotStarted, NotStartedBudget)
	}
	if !errors.Is(r.Err, ErrNotStarted) || !strings.HasPrefix(r.Error, "not started (budget)") {
		t.Errorf("%s: Err = %v, Error = %q; want ErrNotStarted rendered as 'not started (budget)'", r.Label, r.Err, r.Error)
	}
	if r.Value != nil || len(r.ReleasedLeases) > 0 || len(r.OrphanedLeases) > 0 {
		t.Errorf("%s: a child that never started reported %+v — it can have written and held nothing", r.Label, r)
	}
}

// signallingReleaser is a recorder that closes released after its first
// release, so a test can wait for the point at which the pool has finished
// with a child.
type signallingReleaser struct {
	*recorder
	released chan struct{}
	once     sync.Once
}

func (r *signallingReleaser) Release(ctx context.Context, taskID, token string) (bool, error) {
	ok, err := r.recorder.Release(ctx, taskID, token)
	r.once.Do(func() { close(r.released) })
	return ok, err
}
