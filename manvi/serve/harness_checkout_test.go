package serve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
)

// Store-free tests of what a managed attempt does about its checkout: one live
// harness per checkout, recovery of a dead run's block, and the refusal of
// anything that captures a diff while a block is in the worktree.

func liveJob(checkout string, harnessed bool) *managedJob {
	j := &managedJob{checkout: checkout, wantsHarness: harnessed, done: make(chan struct{})}
	if harnessed {
		j.materialised = &harnessMaterialisation{}
	}
	return j
}

func runnerWith(jobs ...*managedJob) *ManagedRunner {
	r := &ManagedRunner{jobs: map[string]*managedJob{}}
	for i, j := range jobs {
		r.jobs[string(rune('a'+i))] = j
	}
	return r
}

func resolvedDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func busyCode(err error) string {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}

func TestOneLiveHarnessPerCheckout(t *testing.T) {
	dir, other := resolvedDir(t), resolvedDir(t)
	cases := map[string]struct {
		live      *managedJob
		wantsNew  bool
		checkout  string
		wantsCode string
	}{
		"harness while a harness is live":      {liveJob(dir, true), true, dir, ErrHarnessBusy},
		"harness while a plain run is live":    {liveJob(dir, false), true, dir, ErrHarnessBusy},
		"plain run while a harness is live":    {liveJob(dir, true), false, dir, ErrHarnessBusy},
		"plain runs share a checkout":          {liveJob(dir, false), false, dir, ""},
		"another checkout is not held":         {liveJob(other, true), true, dir, ""},
		"a finished harness run holds nothing": {finished(liveJob(dir, true)), true, dir, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := runnerWith(c.live)
			job := &managedJob{done: make(chan struct{})}
			if c.wantsNew {
				job.harnessRef = ManagedHarness{Kind: "named", Name: "x"}
			}
			err := r.enterCheckout(job, managedRun{Cwd: c.checkout})
			if busyCode(err) != c.wantsCode {
				t.Fatalf("enterCheckout = %v, want code %q", err, c.wantsCode)
			}
			if c.wantsCode == "" && (job.checkout != dir || job.wantsHarness != c.wantsNew) {
				t.Fatalf("the attempt did not register on its checkout: %q %v", job.checkout, job.wantsHarness)
			}
			if c.wantsCode != "" && job.checkout != "" {
				t.Fatal("a refused attempt registered on the checkout and would block the next one")
			}
		})
	}
}

func finished(j *managedJob) *managedJob { close(j.done); return j }

// Two attempts arriving together cannot both be admitted: the check and the
// registration are one step.
func TestTwoHarnessAttemptsCannotBothBeAdmitted(t *testing.T) {
	dir := resolvedDir(t)
	r := &ManagedRunner{jobs: map[string]*managedJob{}}
	a := &managedJob{done: make(chan struct{}), harnessRef: ManagedHarness{Kind: "named", Name: "x"}}
	b := &managedJob{done: make(chan struct{}), harnessRef: ManagedHarness{Kind: "named", Name: "x"}}
	r.jobs["a"], r.jobs["b"] = a, b
	errs := make(chan error, 2)
	for _, j := range []*managedJob{a, b} {
		go func() { errs <- r.enterCheckout(j, managedRun{Cwd: dir}) }()
	}
	first, second := <-errs, <-errs
	if (first == nil) == (second == nil) {
		t.Fatalf("both or neither were admitted: %v / %v", first, second)
	}
}

func TestEnterCheckoutTakesOutADeadRunsBlockBeforeAnythingElse(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules"})
	if _, err := materialiseHarness(root, "codex", specWithSections(t, houseRules)); err != nil {
		t.Fatal(err)
	}
	// No run is live: the process that wrote the block is gone. A plain attempt,
	// with no harness, is what finds it.
	r := &ManagedRunner{jobs: map[string]*managedJob{}}
	job := &managedJob{done: make(chan struct{})}
	r.jobs["a"] = job
	if err := r.enterCheckout(job, managedRun{Cwd: root}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(job.recovery, "AGENTS.md") {
		t.Fatalf("recovery was not reported: %q", job.recovery)
	}
	if got := readFile(t, root, "AGENTS.md"); got != "rules" {
		t.Fatalf("the block was not taken out exactly: %q", got)
	}
}

func TestEnterCheckoutRefusesOnceWhenTheDeadRunsBlockWasEdited(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	if _, err := materialiseHarness(root, "codex", specWithSections(t, houseRules)); err != nil {
		t.Fatal(err)
	}
	live := readFile(t, root, "AGENTS.md")
	writeRaw(t, root, "AGENTS.md", strings.Replace(live, "Prefer small diffs.", "edited", 1))
	r := &ManagedRunner{jobs: map[string]*managedJob{}}
	first := &managedJob{done: make(chan struct{})}
	r.jobs["a"] = first
	err := r.enterCheckout(first, managedRun{Cwd: root})
	var typed *harnessMaterialiseError
	if !errors.As(err, &typed) || !strings.Contains(err.Error(), "modified harness block") {
		t.Fatalf("enterCheckout = %v, want the typed modified-block refusal", err)
	}
	second := &managedJob{done: make(chan struct{})}
	r.jobs["b"] = second
	if err := r.enterCheckout(second, managedRun{Cwd: root}); err != nil {
		t.Fatalf("the refusal was made twice, so the checkout is wedged: %v", err)
	}
}

func TestHarnessBusyReportsALiveBlockAndAnUnappliedRecord(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	if e := runnerWith().HarnessBusy(root); e != nil {
		t.Fatalf("a clean worktree is busy: %v", e)
	}
	live := liveJob(root, true)
	if e := runnerWith(live).HarnessBusy(root); e == nil || e.Code != ErrHarnessBusy || !strings.Contains(e.Message, "in progress") {
		t.Fatalf("a live block was not reported: %v", e)
	}
	if e := runnerWith(liveJob(root, false)).HarnessBusy(root); e != nil {
		t.Fatalf("a plain run made the worktree busy: %v", e)
	}
	if e := runnerWith(finished(liveJob(root, true))).HarnessBusy(root); e != nil {
		t.Fatalf("a finished run made the worktree busy: %v", e)
	}
	// A block a dead run left, with no live run: also unsafe to diff.
	if _, err := materialiseHarness(root, "codex", specWithSections(t, houseRules)); err != nil {
		t.Fatal(err)
	}
	if e := runnerWith().HarnessBusy(root); e == nil || e.Code != ErrHarnessBusy || !strings.Contains(e.Message, "left its block") {
		t.Fatalf("an unapplied record was not reported: %v", e)
	}
	if _, err := recoverPendingHarness(root); err != nil {
		t.Fatal(err)
	}
	if e := runnerWith().HarnessBusy(root); e != nil {
		t.Fatalf("a recovered worktree is still busy: %v", e)
	}
}

func busyAlways(string) *Error {
	return harnessBusy("a managed harness run is in progress on this worktree")
}

// work.verify.task refuses while a block is in the worktree, before it reads
// the store or captures any diff.
func TestVerifyRefusesWhileAHarnessBlockIsLive(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	runner, err := NewVerifyRunner(VerifyDeps{
		Client: store.New("/does/not/exist/dcstore", filepath.Join(t.TempDir(), "p.sqlite")), Critic: &scriptedCritic{},
		Devmap: func(string) DevmapClient { return &fakeDevmap{} }, Busy: busyAlways,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	raw, _ := json.Marshal(VerifyTaskParams{RequestID: "r", ItemID: "i", Worktree: root, Base: "HEAD", Head: "HEAD"})
	_, err = runner.Start(context.Background(), raw)
	var refusal *store.WorkbenchError
	if !errors.As(err, &refusal) || refusal.Code != ErrHarnessBusy {
		t.Fatalf("Start = %v, want a %s refusal that never reached the store", err, ErrHarnessBusy)
	}
}

func TestScanRefusesWhileAHarnessBlockIsLive(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	runner, err := NewScanRunner(ScanDeps{
		Devmap: func(string) ScanDevmap { return &scanFakeDevmap{} },
		Text:   func(string) ScanText { return &scanFakeText{} },
		Board:  &scanFakeBoard{}, Busy: busyAlways,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	raw, _ := json.Marshal(GapScanParams{RepoRoot: root})
	if _, e := runner.Start(context.Background(), raw); e == nil || e.Code != ErrHarnessBusy {
		t.Fatalf("Start = %v, want %s", e, ErrHarnessBusy)
	}
}

// The recorded block is in tool state, which verification diffs already leave
// out, and it is gone once the run has ended.
func TestThePendingRecordIsUnderToolStateAndRemovedOnRestore(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	m, err := materialiseHarness(root, "codex", specWithSections(t, houseRules))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(harnessPendingFile, ".devcouncil/") || strings.HasPrefix(harnessPendingFile, ".devcouncil/harness/") {
		t.Fatalf("record path %q is not tool state outside the specs directory", harnessPendingFile)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); err != nil {
		t.Fatalf("no record while the block is live: %v", err)
	}
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the record outlived the restore")
	}
}
