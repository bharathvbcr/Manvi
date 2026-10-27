package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// A run whose agent process is not confirmed gone has not released its
// checkout. Close answering "not reaped" (or failing) says the process may be
// alive: restoring the block under it is a read-then-write over a file it may be
// writing, and admitting the next run puts a second agent on a checkout whose
// block the first still reads. The block and its record stay, the checkout stays
// held (every attempt on it is refused at once), and a background reaper keeps
// asking the provider whether it has exited and, when it says so, takes the block
// out and releases the checkout. Four states are tested: a second admission is
// refused; the restore is not executed while held; a later confirmation restores
// and releases; HarnessBusy refuses while held.

// unconfirmedLifecycle is a harness run whose session answers Close with the
// given reaping state; the test flips it to confirm the exit later.
func unconfirmedLifecycle(t *testing.T, root string, session **managedTestSession, configure ...func(*managedTestSession)) *managedLifecycle {
	t.Helper()
	var atStart atomic.Value
	var starts atomic.Int32
	m := harnessLifecycle(t, root, "claude", &atStart, &starts)
	prevInterval := reapInterval
	reapInterval = 5 * time.Millisecond
	t.Cleanup(func() { reapInterval = prevInterval })
	m.runner.factory = func(ctx context.Context, o codingagent.Options) (ManagedSession, error) {
		s := m.fixtureSession(ctx, o)
		s.withRequest = false
		s.unreaped = true
		for _, c := range configure {
			c(s)
		}
		m.session, *session = s, s
		return s, nil
	}
	return m
}

// otherAttempt registers an attempt beside the run under test, as Prepare does,
// and takes it out again before the runner is closed.
func (m *managedLifecycle) otherAttempt(id string, ref ManagedHarness) *managedJob {
	job := &managedJob{done: make(chan struct{}), cancel: func() {}, harnessRef: ref}
	m.runner.mu.Lock()
	m.runner.jobs[id] = job
	m.runner.mu.Unlock()
	m.t.Cleanup(func() {
		m.runner.mu.Lock()
		delete(m.runner.jobs, id)
		m.runner.mu.Unlock()
	})
	return job
}

func secondHarnessAttempt(m *managedLifecycle, root string) (*managedJob, error) {
	job := m.otherAttempt("second", ManagedHarness{Kind: "named", Name: "x"})
	return job, m.runner.enterCheckout(job, managedRun{Cwd: root})
}

func TestAnUnconfirmedExitKeepsTheCheckoutHeldAndTheBlockInPlace(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "project rules\n"})
	var session *managedTestSession
	m := unconfirmedLifecycle(t, root, &session)
	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", `{"kind":"named","name":"registered"}`))
	if err != nil {
		t.Fatal(err)
	}
	activate(t, m, prepared)
	run, raw := m.finished()
	if run.State != "unresolved" {
		t.Fatalf("a run whose process was not confirmed reaped is recorded %s, want unresolved: %s", run.State, raw)
	}

	if session == nil {
		t.Fatal("the factory never built the session")
	}
	// Restore is not executed while held: the reaper keeps asking (Close is called
	// again and again) and never writes the instruction file.
	var writes atomic.Int32
	prevWrite := pinnedWrite
	pinnedWrite = func(r, rel string, data []byte) (bool, error) {
		if rel == "CLAUDE.md" {
			writes.Add(1)
		}
		return prevWrite(r, rel, data)
	}
	t.Cleanup(func() { pinnedWrite = prevWrite })
	asked := session.closes.Load()
	waitFor(t, "the reaper to ask the provider again", func() bool { return session.closes.Load() >= asked+3 })
	if writes.Load() != 0 {
		t.Errorf("the instruction file was written %d times while the agent was unconfirmed", writes.Load())
	}

	if got := readFile(t, root, "CLAUDE.md"); !strings.Contains(got, harnessBeginMarker) {
		t.Errorf("the block was taken out from under a process that may be alive:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); err != nil {
		t.Errorf("the record of a block that is still in the file was removed: %v", err)
	}
	// HarnessBusy refuses while held, naming the run, since when, and the way out.
	e := m.runner.HarnessBusy(root)
	if e == nil || e.Code != ErrHarnessBusy {
		t.Fatalf("HarnessBusy = %v, want the checkout busy while the exit is unconfirmed", e)
	}
	for _, want := range []string{"managed run run ", "without confirmation", "nonce=", "CLAUDE.md", "restart `manvi serve`"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("the busy refusal lacks %q:\n%s", want, e.Message)
		}
	}
	// A second admission is refused at once, with or without a harness, and says the same.
	_, err = secondHarnessAttempt(m, root)
	if err == nil || busyCode(err) != ErrHarnessBusy || !strings.Contains(err.Error(), "restart `manvi serve`") {
		t.Errorf("a second run was admitted on a checkout whose agent may still be running, or not told how to recover: %v", err)
	}
	plain := m.otherAttempt("plain", ManagedHarness{})
	if err := m.runner.enterCheckout(plain, managedRun{Cwd: root}); busyCode(err) != ErrHarnessBusy {
		t.Errorf("a plain run was admitted beside a possibly live agent with a block: %v", err)
	}

	// The provider confirms the exit: the reaper takes the block out and releases
	// the checkout, with nobody asking.
	session.confirmed.Store(true)
	waitFor(t, "the checkout to be released", func() bool { return m.runner.HarnessBusy(root) == nil })
	if got := readFile(t, root, "CLAUDE.md"); got != "project rules\n" {
		t.Fatalf("the block was not taken out exactly: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the record outlived the restore")
	}
	if _, err := secondHarnessAttempt(m, root); err != nil {
		t.Fatalf("the checkout was not released once the exit was confirmed: %v", err)
	}
}

// One stuck agent must not slow anybody else: a refusal on the held checkout is
// immediate while the reaper is inside a slow Close, and another checkout's
// attempts are admitted at once. (The ask used to run inside Prepare on the one
// managed lane: three sequential Prepares against a one-second Close took three.)
func TestASlowCloseOnAHeldCheckoutDelaysNoAttempt(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "project rules\n"})
	var session *managedTestSession
	m := unconfirmedLifecycle(t, root, &session, func(s *managedTestSession) { s.closeDelay = 600 * time.Millisecond })
	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", `{"kind":"named","name":"registered"}`))
	if err != nil {
		t.Fatal(err)
	}
	activate(t, m, prepared)
	m.finished()
	other := resolvedDir(t)

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := secondHarnessAttempt(m, root); busyCode(err) != ErrHarnessBusy {
			t.Fatalf("attempt %d on the held checkout = %v, want E_HARNESS_BUSY", i+1, err)
		}
		job := m.otherAttempt("elsewhere", ManagedHarness{Kind: "named", Name: "x"})
		if err := m.runner.enterCheckout(job, managedRun{Cwd: other}); err != nil {
			t.Fatalf("attempt %d on another checkout = %v", i+1, err)
		}
	}
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Fatalf("six attempts took %v while the provider's Close was slow, want them answered at once", took)
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// abandon, which ends a preparation that never activated, holds the same rule.
func TestAnAbandonedRunWhoseExitIsUnconfirmedKeepsTheBlock(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "project rules\n"})
	var session *managedTestSession
	m := unconfirmedLifecycle(t, root, &session)
	if _, err := m.runner.Prepare(t.Context(), preparing("launch", `{"kind":"named","name":"registered"}`)); err != nil {
		t.Fatal(err)
	}
	m.runner.mu.Lock()
	job := m.runner.jobs["run"]
	m.runner.mu.Unlock()
	if job == nil {
		t.Fatal("the prepared run is not registered")
	}
	m.runner.abandon(job, errors.New("activation did not produce a receipt"))
	if got := readFile(t, root, "CLAUDE.md"); !strings.Contains(got, harnessBeginMarker) {
		t.Fatalf("an abandoned run with an unconfirmed process had its block taken out:\n%s", got)
	}
	if _, err := secondHarnessAttempt(m, root); busyCode(err) != ErrHarnessBusy {
		t.Fatalf("a second run was admitted: %v", err)
	}
}

// A confirmed exit is what it always was: the block is taken out and the
// checkout is free the moment the run ends.
func TestAConfirmedExitReleasesTheCheckoutAtOnce(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "project rules\n"})
	var atStart atomic.Value
	var starts atomic.Int32
	m := harnessLifecycle(t, root, "claude", &atStart, &starts)
	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", `{"kind":"named","name":"registered"}`))
	if err != nil {
		t.Fatal(err)
	}
	activate(t, m, prepared)
	if run, raw := m.finished(); run.State != "exited" {
		t.Fatalf("the run ended %s: %s", run.State, raw)
	}
	if _, err := secondHarnessAttempt(m, root); err != nil {
		t.Fatalf("a finished, confirmed run still holds its checkout: %v", err)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "project rules\n" {
		t.Fatalf("the block was not taken out: %q", got)
	}
}

// An agent that exited 0 while an orphaned grandchild kept its stdout pipe open
// is reaped: os/exec reports ErrWaitDelay beside the exit, and Close hands that
// error back with Reaped true, every time (the wait error is sticky). The agent is
// gone, so the block is taken out and the checkout is released as after a clean
// exit, and the error is kept on the run as a note. Only an agent that is not
// reaped holds the checkout.
func TestAReapedAgentWithAWaitDelayErrorReleasesTheCheckout(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "project rules\n"})
	var session *managedTestSession
	m := unconfirmedLifecycle(t, root, &session, func(s *managedTestSession) {
		s.unreaped = false
		s.closeErr = errors.New("exec: WaitDelay expired before I/O complete")
	})
	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", `{"kind":"named","name":"registered"}`))
	if err != nil {
		t.Fatal(err)
	}
	activate(t, m, prepared)
	_, raw := m.finished()
	if !strings.Contains(string(raw), "WaitDelay") {
		t.Errorf("the wait error is not on the run as a note: %s", raw)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "project rules\n" {
		t.Fatalf("the block of a run whose agent exited was left in the file:\n%s", got)
	}
	if _, err := secondHarnessAttempt(m, root); err != nil {
		t.Fatalf("the checkout of a run whose agent exited is still held: %v", err)
	}
	if e := m.runner.HarnessBusy(root); e != nil {
		t.Fatalf("HarnessBusy = %v after the agent exited", e)
	}
}
