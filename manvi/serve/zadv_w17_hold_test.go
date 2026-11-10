package serve

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// Adversarial tests for lane D (w17): checkoutsOverlap narrowed by hasGitBoundary.
// The hold exists so that a check never judges a change the agent did not make. A
// boundary is therefore only honest if the agent running in a checkout cannot make
// one. Tests named ...BREAK... fail on 0e2946a: they show the narrowing trusting a
// marker (.git) that the agent controls.

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...) // #nosec G204 -- the binary is the literal "git"; the arguments are this test's own.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_EDITOR=true", "EDITOR=true", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// trackedRepo is a real repository whose index tracks files under sub and
// sub/deeper: the files one working tree owns. It returns the resolved root, sub
// and sub/deeper.
func trackedRepo(t *testing.T) (root, sub, deeper string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root = resolvedDir(t)
	sub = filepath.Join(root, "sub")
	deeper = filepath.Join(sub, "deeper")
	if err := os.MkdirAll(deeper, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(sub, "file.txt"), filepath.Join(deeper, "f.go")} {
		if err := os.WriteFile(f, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitOutput(t, root, "init", "-b", "main")
	gitOutput(t, root, "add", "-A")
	gitOutput(t, root, "commit", "-m", "init")
	return root, sub, deeper
}

// requireIndexStillTracks proves the planted marker did not make sub a separate
// tree: the outer repository's own index still lists the files under it.
func requireIndexStillTracks(t *testing.T, root string) {
	t.Helper()
	listed := gitOutput(t, root, "ls-files")
	for _, want := range []string{"sub/file.txt", "sub/deeper/f.go"} {
		if !strings.Contains(listed, want) {
			t.Fatalf("the outer index does not track %s after the plant:\n%s", want, listed)
		}
	}
}

// plantsPortable are the bogus .git entries an agent can write with plain file
// operations, one directory at a time.
var plantsPortable = map[string]func(t *testing.T, dir string){
	"empty file": func(t *testing.T, dir string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, ".git"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	},
	"garbage file": func(t *testing.T, dir string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("\x00\xff not a gitfile \n"), 0o600); err != nil {
			t.Fatal(err)
		}
	},
	"empty directory": func(t *testing.T, dir string) {
		t.Helper()
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o750); err != nil {
			t.Fatal(err)
		}
	},
	"gitdir pointing nowhere": func(t *testing.T, dir string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /nonexistent/nowhere\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	},
}

// BREAK (fails on 0e2946a): hasGitBoundary accepts any .git entry, so an agent in a
// checkout can plant one in a subdirectory the outer repository still tracks, and
// the root and that subdirectory stop holding each other.
func TestZadvW17BreakPlantedGitMarkerSplitsTrackedSubdirectory(t *testing.T) {
	for name, plant := range plantsPortable {
		t.Run(name, func(t *testing.T) {
			root, sub, deeper := trackedRepo(t)
			plant(t, sub)
			requireIndexStillTracks(t, root)
			if !overlap(t, root, sub) {
				t.Errorf("a %s planted in a tracked subdirectory made it a separate tree from its repository", name)
			}
			if !overlap(t, root, deeper) {
				t.Errorf("a %s planted in sub made sub/deeper a separate tree from the repository", name)
			}
		})
	}
}

// BREAK (fails on 0e2946a): the marker may be planted at any depth between the two.
func TestZadvW17BreakPlantedGitMarkerAtAnIntermediateDirectory(t *testing.T) {
	root, sub, deeper := trackedRepo(t)
	plantsPortable["empty file"](t, sub)
	requireIndexStillTracks(t, root)
	if !overlap(t, sub, deeper) {
		t.Error("sub still overlaps deeper, the plant is above it")
	}
	if !overlap(t, root, deeper) {
		t.Error("a marker planted in an intermediate directory splits the root from a tracked directory below it")
	}
}

// BREAK (fails on 0e2946a): the hold is for a straggler that may still write the
// checkout, and a straggler can plant the marker itself. A hold on the root then no
// longer refuses a run in a subdirectory, nor verification of it.
func TestZadvW17BreakStragglerPlantsMarkerUnderAHeldRoot(t *testing.T) {
	root, sub, _ := trackedRepo(t)
	path, info := checkoutOf(t, root)
	r := &ManagedRunner{jobs: map[string]*managedJob{}}
	r.blocked = append(r.blocked, &blockedCheckout{info: info, root: path, session: heldSession{}, runID: "run-1"})
	if busy := r.HarnessBusy(sub); busy == nil {
		t.Fatal("precondition: the hold on the root already does not refuse its subdirectory")
	}
	plantsPortable["empty file"](t, sub)
	requireIndexStillTracks(t, root)
	if busy := r.HarnessBusy(sub); busy == nil {
		t.Error("HarnessBusy admitted a tracked subdirectory of a held root after a straggler planted .git in it")
	}
	subPath, subInfo := checkoutOf(t, sub)
	if reason := r.heldReason(subPath, subInfo); reason == "" {
		t.Error("heldReason no longer names the hold on the root for its tracked subdirectory")
	}
	job := &managedJob{done: make(chan struct{})}
	if err := r.enterCheckout(job, managedRun{Cwd: sub}); err == nil {
		t.Error("enterCheckout admitted a run in a tracked subdirectory of a held root after the plant")
	}
}

// BREAK (fails on 0e2946a): the reverse. A hold in the subdirectory, a marker
// planted in it, then verification of the root is judged while the straggler can
// still write files the root's index tracks.
func TestZadvW17BreakStragglerPlantsMarkerUnderAHeldSubdirectory(t *testing.T) {
	root, sub, _ := trackedRepo(t)
	path, info := checkoutOf(t, sub)
	r := &ManagedRunner{jobs: map[string]*managedJob{}}
	r.blocked = append(r.blocked, &blockedCheckout{info: info, root: path, session: heldSession{}, runID: "run-1"})
	plantsPortable["garbage file"](t, sub)
	requireIndexStillTracks(t, root)
	if busy := r.HarnessBusy(root); busy == nil {
		t.Error("HarnessBusy admitted verification of the root while a held straggler can write a tracked subdirectory")
	}
}

// BREAK (fails on 0e2946a): the same for a live harness run: the run in sub plants
// the marker and the root's harness check no longer sees it.
func TestZadvW17BreakLiveRunPlantsMarker(t *testing.T) {
	root, sub, _ := trackedRepo(t)
	r := runnerWith(liveJob(sub, true))
	if busy := r.HarnessBusy(root); busy == nil {
		t.Fatal("precondition: a live harness run in sub is not seen from the root")
	}
	plantsPortable["empty directory"](t, sub)
	requireIndexStillTracks(t, root)
	if busy := r.HarnessBusy(root); busy == nil {
		t.Error("a live harness run in a tracked subdirectory no longer holds the root once it plants .git")
	}
}

// HOLDS: a marker that comes and goes between checks is read each time; nothing is
// cached, so a lane's .git that disappears makes it a subdirectory again.
func TestZadvW17GitMarkerIsReadAtEveryCheck(t *testing.T) {
	root, lane, _ := nestedRepo(t)
	path, info := checkoutOf(t, root)
	r := &ManagedRunner{jobs: map[string]*managedJob{}}
	r.blocked = append(r.blocked, &blockedCheckout{info: info, root: path, session: heldSession{}, runID: "run-1"})
	if busy := r.HarnessBusy(lane); busy != nil {
		t.Fatalf("a hold on the root refused its linked worktree: %v", busy)
	}
	gitFile := filepath.Join(lane, ".git")
	moved := gitFile + ".away"
	if err := os.Rename(gitFile, moved); err != nil {
		t.Fatal(err)
	}
	if busy := r.HarnessBusy(lane); busy == nil {
		t.Error("with its .git gone the lane is a plain subdirectory of the held root and must be held")
	}
	if err := os.Rename(moved, gitFile); err != nil {
		t.Fatal(err)
	}
	if busy := r.HarnessBusy(lane); busy != nil {
		t.Errorf("with its .git back the lane is a separate tree again: %v", busy)
	}
}

// HOLDS: recheckHeld restarts a reaper only for a hold the attempt's checkout
// overlaps; a nested lane does not touch the root's gave-up hold, and the hold's
// own tree and a plain subdirectory of it do.
func TestZadvW17RecheckHeldFollowsTheBoundary(t *testing.T) {
	root, lane, _ := nestedRepo(t)
	path, info := checkoutOf(t, root)
	sub := filepath.Join(root, "plain")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	// attempts is zero: a restarted reaper makes no provider call and gives up at once.
	b := &blockedCheckout{info: info, root: path, session: heldSession{}, runID: "run-1", gaveUp: true}
	r := &ManagedRunner{jobs: map[string]*managedJob{}, blocked: []*blockedCheckout{b}, reapStop: make(chan struct{})}
	rechecks := func() int {
		r.mu.Lock()
		defer r.mu.Unlock()
		return b.rechecks
	}
	lanePath, laneInfo := checkoutOf(t, lane)
	r.recheckHeld(lanePath, laneInfo)
	if got := rechecks(); got != 0 {
		t.Fatalf("an attempt in a linked worktree restarted the root's reaper (%d)", got)
	}
	subPath, subInfo := checkoutOf(t, sub)
	r.recheckHeld(subPath, subInfo)
	if got := rechecks(); got != 1 {
		t.Fatalf("an attempt in a plain subdirectory did not restart the hold's reaper (%d)", got)
	}
	// The reaper goroutine that restart started gives up at once (no attempts); let it settle
	// before the test ends by waiting for gaveUp, which it sets under mu.
	deadline := time.After(30 * time.Second)
	for {
		r.mu.Lock()
		gave := b.gaveUp
		r.mu.Unlock()
		if gave {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the restarted reaper never finished")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// HOLDS: a case variant of the outer checkout, with a real repository's own .git and
// a .git in the parent of the outer (a home directory kept in git), is not read as
// a boundary, in both orders. Skipped on a case-sensitive volume.
func TestZadvW17CaseVariantOuterIsNotSteppedPast(t *testing.T) {
	base := resolvedDir(t)
	if err := os.Mkdir(filepath.Join(base, ".git"), 0o750); err != nil { // the parent's own repository
		t.Fatal(err)
	}
	root := filepath.Join(base, "Repo")
	pkg := filepath.Join(root, "pkg", "sub")
	if err := os.MkdirAll(pkg, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "REPO")); err != nil {
		t.Skip("volume is case-sensitive")
	}
	variantPkg := filepath.Join(base, "REPO", "pkg", "sub")
	if !overlap(t, root, variantPkg) {
		t.Error("a case-variant spelling of a subdirectory does not overlap its repository")
	}
	if !overlap(t, filepath.Join(base, "rEpO"), pkg) {
		t.Error("a case-variant spelling of the repository does not overlap its subdirectory")
	}
	// A plain outer (no .git of its own) under a parent that has one: the walk must
	// stop at the outer, not read the parent's .git as a boundary.
	plain := filepath.Join(base, "Plain")
	plainPkg := filepath.Join(plain, "pkg")
	if err := os.MkdirAll(plainPkg, 0o750); err != nil {
		t.Fatal(err)
	}
	if !overlap(t, plain, plainPkg) {
		t.Error("a parent's .git was read as a boundary below its child")
	}
	if !overlap(t, plain, filepath.Join(base, "PLAIN", "pkg")) {
		t.Error("a parent's .git was read as a boundary below a case-variant spelling of its child")
	}
}

// HOLDS: a real linked worktree reached through case-variant spellings of the
// container, of an intermediate directory and of the lane itself is still a
// separate tree, and a .GIT spelled in capitals is the marker git reads on a
// case-insensitive volume.
func TestZadvW17CaseVariantLinkedWorktreeStaysSeparate(t *testing.T) {
	root, lane, _ := nestedRepo(t)
	base := filepath.Dir(root)
	// Probe the volume with a name that has letters: root's own basename may
	// have none (Linux t.TempDir() ends in "001"), and then its upper-case
	// spelling is the same path and proves nothing. root always holds a real
	// .git directory, so .GIT resolves exactly when the volume folds case.
	if _, err := os.Stat(filepath.Join(root, ".GIT")); err != nil {
		t.Skip("volume is case-sensitive")
	}
	variantRoot := filepath.Join(base, strings.ToUpper(filepath.Base(root)))
	variantLane := filepath.Join(variantRoot, ".CLAUDE", "Worktrees", "LANE")
	if overlap(t, root, variantLane) {
		t.Error("a case-variant spelling of a linked worktree overlaps its container")
	}
	if overlap(t, variantRoot, lane) {
		t.Error("a linked worktree overlaps a case-variant spelling of its container")
	}
	if err := os.Rename(filepath.Join(lane, ".git"), filepath.Join(lane, ".GIT")); err != nil {
		t.Fatal(err)
	}
	if overlap(t, root, lane) {
		t.Error("a lane whose marker is spelled .GIT is not a boundary on a case-insensitive volume")
	}
}

// HOLDS: a .git in the directory above the outer checkout never separates a
// checkout from its subdirectory (no case variant involved).
func TestZadvW17ParentsGitIsNotABoundaryBelowIt(t *testing.T) {
	base := resolvedDir(t)
	if err := os.Mkdir(filepath.Join(base, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(base, "plain")
	pkg := filepath.Join(plain, "pkg")
	if err := os.MkdirAll(pkg, 0o750); err != nil {
		t.Fatal(err)
	}
	if !overlap(t, plain, pkg) {
		t.Error("the parent's .git split a checkout from its subdirectory")
	}
}

// HOLDS: one deadline covers every job, and a job that never finishes costs
// Close its bound once, not once per job.
func TestZadvW17CloseOneDeadlineForManyHangingJobs(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	cancelled := map[string]bool{}
	hang := func(id string) *managedJob {
		return &managedJob{id: id, done: make(chan struct{}), cancel: func() { mu.Lock(); cancelled[id] = true; mu.Unlock() }}
	}
	r := &ManagedRunner{jobs: map[string]*managedJob{"a": hang("a"), "b": hang("b"), "c": hang("c")}}
	start := time.Now()
	err := r.Close()
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("Close = %v, want the typed unresolved refusal", err)
	}
	if floor := codingagent.CloseBudget(); took < floor {
		t.Errorf("Close gave up after %v, before codingagent's close budget %v", took, floor)
	}
	// Three jobs each given the whole wait would take three times as long; the
	// slack is a bound on scheduling, not an assertion that anything stayed quiet.
	if ceiling := managedCloseWait() + 3*time.Second; took > ceiling {
		t.Errorf("Close took %v for three hanging jobs, want one deadline of %v", took, managedCloseWait())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{"a", "b", "c"} {
		if !cancelled[id] {
			t.Errorf("job %s was not cancelled", id)
		}
	}
}

// HOLDS: jobs that finish when cancelled, and one that finishes late but inside the
// budget, let Close return nil; only the budget, never the order of the jobs, sets
// the wait. A late job is released by a timer started when Close cancels it.
func TestZadvW17CloseWaitsForALateJobInsideTheBudget(t *testing.T) {
	t.Parallel()
	prompt := func() *managedJob {
		j := &managedJob{done: make(chan struct{})}
		j.cancel = func() { close(j.done) }
		return j
	}
	late := &managedJob{id: "late", done: make(chan struct{})}
	late.cancel = func() {
		time.AfterFunc(codingagent.CloseBudget()+time.Second, func() { close(late.done) })
	}
	r := &ManagedRunner{jobs: map[string]*managedJob{"a": prompt(), "b": late, "c": prompt()}}
	if err := r.Close(); err != nil {
		t.Fatalf("Close = %v, want nil: the late job finished inside CloseBudget+margin", err)
	}
	select {
	case <-late.done:
	default:
		t.Fatal("Close returned nil before the late job was done")
	}
}

// HOLDS: Close with no jobs, and a second Close, return at once.
func TestZadvW17CloseIsIdempotentAndEmptyIsImmediate(t *testing.T) {
	t.Parallel()
	r := &ManagedRunner{jobs: map[string]*managedJob{}}
	if err := r.Close(); err != nil {
		t.Fatalf("Close on an empty runner = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	j := &managedJob{id: "x", done: make(chan struct{}), cancel: func() { cancel() }}
	r = &ManagedRunner{jobs: map[string]*managedJob{"x": j}}
	go func() { <-ctx.Done(); close(j.done) }()
	if err := r.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}
