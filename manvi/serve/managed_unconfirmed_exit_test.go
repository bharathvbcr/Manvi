package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// A run whose agent process is not confirmed gone has not released its
// checkout. Close answering "not reaped" (or failing) says the process may be
// alive: restoring the block under it is a read-then-write over a file it may be
// writing, and admitting the next run puts a second agent on a checkout whose
// block the first still reads. The block and its record stay, the checkout stays
// held, and a later attempt re-asks the provider whether it has exited.

// unconfirmedLifecycle is a harness run whose session answers Close with the
// given reaping state; the test flips it to confirm the exit later.
func unconfirmedLifecycle(t *testing.T, root string, session **managedTestSession) *managedLifecycle {
	t.Helper()
	var atStart atomic.Value
	var starts atomic.Int32
	m := harnessLifecycle(t, root, "claude", &atStart, &starts)
	m.runner.factory = func(ctx context.Context, o codingagent.Options) (ManagedSession, error) {
		s := m.fixtureSession(ctx, o)
		s.withRequest = false
		s.unreaped = true
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

	if got := readFile(t, root, "CLAUDE.md"); !strings.Contains(got, harnessBeginMarker) {
		t.Errorf("the block was taken out from under a process that may be alive:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); err != nil {
		t.Errorf("the record of a block that is still in the file was removed: %v", err)
	}
	if e := m.runner.HarnessBusy(root); e == nil || e.Code != ErrHarnessBusy {
		t.Errorf("HarnessBusy = %v, want the checkout busy while the exit is unconfirmed", e)
	}
	if _, err := secondHarnessAttempt(m, root); busyCode(err) != ErrHarnessBusy {
		t.Errorf("a second run was admitted on a checkout whose agent may still be running: %v", err)
	}
	// A plain attempt reads the block as repository content too.
	plain := m.otherAttempt("plain", ManagedHarness{})
	if err := m.runner.enterCheckout(plain, managedRun{Cwd: root}); busyCode(err) != ErrHarnessBusy {
		t.Errorf("a plain run was admitted beside a possibly live agent with a block: %v", err)
	}

	// The provider confirms the exit when it is asked again: the block is taken
	// out and the checkout is released.
	session.unreaped = false
	job, err := secondHarnessAttempt(m, root)
	if err != nil {
		t.Fatalf("the checkout was not released once the exit was confirmed: %v", err)
	}
	if !strings.Contains(job.recovery, "CLAUDE.md") {
		t.Fatalf("the release did not report the block it took out: %q", job.recovery)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "project rules\n" {
		t.Fatalf("the block was not taken out exactly: %q", got)
	}
	if e := m.runner.HarnessBusy(root); e != nil && !strings.Contains(e.Message, "in progress") {
		t.Fatalf("HarnessBusy = %v after the release", e)
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
