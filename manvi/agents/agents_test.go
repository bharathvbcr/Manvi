package agents

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorder is a releaser that records what it was asked to give back, and can
// be made to fail or to observe the context it was handed.
type recorder struct {
	mu       sync.Mutex
	released []string
	// ctxLive records, per call, whether the context was still alive. This is
	// the assertion that matters: cleanup after a cancellation must not run on
	// the cancelled context.
	ctxLive []bool
	fail    map[string]error
}

func newRecorder() *recorder { return &recorder{fail: map[string]error{}} }

func (r *recorder) Release(ctx context.Context, taskID, token string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctxLive = append(r.ctxLive, ctx.Err() == nil)
	if err, bad := r.fail[taskID]; bad {
		return false, err
	}
	r.released = append(r.released, taskID)
	return true, nil
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.released...)
}

func pool(t *testing.T, rel Releaser) *Pool {
	t.Helper()
	p, err := New(2, 8, rel)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCancellingAParentReleasesChildLeases is the reason this package exists.
// Cancelling a context stops the work and leaves the store's lease row active,
// so the task stays locked until its TTL expires — including against the
// orchestrator's own retry.
func TestCancellingAParentReleasesChildLeases(t *testing.T) {
	rel := newRecorder()
	p := pool(t, rel)

	ctx, cancel := context.WithCancel(context.Background())
	holding := make(chan struct{})

	tasks := []Task{
		{Label: "searcher-a", Run: func(ctx context.Context, h *Holder) (any, error) {
			h.Add(Lease{TaskID: "TASK-A", Token: "tok-a"})
			close(holding)
			<-ctx.Done()
			return nil, ctx.Err()
		}},
		{Label: "searcher-b", Run: func(ctx context.Context, h *Holder) (any, error) {
			h.Add(Lease{TaskID: "TASK-B", Token: "tok-b"})
			<-ctx.Done()
			return nil, ctx.Err()
		}},
	}

	go func() {
		<-holding
		cancel()
	}()

	results, err := p.Run(ctx, tasks)
	if err != nil {
		t.Fatal(err)
	}

	got := rel.got()
	if len(got) != 2 {
		t.Fatalf("released %v, want both child leases handed back", got)
	}
	for _, r := range results {
		if len(r.ReleasedLeases) != 1 {
			t.Errorf("%s reported released = %v", r.Label, r.ReleasedLeases)
		}
		if len(r.OrphanedLeases) != 0 {
			t.Errorf("%s orphaned %v", r.Label, r.OrphanedLeases)
		}
	}

	// The releases must have run on a live context. A context derived from the
	// cancelled parent is already dead, so the release would fail instantly and
	// strand the lease the cleanup exists to recover.
	rel.mu.Lock()
	defer rel.mu.Unlock()
	for i, live := range rel.ctxLive {
		if !live {
			t.Fatalf("release %d ran on an already-cancelled context", i)
		}
	}
}

// TestAPanickingChildStillReleases: its leases are exactly as stranded as a
// cancelled child's, and a panic must not take the pool down.
func TestAPanickingChildStillReleases(t *testing.T) {
	rel := newRecorder()
	p := pool(t, rel)

	results, err := p.Run(context.Background(), []Task{
		{Label: "boom", Run: func(ctx context.Context, h *Holder) (any, error) {
			h.Add(Lease{TaskID: "TASK-P", Token: "tok"})
			panic("something went wrong")
		}},
		{Label: "fine", Run: func(ctx context.Context, h *Holder) (any, error) {
			return "ok", nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err == nil {
		t.Fatal("a panicking child reported success")
	}
	if got := rel.got(); len(got) != 1 || got[0] != "TASK-P" {
		t.Fatalf("released %v, want the panicking child's lease", got)
	}
	if results[1].Err != nil || results[1].Value != "ok" {
		t.Fatalf("a sibling was affected by the panic: %+v", results[1])
	}
}

// TestAnUnreleasableLeaseIsReportedNotSwallowed: an orphan keeps a task locked
// until its TTL runs out, and a report that omits it reads as a clean fan-out.
func TestAnUnreleasableLeaseIsReportedNotSwallowed(t *testing.T) {
	rel := newRecorder()
	rel.fail["TASK-STUCK"] = errors.New("store unreachable")
	p := pool(t, rel)

	results, err := p.Run(context.Background(), []Task{
		{Label: "stuck", Run: func(ctx context.Context, h *Holder) (any, error) {
			h.Add(Lease{TaskID: "TASK-STUCK", Token: "tok"})
			return nil, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results[0].OrphanedLeases) != 1 || results[0].OrphanedLeases[0] != "TASK-STUCK" {
		t.Fatalf("orphaned = %v, want the stranded task named", results[0].OrphanedLeases)
	}
	rep := Summarise(results)
	if rep.Clean() {
		t.Fatal("a fan-out with a stranded lease must not summarise as clean")
	}
}

// TestSelfReleasedLeasesAreNotReleasedTwice: a child that released its own
// lease and dropped it must not have the release repeated, which on a reused
// task id would release someone else's.
func TestSelfReleasedLeasesAreNotReleasedTwice(t *testing.T) {
	rel := newRecorder()
	p := pool(t, rel)
	_, err := p.Run(context.Background(), []Task{
		{Label: "tidy", Run: func(ctx context.Context, h *Holder) (any, error) {
			h.Add(Lease{TaskID: "TASK-T", Token: "tok"})
			h.Drop("TASK-T")
			return nil, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := rel.got(); len(got) != 0 {
		t.Fatalf("released %v, want nothing — the child released its own lease", got)
	}
}

// TestFanoutBeyondTheLimitIsRefusedNotTrimmed: running the first N and
// reporting success answers a question the caller did not ask.
func TestFanoutBeyondTheLimitIsRefusedNotTrimmed(t *testing.T) {
	rel := newRecorder()
	p, err := New(2, 3, rel)
	if err != nil {
		t.Fatal(err)
	}
	var ran atomic.Int32
	tasks := make([]Task, 5)
	for i := range tasks {
		tasks[i] = Task{Label: fmt.Sprintf("t%d", i), Run: func(ctx context.Context, h *Holder) (any, error) {
			ran.Add(1)
			return nil, nil
		}}
	}
	if _, err := p.Run(context.Background(), tasks); !errors.Is(err, ErrFanoutExceeded) {
		t.Fatalf("err = %v, want ErrFanoutExceeded", err)
	}
	if ran.Load() != 0 {
		t.Fatalf("%d tasks ran despite the refusal", ran.Load())
	}
}

// TestDepthIsBoundedInCode: a limit expressed as a prompt instruction is a
// limit the model can decline to follow.
func TestDepthIsBoundedInCode(t *testing.T) {
	rel := newRecorder()
	p, err := New(2, 4, rel)
	if err != nil {
		t.Fatal(err)
	}
	depth1, err := p.Child()
	if err != nil {
		t.Fatal(err)
	}
	depth2, err := depth1.Child()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := depth2.Child(); !errors.Is(err, ErrDepthExceeded) {
		t.Fatalf("err = %v, want ErrDepthExceeded at depth %d", err, depth2.Depth)
	}
	if depth2.Depth != 2 {
		t.Fatalf("depth = %d", depth2.Depth)
	}
}

// TestAPoolWithoutAReleaserIsRefused: discovering it at the moment a
// cancellation needs cleanup is discovering it too late.
func TestAPoolWithoutAReleaserIsRefused(t *testing.T) {
	if _, err := New(2, 4, nil); err == nil {
		t.Fatal("a pool without a releaser was constructed")
	}
	if _, err := New(2, 0, newRecorder()); err == nil {
		t.Fatal("a pool with a zero fan-out was constructed")
	}
}

// TestChildrenRunConcurrently: sequential execution would still pass every
// other test here while making fan-out pointless.
func TestChildrenRunConcurrently(t *testing.T) {
	rel := newRecorder()
	p := pool(t, rel)
	const n = 4
	var arrived sync.WaitGroup
	arrived.Add(n)
	tasks := make([]Task, n)
	for i := range tasks {
		tasks[i] = Task{Label: fmt.Sprintf("t%d", i), Run: func(ctx context.Context, h *Holder) (any, error) {
			arrived.Done()
			// Every child must reach this point before any may leave it, which
			// is only possible if they overlap.
			done := make(chan struct{})
			go func() { arrived.Wait(); close(done) }()
			select {
			case <-done:
				return nil, nil
			case <-time.After(5 * time.Second):
				return nil, errors.New("children did not overlap; execution is sequential")
			}
		}}
	}
	results, err := p.Run(context.Background(), tasks)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	}
}

// TestReleaseIsBoundedWhenTheStoreHangs: cleanup runs on a fresh context
// precisely because the parent's is cancelled, so it needs its own deadline or
// a wedged store hangs the shutdown it exists to complete.
func TestReleaseIsBoundedWhenTheStoreHangs(t *testing.T) {
	hang := releaserFunc(func(ctx context.Context, taskID, token string) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	})
	p, err := New(1, 2, hang)
	if err != nil {
		t.Fatal(err)
	}
	p.ReleaseTimeout = 200 * time.Millisecond

	done := make(chan []Result, 1)
	go func() {
		results, _ := p.Run(context.Background(), []Task{
			{Label: "held", Run: func(ctx context.Context, h *Holder) (any, error) {
				h.Add(Lease{TaskID: "TASK-H", Token: "tok"})
				return nil, nil
			}},
		})
		done <- results
	}()

	select {
	case results := <-done:
		if len(results[0].OrphanedLeases) != 1 {
			t.Fatalf("orphaned = %v, want the lease reported as stranded", results[0].OrphanedLeases)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a hung store blocked the fan-out's cleanup indefinitely")
	}
}

type releaserFunc func(ctx context.Context, taskID, token string) (bool, error)

func (f releaserFunc) Release(ctx context.Context, taskID, token string) (bool, error) {
	return f(ctx, taskID, token)
}

func TestAdaptiveFanoutLimit(t *testing.T) {
	cases := []struct {
		provider string
		in       int
		want     int
	}{
		{"local", 8, 2},
		{"local", 4, 2},
		{"local", 2, 2},
		{"local", 1, 1},
		{"local", 0, 1},
		{"anthropic", 8, 8},
		{"gemini", 16, 16},
		{"xai", 4, 4},
	}
	for _, tc := range cases {
		got := AdaptiveFanoutLimit(tc.provider, tc.in)
		if got != tc.want {
			t.Errorf("AdaptiveFanoutLimit(%q, %d) = %d, want %d", tc.provider, tc.in, got, tc.want)
		}
	}
}
