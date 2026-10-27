package serve

import (
	"os"
	"path/filepath"
	"strings"
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
	waitFor(t, "the reaper to give up", func() bool { return strings.Contains(m.runner.heldReason(info), "stopped asking") })

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
