package serve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

func exitFor(reaped bool, code int) codingagent.Exit {
	return codingagent.Exit{Reaped: reaped, Code: code}
}

// Adversarial tests for the managed process-group lifecycle of ft-43e7cb92
// (w8-procgroup, criteria 3-6).

// advFaultStore refuses the next failFinish calls of runs.finish, as a store that
// is busy or briefly unavailable would, and counts every one.
type advFaultStore struct {
	managedStore
	failFinish atomic.Int32
	finishes   atomic.Int32
}

func (s *advFaultStore) Workbench(ctx context.Context, method string, input json.RawMessage) (json.RawMessage, error) {
	if method == "runs.finish" {
		s.finishes.Add(1)
		if s.failFinish.Add(-1) >= 0 {
			return nil, errors.New("injected: the store refused runs.finish")
		}
	}
	return s.managedStore.Workbench(ctx, method, input)
}

// advPlainRun is plainRun with a fault store in place before the run starts.
func advPlainRun(t *testing.T, failFirst int32, configure ...func(*managedTestSession)) (string, *managedTestSession, *managedLifecycle, *advFaultStore) {
	t.Helper()
	root := scanRepo(t, map[string]string{"CLAUDE.md": "project rules\n"})
	var session *managedTestSession
	m := unconfirmedLifecycle(t, root, &session, configure...)
	fault := &advFaultStore{managedStore: m.runner.client}
	fault.failFinish.Store(failFirst)
	m.runner.client = fault
	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", ""))
	if err != nil {
		t.Fatal(err)
	}
	activate(t, m, prepared)
	m.finished()
	if session == nil {
		t.Fatal("the factory never built the session")
	}
	return root, session, m, fault
}

// The checkout is a directory, compared by identity. A plain run held at the
// worktree root may still be writing anywhere under it, but a run whose cwd is a
// subdirectory (a monorepo package, which is where agents are most often pointed)
// is a different directory to the guard, so it is admitted and so is the
// verification of that subdirectory. The hold is then a hold of one spelling of
// the checkout, not of the files the group can write.
func TestAHeldPlainCheckoutAlsoRefusesARunStartedInASubdirectoryOfIt(t *testing.T) {
	root, _, m := plainRun(t, liveGroup)
	sub := filepath.Join(root, "pkg", "inner")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	if e := m.runner.HarnessBusy(root); e == nil {
		t.Fatal("the root is not held, so the scenario did not arise")
	}
	err := m.runner.enterCheckout(m.otherAttempt("sub", ManagedHarness{}), managedRun{Cwd: sub})
	if busyCode(err) != ErrHarnessBusy {
		t.Errorf("a run in %s was admitted while its worktree root is held for a live process group: %v", sub, err)
	}
	if e := m.runner.HarnessBusy(sub); e == nil {
		t.Errorf("work.verify.task on %s is not refused while the worktree root is held", sub)
	}
}

// A revision that fails once (the store is busy for a moment) must not leave the
// run unresolved for good: the hold has ended and the group is confirmed gone,
// and nothing will ask again. The error is joined onto job.err, which nothing
// reads after the run has ended.
func TestARevisionTheStoreRefusesOnceIsRetriedNotLostForGood(t *testing.T) {
	root, session, m, fault := advPlainRun(t, 0, liveGroup)
	if run, _ := storedRun(t, m); run.State != "unresolved" {
		t.Fatalf("the run is %s, want unresolved", run.State)
	}
	fault.failFinish.Store(1)
	session.confirmed.Store(true)
	waitFor(t, "the checkout to be released", func() bool { return m.runner.HarnessBusy(root) == nil })
	waitFor(t, "the revision to have been attempted", func() bool { return fault.finishes.Load() >= 2 })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if run, _ := storedRun(t, m); run.State == "exited" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	run, _ := storedRun(t, m)
	t.Fatalf("the run is %s after the store refused the revision once and then recovered: it stays unresolved with the group long gone", run.State)
}

// The first write failing (runs.finish refused while the hold is taken) leaves
// the stored run in "running", which the `from` guard of the revision does not
// accept, so when the group is later confirmed gone the reaper releases the
// checkout and never records anything: a process-less run that the store still
// calls running.
func TestAFirstFinishTheStoreRefusedIsRepairedWhenTheGroupIsConfirmedGone(t *testing.T) {
	root, session, m, _ := advPlainRun(t, 1, liveGroup)
	if run, _ := storedRun(t, m); run.State != "running" {
		t.Skipf("the first finish was recorded (%s), so the scenario did not arise", run.State)
	}
	session.confirmed.Store(true)
	waitFor(t, "the checkout to be released", func() bool { return m.runner.HarnessBusy(root) == nil })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if run, _ := storedRun(t, m); run.State != "running" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the run is still recorded running although its agent and group are confirmed gone and the checkout released")
}

// Many attempts arriving on an exhausted hold each restart a reaper, so several
// reapers can find the group gone at once. Exactly one ends the hold and exactly
// one writes the revision: the stored revision moves by one. Run with -race.
func TestConcurrentReapersAndAttemptsRevisePlainRunExactlyOnce(t *testing.T) {
	prev := maxReapAttempts
	maxReapAttempts = 1
	t.Cleanup(func() { maxReapAttempts = prev })
	root, session, m := plainRun(t, liveGroup)
	info := mustResolvedInfo(t, root)
	waitFor(t, "the reaper to give up", func() bool { return strings.Contains(m.runner.heldReason(root, info), "stopped asking") })
	before, _ := storedRun(t, m)

	var wg sync.WaitGroup
	start := make(chan struct{})
	session.confirmed.Store(true)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			job := &managedJob{done: make(chan struct{}), cancel: func() {}}
			_ = m.runner.enterCheckout(job, managedRun{Cwd: root})
		}()
	}
	close(start)
	wg.Wait()
	waitFor(t, "the hold to end", func() bool { return m.runner.HarnessBusy(root) == nil })
	waitFor(t, "the revision", func() bool { run, _ := storedRun(t, m); return run.State == "exited" })
	time.Sleep(100 * time.Millisecond) // a second writer, if there is one, lands here
	after, _ := storedRun(t, m)
	if after.Revision != before.Revision+1 {
		t.Fatalf("the run's revision went %d -> %d, want exactly one write", before.Revision, after.Revision)
	}
}

// A new attempt admitted the instant the hold ends must not be disturbed by the
// revision of the old run, and the revision must still land: they touch different
// runs and different state.
func TestARevisionRacingTheNextAttemptOnTheReleasedCheckout(t *testing.T) {
	root, session, m := plainRun(t, liveGroup)
	var admitted atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
			job := &managedJob{done: make(chan struct{}), cancel: func() {}}
			if err := m.runner.enterCheckout(job, managedRun{Cwd: root}); err == nil {
				admitted.Store(true)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	session.confirmed.Store(true)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("no attempt was admitted after the group was confirmed gone")
	}
	if !admitted.Load() {
		t.Fatal("not admitted")
	}
	waitFor(t, "the old run to be revised", func() bool { run, _ := storedRun(t, m); return run.State == "exited" })
}

// ManagedRunner.Close waits codingagent's close budget (a provider Close can take
// ten seconds) and a margin in all for jobs to finish. A provider Close that
// outlasts that must give the operator a bounded, typed refusal, the run must
// still reach a terminal record when Close finally returns, and the reaper a
// finish starts must not keep asking the provider once the runner is closed.
func TestShutdownWithASlowProviderCloseIsBoundedAndTheRunStillSettles(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "project rules\n"})
	var session *managedTestSession
	m := unconfirmedLifecycle(t, root, &session, func(s *managedTestSession) {
		s.unreaped = false
		s.groupLive = true
		s.liveGroupErr = errors.New("managed provider process group 123 still has a live member after SIGKILL")
		s.closeDelay = managedCloseWait() + 1500*time.Millisecond
	})
	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", ""))
	if err != nil {
		t.Fatal(err)
	}
	activate(t, m, prepared)
	m.runner.mu.Lock()
	job := m.runner.jobs["run"]
	m.runner.mu.Unlock()
	if job == nil || session == nil {
		t.Fatal("the activated run has no job or no provider session")
	}

	start := time.Now()
	err = m.runner.Close()
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "run run") {
		t.Fatalf("Close = %v after %v, want the typed refusal naming the run that is still closing", err, took)
	}
	if took > managedCloseWait()+800*time.Millisecond {
		t.Fatalf("Close took %v, want it bounded by its own %v", took, managedCloseWait())
	}
	select {
	case <-job.done:
	case <-time.After(15 * time.Second):
		t.Fatal("the job never finished after Close gave up: the drive goroutine is leaked")
	}
	if run, _ := storedRun(t, m); run.State != "unresolved" && run.State != "exited" {
		t.Fatalf("the run is %s after shutdown, want a terminal record", run.State)
	}
	asked := session.closes.Load()
	time.Sleep(300 * time.Millisecond) // reapInterval is 5ms here
	if again := session.closes.Load(); again > asked+1 {
		t.Fatalf("the reaper kept asking the provider after the runner closed: %d -> %d", asked, again)
	}
}

// runOutcome is the one mapping, so its corners are pinned here: a negative code
// (the process was signalled; the real Close reports -1) is an exit with no code,
// never a negative code and never unresolved; any error or an unreaped agent is
// unresolved with no code.
func TestRunOutcomeCorners(t *testing.T) {
	cases := []struct {
		name    string
		reaped  bool
		code    int
		err     error
		outcome string
		hasCode bool
	}{
		{"clean", true, 0, nil, "exited", true},
		{"signalled", true, -1, nil, "exited", false},
		{"reaped with an error", true, 0, errors.New("WaitDelay"), "unresolved", false},
		{"not reaped", false, 0, nil, "unresolved", false},
		{"not reaped and an error", false, 3, errors.New("x"), "unresolved", false},
		{"large code", true, 255, nil, "exited", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			outcome, code := runOutcome(exitFor(c.reaped, c.code), c.err)
			if outcome != c.outcome || (code != nil) != c.hasCode {
				t.Fatalf("runOutcome = %s, %v; want %s, code present %v", outcome, code, c.outcome, c.hasCode)
			}
			if code != nil && *code != c.code {
				t.Fatalf("code = %d, want %d", *code, c.code)
			}
		})
	}
}
