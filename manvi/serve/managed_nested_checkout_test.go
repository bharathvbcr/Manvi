package serve

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A checkout holds the files its own working tree owns. GitPulse puts linked
// worktrees inside the repository (<repo>/.claude/worktrees/<lane>,
// .gitpulse/worktrees/<x>), so a hold or live run on the root must not refuse the
// lane, nor the lane the root: they are two working trees. A subdirectory of a
// tree, or a plain directory nested in another, is still the same files.

func gitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...) // #nosec G204 -- the binary is the literal "git"; the arguments are this test's own.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_EDITOR=true", "EDITOR=true", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// nestedRepo makes a real repository with linked worktrees under the two places
// GitPulse puts them, and returns the resolved root and the two lanes.
func nestedRepo(t *testing.T) (root, claudeLane, pulseLane string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root = resolvedDir(t)
	gitFixture(t, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, root, "add", "README")
	gitFixture(t, root, "commit", "-m", "init")
	claudeLane = filepath.Join(root, ".claude", "worktrees", "lane")
	pulseLane = filepath.Join(root, ".gitpulse", "worktrees", "x")
	gitFixture(t, root, "worktree", "add", "-b", "lane", claudeLane)
	gitFixture(t, root, "worktree", "add", "-b", "x", pulseLane)
	return root, claudeLane, pulseLane
}

func checkoutOf(t *testing.T, dir string) (string, os.FileInfo) {
	t.Helper()
	path, info, ok := resolvedCheckout(dir)
	if !ok {
		t.Fatalf("%s does not resolve", dir)
	}
	return path, info
}

func overlap(t *testing.T, a, b string) bool {
	t.Helper()
	aPath, aInfo := checkoutOf(t, a)
	bPath, bInfo := checkoutOf(t, b)
	ab, ba := checkoutsOverlap(aInfo, aPath, bInfo, bPath), checkoutsOverlap(bInfo, bPath, aInfo, aPath)
	if ab != ba {
		t.Fatalf("overlap is not symmetric for %s and %s: %v / %v", a, b, ab, ba)
	}
	return ab
}

func TestLinkedWorktreeNestedInARepositoryDoesNotOverlapIt(t *testing.T) {
	root, claudeLane, pulseLane := nestedRepo(t)
	for name, lane := range map[string]string{".claude/worktrees": claudeLane, ".gitpulse/worktrees": pulseLane} {
		if overlap(t, root, lane) {
			t.Errorf("the root and the linked worktree under %s are one checkout", name)
		}
		// A subdirectory of the lane belongs to the lane, not to the root.
		sub := filepath.Join(lane, "pkg", "sub")
		if err := os.MkdirAll(sub, 0o750); err != nil {
			t.Fatal(err)
		}
		if overlap(t, root, sub) {
			t.Errorf("a subdirectory of the lane under %s overlaps the root", name)
		}
		if !overlap(t, lane, sub) {
			t.Errorf("a subdirectory of the lane under %s does not overlap its lane", name)
		}
	}
	if overlap(t, claudeLane, pulseLane) {
		t.Error("two sibling lanes overlap")
	}
}

func TestLiveRunOnTheRootDoesNotRefuseANestedLaneAndViceVersa(t *testing.T) {
	root, lane, _ := nestedRepo(t)
	for name, c := range map[string]struct{ live, other string }{"root live, lane asks": {root, lane}, "lane live, root asks": {lane, root}} {
		t.Run(name, func(t *testing.T) {
			r := runnerWith(liveJob(c.live, true))
			if busy := r.HarnessBusy(c.other); busy != nil {
				t.Errorf("HarnessBusy refused a different working tree: %v", busy)
			}
			job := &managedJob{done: make(chan struct{}), harnessRef: ManagedHarness{Kind: "named", Name: "x"}}
			if err := r.enterCheckout(job, managedRun{Cwd: c.other}); err != nil {
				t.Errorf("enterCheckout refused a different working tree: %v", err)
			}
			// The live tree itself is still held.
			if busy := r.HarnessBusy(c.live); busy == nil {
				t.Error("HarnessBusy no longer refuses the working tree with the live harness")
			}
		})
	}
}

// heldSession is a hold's session: the checks below ask the hold, not the provider.
type heldSession struct{ ManagedSession }

func TestHoldOnTheRootDoesNotRefuseANestedLaneAndViceVersa(t *testing.T) {
	root, lane, _ := nestedRepo(t)
	for name, c := range map[string]struct{ held, other string }{"root held, lane asks": {root, lane}, "lane held, root asks": {lane, root}} {
		t.Run(name, func(t *testing.T) {
			path, info := checkoutOf(t, c.held)
			r := &ManagedRunner{jobs: map[string]*managedJob{}}
			r.blocked = append(r.blocked, &blockedCheckout{info: info, root: path, session: heldSession{}, runID: "run-1"})
			if busy := r.HarnessBusy(c.other); busy != nil {
				t.Errorf("a hold on %s refused work on %s: %v", c.held, c.other, busy)
			}
			job := &managedJob{done: make(chan struct{})}
			if err := r.enterCheckout(job, managedRun{Cwd: c.other}); err != nil {
				t.Errorf("enterCheckout refused a different working tree under a hold: %v", err)
			}
			if busy := r.HarnessBusy(c.held); busy == nil {
				t.Error("the held working tree is not held")
			}
			if reason := r.heldReason(path, info); reason == "" {
				t.Error("heldReason does not name the hold on its own tree")
			}
		})
	}
}

// A submodule keeps a .git file, like a linked worktree: it is a repository of
// its own, and a run in it is not a run in its superproject's tree.
func TestSubmoduleStyleGitFileSeparatesCheckouts(t *testing.T) {
	root := resolvedDir(t)
	sub := filepath.Join(root, "vendor", "lib")
	deep := filepath.Join(sub, "src")
	if err := os.MkdirAll(deep, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: ../../.git/modules/lib\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if overlap(t, root, sub) {
		t.Error("a checkout with a .git file overlaps its container")
	}
	if overlap(t, root, deep) {
		t.Error("a subdirectory of a nested repository overlaps the outer directory")
	}
	if !overlap(t, sub, deep) {
		t.Error("a subdirectory does not overlap the repository it is in")
	}
}

func TestNestedPlainDirectoriesAndSubdirectoriesStillOverlap(t *testing.T) {
	root := resolvedDir(t)
	pkg := filepath.Join(root, "packages", "a")
	if err := os.MkdirAll(pkg, 0o750); err != nil {
		t.Fatal(err)
	}
	if !overlap(t, root, pkg) {
		t.Error("a plain directory nested in another does not overlap it")
	}
	// The outer checkout's own .git is not a boundary between it and its subdirectory.
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if !overlap(t, root, pkg) {
		t.Error("a subdirectory of a repository does not overlap its repository")
	}
	if !overlap(t, root, root) {
		t.Error("a checkout does not overlap itself")
	}
	other := resolvedDir(t)
	if overlap(t, root, other) {
		t.Error("two unrelated directories overlap")
	}
}
