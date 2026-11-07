package serve

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for what the second adversarial review of the hold found.

// heldRun is a harness run whose agent is not confirmed gone, ended and held.
func heldRun(t *testing.T, files map[string]string, configure ...func(*managedTestSession)) (string, *managedTestSession, *managedLifecycle) {
	t.Helper()
	root := scanRepo(t, files)
	var session *managedTestSession
	m := unconfirmedLifecycle(t, root, &session, configure...)
	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", `{"kind":"named","name":"registered"}`))
	if err != nil {
		t.Fatal(err)
	}
	activate(t, m, prepared)
	m.finished()
	if session == nil {
		t.Fatal("the factory never built the session")
	}
	return root, session, m
}

func noHolds(m *managedLifecycle) func() bool {
	return func() bool {
		m.runner.mu.Lock()
		defer m.runner.mu.Unlock()
		for _, b := range m.runner.blocked {
			if b.session != nil {
				return false
			}
		}
		return true
	}
}

// A checkout directory deleted and recreated is a new directory to the guard, so
// a second run is admitted there while the first is held. The held run's reaper
// must act on the record only if it is the held run's own: the record at the path
// is the live second run's, and its block and record are left alone.
func TestTheReaperLeavesALiveRunsBlockAndRecordAlone(t *testing.T) {
	root, session, m := heldRun(t, map[string]string{"CLAUDE.md": "project rules\n"})
	if err := os.Rename(root, root+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, root, "CLAUDE.md", "rules2\n")
	job := m.otherAttempt("second", ManagedHarness{Kind: "named", Name: "x"})
	if err := m.runner.enterCheckout(job, managedRun{Cwd: root}); err != nil {
		t.Fatalf("the recreated directory was refused, so the scenario did not arise: %v", err)
	}
	live, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
	if err != nil {
		t.Fatal(err)
	}
	before := readFile(t, root, "CLAUDE.md")
	record := readFile(t, root, harnessPendingFile)

	session.confirmed.Store(true)
	waitFor(t, "the old hold to be released", noHolds(m))
	if got := readFile(t, root, "CLAUDE.md"); got != before {
		t.Fatalf("the reaper of the old hold took the live run's block out:\n%q\n->\n%q", before, got)
	}
	if got := readFile(t, root, harnessPendingFile); got != record {
		t.Fatalf("the reaper of the old hold removed or changed the live run's record:\n%s", got)
	}
	if err := live.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "rules2\n" {
		t.Fatalf("the live run's own restore did not give the file back: %q", got)
	}
}

// A block the agent edited inside, found when the hold is released, is reported as
// it is when a run ends normally: the record is kept, HarnessBusy refuses naming
// it, and the next attempt reports it once. It is not cleared and released unseen.
func TestAnEditedBlockFoundAtReleaseIsReportedNotReleasedSilently(t *testing.T) {
	root, session, m := heldRun(t, map[string]string{"CLAUDE.md": "project rules\n"})
	got := readFile(t, root, "CLAUDE.md")
	i := strings.Index(got, harnessBeginMarker)
	nl := strings.Index(got[i:], "\n")
	edited := got[:i+nl+1] + "AGENT EDIT\n" + got[i+nl+1:]
	writeRaw(t, root, "CLAUDE.md", edited)

	session.confirmed.Store(true)
	waitFor(t, "the hold to be released", noHolds(m))
	if after := readFile(t, root, "CLAUDE.md"); after != edited {
		t.Fatalf("the edited file was changed:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); err != nil {
		t.Fatalf("the record of an edited block was cleared unreported: %v", err)
	}
	busy := m.runner.HarnessBusy(root)
	if busy == nil {
		t.Fatal("HarnessBusy is nil: the leftover block was released silently")
	}
	for _, want := range []string{"modified harness block", "run run", "has since exited"} {
		if !strings.Contains(busy.Message, want) {
			t.Errorf("the busy refusal lacks %q:\n%s", want, busy.Message)
		}
	}
	// The next attempt reports it, once, and clears it.
	first := m.otherAttempt("next", ManagedHarness{Kind: "named", Name: "x"})
	if err := m.runner.enterCheckout(first, managedRun{Cwd: root}); err == nil || !strings.Contains(err.Error(), "modified harness block") {
		t.Fatalf("the next attempt = %v, want the modified-block refusal", err)
	}
	if err := m.runner.enterCheckout(first, managedRun{Cwd: root}); err != nil {
		t.Fatalf("the refusal was made twice: %v", err)
	}
}

// removeRecord is read, compare, unlink. Another run's record written between
// the compare and the unlink must survive: every mutation of the record is under
// one lock, so the writer waits for the remover.
func TestRemovingARecordIsAtomicWithAnotherRunsWriteOfIt(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "rules\n"})
	a, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
	if err != nil {
		t.Fatal(err)
	}
	var b *harnessMaterialisation
	var bDone chan struct{}
	recordReadHook = func() {
		recordReadHook = nil
		bDone = make(chan struct{})
		go func() {
			defer close(bDone)
			b, _ = materialiseHarness(root, "claude", specWithSections(t, houseRules))
		}()
		select { // unlocked, run B finishes in this window; locked, it waits for A
		case <-bDone:
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Cleanup(func() { recordReadHook = nil })
	if err := a.Restore(); err != nil {
		t.Fatal(err)
	}
	<-bDone
	if b == nil {
		t.Fatal("run B was not materialised")
	}
	if got := readFile(t, root, harnessPendingFile); !strings.Contains(got, b.pending.Nonce) {
		t.Fatalf("run B's record was removed by run A's end (or replaced): %s", got)
	}
	if err := b.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "rules\n" {
		t.Fatalf("run B's block was not taken out: %q", got)
	}
}

// What an agent names is not put in a command or shown beyond the checkout: a link
// target is untrusted, so the remedy is in words, an outside target is not named,
// and an inside one is quoted.
func TestAHostileLinkTargetIsNeitherARunnableCommandNorLeaked(t *testing.T) {
	cases := []struct {
		target  string
		outside bool
	}{
		{"x; touch PWNED #", false},
		{"$(touch PWNED)", false},
		{"/etc/hosts", true},
		{"../../etc/hosts", true},
	}
	for _, c := range cases {
		t.Run(c.target, func(t *testing.T) {
			root := scanRepo(t, map[string]string{"CLAUDE.md": "rules\n"})
			m, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(root, "CLAUDE.md")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(c.target, filepath.Join(root, "CLAUDE.md")); err != nil {
				t.Skipf("no symbolic links here: %v", err)
			}
			texts := []string{m.Restore().Error()}
			if busy := (&ManagedRunner{jobs: map[string]*managedJob{}}).HarnessBusy(root); busy != nil {
				texts = append(texts, busy.Message)
			} else {
				t.Fatal("HarnessBusy is nil")
			}
			for _, text := range texts {
				for _, bad := range []string{"rm CLAUDE.md", "cp ", " && ", "mv "} {
					if strings.Contains(text, bad) {
						t.Errorf("the message carries a runnable command (%q):\n%s", bad, text)
					}
				}
				if c.outside {
					if !strings.Contains(text, "points outside the checkout") {
						t.Errorf("an outside target is not described as outside:\n%s", text)
					}
					if strings.Contains(text, "etc/hosts") {
						t.Errorf("an outside target is shown:\n%s", text)
					}
				} else if !strings.Contains(text, `symbolic link to "`+c.target+`"`) {
					t.Errorf("an inside target is not shown quoted:\n%s", text)
				}
			}
		})
	}
}

// After the reaper has used its attempts the hold stays, but an attempt on the
// checkout restarts it (off the lane; the attempt is still refused at once), so an
// agent that exits later is still noticed.
func TestAnExhaustedHoldIsRecheckedWhenAnAttemptArrives(t *testing.T) {
	prev := maxReapAttempts
	maxReapAttempts = 2
	t.Cleanup(func() { maxReapAttempts = prev })
	root, session, m := heldRun(t, map[string]string{"CLAUDE.md": "project rules\n"})
	info := mustResolvedInfo(t, root)
	waitFor(t, "the reaper to give up", func() bool { return strings.Contains(m.runner.heldReason(root, info), "stopped asking") })

	asks := session.closes.Load()
	session.confirmed.Store(true) // the agent has exited since
	job := m.otherAttempt("next", ManagedHarness{Kind: "named", Name: "x"})
	start := time.Now()
	err := m.runner.enterCheckout(job, managedRun{Cwd: root})
	if busyCode(err) != ErrHarnessBusy {
		t.Fatalf("the attempt = %v, want E_HARNESS_BUSY at once", err)
	}
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Fatalf("the attempt took %v; the recheck must not run on the lane", took)
	}
	waitFor(t, "the restarted reaper to release the checkout", func() bool { return m.runner.HarnessBusy(root) == nil })
	if session.closes.Load() <= asks {
		t.Fatal("the provider was not asked again")
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "project rules\n" {
		t.Fatalf("the block was not taken out after the late exit: %q", got)
	}
	var admitted atomic.Bool
	if err := m.runner.enterCheckout(m.otherAttempt("after", ManagedHarness{Kind: "named", Name: "x"}), managedRun{Cwd: root}); err == nil {
		admitted.Store(true)
	}
	if !admitted.Load() {
		t.Fatal("the released checkout was not admitted")
	}
}

func mustResolvedInfo(t *testing.T, dir string) os.FileInfo {
	t.Helper()
	_, info, ok := resolvedCheckout(dir)
	if !ok {
		t.Fatalf("%s does not resolve", dir)
	}
	return info
}

// storedRun is the run as the store holds it now.
func storedRun(t *testing.T, m *managedLifecycle) (managedRun, map[string]any) {
	t.Helper()
	run, raw, err := m.runner.run(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	item, _ := fields["item"].(map[string]any)
	return run, item
}

// A run held for a live process group was recorded unresolved because its first
// Close could not confirm the group gone. When a later Close does, the group is
// what it would have been had the first found it so: the run's outcome is what
// the first Close would have recorded, exited with the agent's code, and it is
// no longer uncertain. Before, only the checkout was released and the run stayed
// unresolved for good.
func TestALaterCloseThatFindsTheGroupGoneRevisesTheUnresolvedRun(t *testing.T) {
	root, session, m := heldRun(t, map[string]string{"CLAUDE.md": "project rules\n"}, func(s *managedTestSession) {
		s.unreaped = false
		s.groupLive = true
		s.liveGroupErr = errors.New("managed provider process group 123 still has a live member after SIGKILL")
	})
	if run, _ := storedRun(t, m); run.State != "unresolved" {
		t.Fatalf("the held run is recorded %s, want unresolved", run.State)
	}
	session.confirmed.Store(true)
	waitFor(t, "the checkout to be released", func() bool { return m.runner.HarnessBusy(root) == nil })
	waitFor(t, "the stored run to be revised", func() bool { run, _ := storedRun(t, m); return run.State == "exited" })
	_, item := storedRun(t, m)
	if item["outcome_uncertain"] != false || item["exit_code"] != float64(0) || item["finished_at"] == nil {
		t.Fatalf("the revised run is not a settled exit with the agent's code: %v", item)
	}
	if reason, _ := item["reason"].(string); !strings.Contains(reason, "confirmed gone") {
		t.Fatalf("the revised run does not say why it changed: %q", reason)
	}
}

// A member alive when the reaper asks keeps the run unresolved, the checkout
// held and the block in place, however many times it asks.
func TestARunWhoseGroupStaysAliveStaysUnresolvedAndHeld(t *testing.T) {
	root, session, m := heldRun(t, map[string]string{"CLAUDE.md": "project rules\n"}, func(s *managedTestSession) {
		s.unreaped = false
		s.groupLive = true
		s.liveGroupErr = errors.New("managed provider process group 123 still has a live member after SIGKILL")
	})
	asked := session.closes.Load()
	waitFor(t, "the reaper to ask again", func() bool { return session.closes.Load() >= asked+3 })
	if run, _ := storedRun(t, m); run.State != "unresolved" {
		t.Fatalf("the run is %s while a member of its group is alive, want unresolved", run.State)
	}
	if e := m.runner.HarnessBusy(root); e == nil || e.Code != ErrHarnessBusy {
		t.Fatalf("HarnessBusy = %v, want the checkout held", e)
	}
	if got := readFile(t, root, "CLAUDE.md"); !strings.Contains(got, harnessBeginMarker) {
		t.Fatalf("the block was taken out under a live group:\n%s", got)
	}
}

// What the first Close would have recorded had the group been gone is what the
// later one records, and no more: a Close that still carries an error (the wait
// error is sticky) leaves the run unresolved, as the first would have.
func TestALaterCloseWithAnErrorLeavesTheRunUnresolved(t *testing.T) {
	root, session, m := heldRun(t, map[string]string{"CLAUDE.md": "project rules\n"}, func(s *managedTestSession) {
		s.unreaped = false
		s.groupLive = true
		s.closeErr = errors.New("exec: WaitDelay expired before I/O complete")
	})
	session.confirmed.Store(true)
	waitFor(t, "the checkout to be released", func() bool { return m.runner.HarnessBusy(root) == nil })
	if run, _ := storedRun(t, m); run.State != "unresolved" {
		t.Fatalf("the run is %s, want unresolved: the first Close would have kept it so", run.State)
	}
}

// Two reaps that both find the group gone (a restarted reaper beside one that was
// already asking, or the same release run twice) end the hold once and write the
// run once; a run already finalised is not rewritten by a late revision.
func TestRevisingAHeldRunIsIdempotentAndNeverRewritesAFinalisedRun(t *testing.T) {
	root, session, m := heldRun(t, map[string]string{"CLAUDE.md": "project rules\n"}, func(s *managedTestSession) {
		s.unreaped = false
		s.groupLive = true
		s.liveGroupErr = errors.New("managed provider process group 123 still has a live member after SIGKILL")
	})
	m.runner.mu.Lock()
	var held *blockedCheckout
	for _, b := range m.runner.blocked {
		if b.session != nil {
			held = b
		}
	}
	m.runner.mu.Unlock()
	if held == nil {
		t.Fatal("no hold was taken")
	}
	session.confirmed.Store(true)
	waitFor(t, "the checkout to be released", func() bool { return m.runner.HarnessBusy(root) == nil })
	waitFor(t, "the stored run to be revised", func() bool { run, _ := storedRun(t, m); return run.State == "exited" })
	revised, _ := storedRun(t, m)

	exit, err := session.Close()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); m.runner.releaseHeld(held, exit, err) }()
	}
	wg.Wait()
	m.runner.reviseHeldRun(held, exit, err)
	if again, _ := storedRun(t, m); again.Revision != revised.Revision || again.State != "exited" {
		t.Fatalf("the run was rewritten after it was finalised: revision %d -> %d, state %s", revised.Revision, again.Revision, again.State)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "project rules\n" {
		t.Fatalf("a repeated release touched the file: %q", got)
	}
}

// plainRun is a run with no harness, ended through its own Close.
func plainRun(t *testing.T, configure ...func(*managedTestSession)) (string, *managedTestSession, *managedLifecycle) {
	t.Helper()
	root := scanRepo(t, map[string]string{"CLAUDE.md": "project rules\n"})
	var session *managedTestSession
	m := unconfirmedLifecycle(t, root, &session, configure...)
	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", ""))
	if err != nil {
		t.Fatal(err)
	}
	activate(t, m, prepared)
	m.finished()
	if session == nil {
		t.Fatal("the factory never built the session")
	}
	return root, session, m
}

func liveGroup(s *managedTestSession) {
	s.unreaped = false
	s.groupLive = true
	s.liveGroupErr = errors.New("managed provider process group 123 still has a live member after SIGKILL")
}

// A plain run (owner decision 2026-11-06, ft-43e7cb92) holds its checkout for as
// long as a member of its agent's process group lives, as a harness run does: the
// group can still write the checkout a second run would be admitted on. The
// refusal says why and names the group; when the group is gone the reaper releases
// the checkout and the run, recorded unresolved until then, is revised.
func TestAPlainRunWhoseGroupOutlivesTheAgentKeepsTheCheckoutHeldUntilItIsGone(t *testing.T) {
	root, session, m := plainRun(t, liveGroup)
	if run, _ := storedRun(t, m); run.State != "unresolved" {
		t.Fatalf("the run is recorded %s, want unresolved while its group lives", run.State)
	}
	e := m.runner.HarnessBusy(root)
	if e == nil || e.Code != ErrHarnessBusy {
		t.Fatalf("HarnessBusy = %v, want the checkout held for a plain run whose group lives", e)
	}
	for _, want := range []string{"managed run run ", "process group", "no harness block was written", "checkout is held", "restart `manvi serve`"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("the hold message lacks %q:\n%s", want, e.Message)
		}
	}
	waitFor(t, "the refusal to name the live group", func() bool {
		e := m.runner.HarnessBusy(root)
		return e != nil && strings.Contains(e.Message, "process group 123 still has a live member")
	})
	// A second managed run, with a harness or without, is refused with the reason.
	_, err := secondHarnessAttempt(m, root)
	if busyCode(err) != ErrHarnessBusy || !strings.Contains(err.Error(), "may still be writing this checkout") {
		t.Errorf("a harness run was admitted on a held checkout, or not told why: %v", err)
	}
	err = m.runner.enterCheckout(m.otherAttempt("plain", ManagedHarness{}), managedRun{Cwd: root})
	if busyCode(err) != ErrHarnessBusy || !strings.Contains(err.Error(), "may still be writing this checkout") {
		t.Errorf("a plain run was admitted on a held checkout, or not told why: %v", err)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "project rules\n" {
		t.Fatalf("a hold with no block touched the instruction file: %q", got)
	}

	session.confirmed.Store(true)
	waitFor(t, "the checkout to be released", func() bool { return m.runner.HarnessBusy(root) == nil })
	waitFor(t, "the stored run to be revised", func() bool { run, _ := storedRun(t, m); return run.State == "exited" })
	if err := m.runner.enterCheckout(m.otherAttempt("after", ManagedHarness{Kind: "named", Name: "x"}), managedRun{Cwd: root}); err != nil {
		t.Fatalf("the released checkout was not admitted: %v", err)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "project rules\n" {
		t.Fatalf("the release touched the instruction file: %q", got)
	}
}

// A plain run whose group is already gone when it closes holds nothing: it is the
// ordinary exit, recorded exited, and the checkout is free at once.
func TestAPlainRunWhoseGroupIsGoneAtCloseReleasesTheCheckoutAtOnce(t *testing.T) {
	root, _, m := plainRun(t, func(s *managedTestSession) { s.unreaped = false })
	if run, _ := storedRun(t, m); run.State != "exited" {
		t.Fatalf("the run is recorded %s, want exited", run.State)
	}
	if e := m.runner.HarnessBusy(root); e != nil {
		t.Fatalf("HarnessBusy = %v for a run whose group was gone at close", e)
	}
	if err := m.runner.enterCheckout(m.otherAttempt("next", ManagedHarness{}), managedRun{Cwd: root}); err != nil {
		t.Fatalf("the checkout of a cleanly ended plain run is held: %v", err)
	}
}

// A hold covers the files the group can write, which is the tree below the
// directory the run started in, and the tree above it that the run's writes reach.
// A hold taken in a subdirectory refuses the worktree root, an unrelated sibling
// of it is admitted, and a hold at the root refuses a subdirectory (the adversarial
// test beside this one).
func TestAHoldInASubdirectoryRefusesTheRootButNotASibling(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "project rules\n"})
	sub, sibling := filepath.Join(root, "pkg", "inner"), filepath.Join(root, "pkg", "other")
	for _, dir := range []string{sub, sibling} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	var session *managedTestSession
	m := unconfirmedLifecycle(t, root, &session, liveGroup)
	m.runner.client = &managedCwd{managedStore: m.runner.client, cwd: sub}
	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", ""))
	if err != nil {
		t.Fatal(err)
	}
	activate(t, m, prepared)
	m.finished()
	if e := m.runner.HarnessBusy(sub); e == nil {
		t.Fatal("the run's own directory is not held, so the scenario did not arise")
	}
	if e := m.runner.HarnessBusy(root); e == nil {
		t.Error("work.verify.task on the worktree root is not refused while a run in a subdirectory of it is held")
	}
	if err := m.runner.enterCheckout(m.otherAttempt("root", ManagedHarness{}), managedRun{Cwd: root}); busyCode(err) != ErrHarnessBusy {
		t.Errorf("a run at the worktree root was admitted while a run in a subdirectory is held: %v", err)
	}
	if e := m.runner.HarnessBusy(sibling); e != nil {
		t.Errorf("an unrelated sibling directory is refused: %v", e)
	}
	if err := m.runner.enterCheckout(m.otherAttempt("sibling", ManagedHarness{}), managedRun{Cwd: sibling}); err != nil {
		t.Errorf("an unrelated sibling directory was not admitted: %v", err)
	}
}

// A revision the store keeps refusing is retried a bounded number of times and
// then logged for the operator, not dropped into a job error nothing reads.
func TestARevisionTheStoreKeepsRefusingIsLoggedAfterBoundedRetries(t *testing.T) {
	prevBackoff := reviseBackoff
	reviseBackoff = time.Millisecond
	t.Cleanup(func() { reviseBackoff = prevBackoff })
	var logged strings.Builder
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logged.Write(p) }))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	root, session, m, fault := advPlainRun(t, 0, liveGroup)
	fault.failFinish.Store(1 << 20)
	before := fault.finishes.Load()
	session.confirmed.Store(true)
	waitFor(t, "the checkout to be released", func() bool { return m.runner.HarnessBusy(root) == nil })
	waitFor(t, "the revision to be logged", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(logged.String(), "revising run run")
	})
	if got := fault.finishes.Load() - before; got != int32(reviseAttempts) {
		t.Fatalf("the revision was attempted %d times, want %d", got, reviseAttempts)
	}
	if run, _ := storedRun(t, m); run.State != "unresolved" {
		t.Fatalf("the run is %s although every write was refused", run.State)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// managedCwd has the store report a different cwd for the run, so a run can be
// started in a subdirectory of the checkout its fixture was prepared in.
type managedCwd struct {
	managedStore
	cwd string
}

func (s *managedCwd) Workbench(ctx context.Context, method string, input json.RawMessage) (json.RawMessage, error) {
	raw, err := s.managedStore.Workbench(ctx, method, input)
	if err != nil || method != "runs.get" {
		return raw, err
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return raw, nil
	}
	if item, ok := doc["item"].(map[string]any); ok {
		item["cwd"] = s.cwd
	}
	return json.Marshal(doc)
}
