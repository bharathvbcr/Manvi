package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Adversarial tests for lane w24-verdicts-b, criteria 8 (leases in the managed
// runner's checkout table) and 9 (the owner of a running verification record).

func zadvBlockedUntil(release <-chan struct{}, started chan<- struct{}) func(context.Context, string, int) (string, error) {
	var once sync.Once
	return func(ctx context.Context, prompt string, criteria int) (string, error) {
		once.Do(func() { close(started) })
		<-release
		return allPass(ctx, prompt, criteria)
	}
}

// A lease handed back twice, from two goroutines, takes nothing else with it:
// not a second lease on the same worktree, not one taken after the first ended.
func TestZadv_w24vb_LeaseReleaseIsIdempotentAndIndependent(t *testing.T) {
	m := runnerWith()
	dir := resolvedDir(t)
	lease := m.Lease("verification")
	first, e := lease(dir)
	if e != nil {
		t.Fatal(e)
	}
	second, e := lease(dir)
	if e != nil {
		t.Fatalf("a second reader on a worktree was refused: %v", e)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); first() }()
	}
	wg.Wait()
	if err := harnessCanTake(m, dir); busyCode(err) != ErrHarnessBusy {
		t.Fatalf("releasing the first lease eight times freed a worktree the second still reads: %v", err)
	}
	second()
	if err := harnessCanTake(m, dir); err != nil {
		t.Fatalf("both leases are released and the worktree is still held: %v", err)
	}
	third, e := lease(dir)
	if e != nil {
		t.Fatal(e)
	}
	first() // a stale release, long after its lease ended
	if err := harnessCanTake(m, dir); busyCode(err) != ErrHarnessBusy {
		t.Fatalf("a stale release removed a lease taken after it: %v", err)
	}
	third()
}

// A reader and a harness attempt that arrive together are never both admitted.
func TestZadv_w24vb_LeaseAndEnterCheckoutNeverBothAdmitted(t *testing.T) {
	dir := resolvedDir(t)
	both := 0
	for i := range 500 {
		m := runnerWith()
		job := leaseTestHarnessJob()
		m.mu.Lock()
		m.jobs["attempt"] = job
		m.mu.Unlock()
		var leaseErr *Error
		var enterErr error
		var release func()
		gate := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-gate
			release, leaseErr = m.Lease("verification")(dir)
		}()
		go func() {
			defer wg.Done()
			<-gate
			enterErr = m.enterCheckout(job, managedRun{Cwd: dir})
		}()
		close(gate)
		wg.Wait()
		if leaseErr == nil && enterErr == nil {
			both++
			t.Errorf("iteration %d: a reader and a harness attempt were both admitted on one worktree", i)
			if both > 3 {
				return
			}
		}
		if release != nil {
			release()
		}
		close(job.done)
	}
}

// The lease is the checkout, not how its path is spelled: a link to the
// worktree and a directory inside it are the same files.
func TestZadv_w24vb_LeaseHoldsThroughAnAliasAndASubdirectory(t *testing.T) {
	m := runnerWith()
	dir := resolvedDir(t)
	sub := filepath.Join(dir, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(resolvedDir(t), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	release, e := m.Lease("verification")(alias)
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	for name, cwd := range map[string]string{"directory": dir, "subdirectory": sub, "alias": alias} {
		if err := harnessCanTake(m, cwd); busyCode(err) != ErrHarnessBusy {
			t.Errorf("a harness prepare in the %s of a leased worktree was admitted: %v", name, err)
		}
	}
	other := resolvedDir(t)
	if err := harnessCanTake(m, other); err != nil {
		t.Errorf("a lease held an unrelated checkout: %v", err)
	}
}

// Starvation: a verification whose critic never returns holds the worktree for
// as long as the critic does. The job context ends at the deadline, so the
// lease should end with it, whatever the critic does with its context.
func TestZadv_w24vb_ALeaseIsBoundedByTheJobDeadline(t *testing.T) {
	m := runnerWith()
	started, release := make(chan struct{}), make(chan struct{})
	h := newVerifyHarness(t, []string{"adds A"}, func(d *VerifyDeps) {
		leasedVerify(m)(d)
		d.Timeout = 300 * time.Millisecond
	})
	t.Cleanup(func() { close(release) })
	h.critic.answer = zadvBlockedUntil(release, started)
	h.change()
	h.start("t", "req-1")
	awaitSignal(t, started, "the critic to be asked")
	deadline := time.Now().Add(4 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = harnessCanTake(m, h.repo); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("a verification past its 300ms deadline still holds the worktree 4s later, because its critic ignores cancellation: %v", err)
}

// A request whose context is already cancelled, and a Close that races a Start,
// leave no lease behind.
func TestZadv_w24vb_CancelledStartAndCloseRaceLeaveNoLease(t *testing.T) {
	for i := range 12 {
		m := runnerWith()
		h := newVerifyHarness(t, []string{"adds A"}, leasedVerify(m))
		h.change()
		ctx, cancel := context.WithCancel(context.Background())
		if i%2 == 0 {
			cancel()
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = h.runner.Start(ctx, h.params("t", fmt.Sprintf("req-%d", i)))
		}()
		go func() {
			defer wg.Done()
			if i%3 == 0 {
				time.Sleep(time.Millisecond)
			}
			_ = h.runner.Close()
		}()
		wg.Wait()
		cancel()
		waitForFree(t, m, h.repo)
	}
}

// Two verifications of different tasks on one worktree are both admitted, and
// the worktree stays held until the last of them ends.
func TestZadv_w24vb_TwoVerificationsOnOneWorktreeHoldItUntilTheLastEnds(t *testing.T) {
	m := runnerWith()
	h := newVerifyHarness(t, []string{"adds A"}, leasedVerify(m))
	h.putItem("u", `["adds A"]`)
	h.change()
	var calls atomic.Int32
	started := make(chan struct{}, 2)
	rel := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	var closeOnce [2]sync.Once
	t.Cleanup(func() {
		for i := range rel {
			closeOnce[i].Do(func() { close(rel[i]) })
		}
	})
	h.critic.answer = func(ctx context.Context, prompt string, criteria int) (string, error) {
		n := calls.Add(1) - 1
		started <- struct{}{}
		<-rel[n]
		return allPass(ctx, prompt, criteria)
	}
	h.start("t", "req-1")
	h.start("u", "req-2")
	awaitSignal(t, func() <-chan struct{} {
		c := make(chan struct{})
		go func() { <-started; <-started; close(c) }()
		return c
	}(), "both critics")
	closeOnce[0].Do(func() { close(rel[0]) })
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := h.runner.Get(context.Background(), json.RawMessage(`{"item_id":"t","request_id":"req-1"}`))
		b, _ := h.runner.Get(context.Background(), json.RawMessage(`{"item_id":"u","request_id":"req-2"}`))
		if (a.Record.State == verifySettled) != (b.Record.State == verifySettled) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := harnessCanTake(m, h.repo); busyCode(err) != ErrHarnessBusy {
		t.Fatalf("one of two verifications ended and the worktree was admitted to a harness while the other still reads it: %v", err)
	}
	closeOnce[1].Do(func() { close(rel[1]) })
	h.wait("t", "req-1")
	h.wait("u", "req-2")
	waitForFree(t, m, h.repo)
}
