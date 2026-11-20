package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
)

// A verification or scan reads the worktree's diff, and a managed harness
// attempt writes its block into the same worktree. Before the checkout table
// took leases from the readers, only the harness-to-reader direction was
// guarded: a prepare could start on a worktree whose diff a verification was
// still reading, and the block ended up in the verdict.

// leasedVerify and leasedScan are the one place a test says how a reader is
// wired to the managed runner's checkout table.
func leasedVerify(m *ManagedRunner) func(*VerifyDeps) {
	return func(d *VerifyDeps) { d.Lease = m.Lease("verification") }
}

func leasedScan(m *ManagedRunner) func(*ScanDeps) {
	return func(d *ScanDeps) { d.Lease = m.Lease("scan") }
}

func leaseTestHarnessJob() *managedJob {
	return &managedJob{done: make(chan struct{}), harnessRef: ManagedHarness{Kind: "named", Name: "x"}}
}

// harnessCanTake reports whether a harness attempt is admitted on dir now, and
// leaves nothing behind: the attempt is ended again at once.
func harnessCanTake(m *ManagedRunner, dir string) error {
	job := leaseTestHarnessJob()
	m.mu.Lock()
	m.jobs[fmt.Sprintf("probe-%p", job)] = job
	m.mu.Unlock()
	err := m.enterCheckout(job, managedRun{Cwd: dir})
	close(job.done)
	return err
}

func blockingCritic(started chan<- struct{}, release <-chan struct{}) func(context.Context, string, int) (string, error) {
	var once sync.Once
	return func(ctx context.Context, prompt string, criteria int) (string, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return allPass(ctx, prompt, criteria)
	}
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Criterion 8: the failing-before test. A verification whose critic blocks
// holds the worktree; a harness prepare on the same worktree is refused until
// the verification ends, and then admitted.
func TestAHarnessPrepareIsRefusedWhileAVerificationHoldsTheWorktree(t *testing.T) {
	m := runnerWith()
	started, release := make(chan struct{}), make(chan struct{})
	h := newVerifyHarness(t, []string{"adds A"}, leasedVerify(m))
	h.critic.answer = blockingCritic(started, release)
	h.change()
	h.start("t", "req-1")
	awaitSignal(t, started, "the critic to be asked")

	err := harnessCanTake(m, h.repo)
	if busyCode(err) != ErrHarnessBusy {
		close(release)
		t.Fatalf("a harness prepare was admitted on a worktree a verification is reading: %v", err)
	}
	close(release)
	record := h.wait("t", "req-1")
	if record.Outcome != VerifyPassed {
		t.Fatalf("the verification outcome = %q (%v), want passed: nothing wrote into its worktree", record.Outcome, record.Degraded)
	}
	if err := harnessCanTake(m, h.repo); err != nil {
		t.Fatalf("the worktree was not released when the verification ended: %v", err)
	}
}

// The other direction: a prepare that has claimed the checkout but not yet
// written its block already makes the worktree busy, so a verification that
// starts in that window is refused and does not read a diff the block is about
// to be in.
func TestAVerificationIsRefusedWhileAHarnessPrepareHoldsTheWorktree(t *testing.T) {
	m := runnerWith()
	h := newVerifyHarness(t, []string{"adds A"}, leasedVerify(m))
	h.change()
	job := leaseTestHarnessJob()
	m.mu.Lock()
	m.jobs["prepare"] = job
	m.mu.Unlock()
	if err := m.enterCheckout(job, managedRun{Cwd: h.repo}); err != nil {
		t.Fatal(err)
	}
	_, err := h.runner.Start(context.Background(), h.params("t", "req-1"))
	var refusal *store.WorkbenchError
	if !errors.As(err, &refusal) || refusal.Code != ErrHarnessBusy {
		t.Fatalf("Start = %v, want %s: the prepare's block is about to be in the diff", err, ErrHarnessBusy)
	}
}

// A lease is released on every way a job can end.
func TestAVerificationLeaseIsReleasedWhateverEndsTheJob(t *testing.T) {
	cases := map[string]func(t *testing.T, h *verifyHarness, m *ManagedRunner){
		"settled": func(t *testing.T, h *verifyHarness, _ *ManagedRunner) {
			h.settled("t", "req-1")
		},
		"critic error": func(t *testing.T, h *verifyHarness, _ *ManagedRunner) {
			h.critic.answer = func(context.Context, string, int) (string, error) { return "", fmt.Errorf("provider down") }
			h.settled("t", "req-1")
		},
		"critic panic": func(t *testing.T, h *verifyHarness, _ *ManagedRunner) {
			h.critic.answer = func(context.Context, string, int) (string, error) { panic("critic crashed") }
			h.settled("t", "req-1")
		},
		"cancelled by close": func(t *testing.T, h *verifyHarness, _ *ManagedRunner) {
			started := make(chan struct{})
			h.critic.answer = blockingCritic(started, make(chan struct{}))
			h.start("t", "req-1")
			awaitSignal(t, started, "the critic to be asked")
			if err := h.runner.Close(); err != nil {
				t.Fatal(err)
			}
		},
		"refused before the claim": func(t *testing.T, h *verifyHarness, _ *ManagedRunner) {
			raw := json.RawMessage(fmt.Sprintf(`{"request_id":"bad","item_id":"t","worktree":%q,"base":"no-such-rev","head":"HEAD"}`, h.repo))
			if _, err := h.runner.Start(context.Background(), raw); err == nil {
				t.Fatal("a request naming no such revision was accepted")
			}
		},
		"replayed": func(t *testing.T, h *verifyHarness, _ *ManagedRunner) {
			h.settled("t", "req-1")
			if again := h.start("t", "req-1"); !again.Replayed {
				t.Fatalf("second Start = %+v, want a replay", again)
			}
		},
	}
	for name, end := range cases {
		t.Run(name, func(t *testing.T) {
			m := runnerWith()
			h := newVerifyHarness(t, []string{"adds A"}, leasedVerify(m))
			h.change()
			end(t, h, m)
			waitForFree(t, m, h.repo)
		})
	}
}

func TestAVerificationLeaseIsReleasedWhenTheJobTimesOut(t *testing.T) {
	m := runnerWith()
	started := make(chan struct{})
	h := newVerifyHarness(t, []string{"adds A"}, func(d *VerifyDeps) {
		leasedVerify(m)(d)
		d.Timeout = 300 * time.Millisecond
	})
	h.critic.answer = blockingCritic(started, make(chan struct{}))
	h.change()
	h.start("t", "req-1")
	awaitSignal(t, started, "the critic to be asked")
	h.wait("t", "req-1")
	waitForFree(t, m, h.repo)
}

func waitForFree(t *testing.T, m *ManagedRunner, dir string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = harnessCanTake(m, dir); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the worktree was never released: %v", err)
}

// A scan holds the checkout for as long as it runs, and gives it back when it
// finishes and when it is cancelled.
func TestAScanHoldsTheWorktreeUntilItEnds(t *testing.T) {
	for _, how := range []string{"finishes", "is cancelled"} {
		t.Run(how, func(t *testing.T) {
			m := runnerWith()
			root := scanRepo(t, map[string]string{"a.go": "package a\n"})
			h := newScanHarness(t, 0)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			h.dm.deadWait = func(ctx context.Context) error {
				once.Do(func() { close(started) })
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			deps := ScanDeps{
				Devmap: func(string) ScanDevmap { return h.dm }, Text: func(string) ScanText { return h.text },
				Gaps: func(string) ScanGaps { return h.gaps }, Board: h.board,
			}
			leasedScan(m)(&deps)
			runner, err := NewScanRunner(deps)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runner.Close() })
			h.runner = runner

			view := h.start(t, fmt.Sprintf(`{"repo_root":%q}`, root))
			awaitSignal(t, started, "the scan to reach the index")
			if err := harnessCanTake(m, root); busyCode(err) != ErrHarnessBusy {
				close(release)
				t.Fatalf("a harness prepare was admitted on a worktree a scan is reading: %v", err)
			}
			if how == "finishes" {
				close(release)
			} else if _, e := runner.Cancel(json.RawMessage(fmt.Sprintf(`{"scan_id":%q}`, view.ScanID))); e != nil {
				t.Fatal(e)
			}
			h.wait(t, view.ScanID)
			waitForFree(t, m, root)
			if how != "finishes" {
				close(release)
			}
		})
	}
}

// Two scans of one root share a job, and so share one lease: the second does
// not take a lease the first's end would not give back.
func TestAJoinedScanDoesNotTakeASecondLease(t *testing.T) {
	m := runnerWith()
	root := scanRepo(t, map[string]string{"a.go": "package a\n"})
	h := newScanHarness(t, 0)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.dm.deadWait = func(ctx context.Context) error {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	deps := ScanDeps{
		Devmap: func(string) ScanDevmap { return h.dm }, Text: func(string) ScanText { return h.text },
		Gaps: func(string) ScanGaps { return h.gaps }, Board: h.board,
	}
	leasedScan(m)(&deps)
	runner, err := NewScanRunner(deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	h.runner = runner
	params := fmt.Sprintf(`{"repo_root":%q}`, root)
	first := h.start(t, params)
	awaitSignal(t, started, "the scan to reach the index")
	if joined := h.start(t, params); joined.ScanID != first.ScanID {
		t.Fatalf("second scan = %s, want to join %s", joined.ScanID, first.ScanID)
	}
	close(release)
	h.wait(t, first.ScanID)
	waitForFree(t, m, root)
}

// A critic that ignores cancellation does not hold a job, its lease, or Close
// past the stop grace: the job settles as degraded naming the critic, and the
// abandoned critic cannot write the record afterwards.
func TestAJobEndsAtItsDeadlineWhateverTheCriticDoes(t *testing.T) {
	old := verifyStopGrace
	verifyStopGrace = 200 * time.Millisecond
	t.Cleanup(func() { verifyStopGrace = old })
	m := runnerWith()
	started, release := make(chan struct{}), make(chan struct{})
	h := newVerifyHarness(t, []string{"adds A"}, func(d *VerifyDeps) { leasedVerify(m)(d) })
	h.critic.answer = func(ctx context.Context, prompt string, criteria int) (string, error) {
		close(started)
		<-release // ignores ctx
		return allPass(ctx, prompt, criteria)
	}
	h.change()
	h.start("t", "req-1")
	awaitSignal(t, started, "the critic to be asked")
	begun := time.Now()
	if err := h.runner.Close(); err != nil {
		t.Fatalf("Close = %v, want it bounded by the stop grace, not the critic", err)
	}
	if took := time.Since(begun); took > 5*time.Second {
		t.Fatalf("Close took %s with a critic that ignores cancellation", took)
	}
	record := h.wait("t", "req-1")
	requireDegraded(t, record, "critic did not stop")
	waitForFree(t, m, h.repo)
	close(release)
	time.Sleep(100 * time.Millisecond)
	if again := h.wait("t", "req-1"); again.Outcome != VerifyDegraded || len(again.Criteria) != len(record.Criteria) {
		t.Fatalf("the abandoned critic changed the settled record: %+v", again)
	}
}
