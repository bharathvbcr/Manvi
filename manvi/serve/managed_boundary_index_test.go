package serve

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A genuine submodule is a gitlink entry in the superproject's index, not a tracked
// file under it, so it is still a separate tree.
func TestRealSubmoduleIsABoundaryAndItsGitlinkIsNotATrackedFile(t *testing.T) {
	inner := resolvedDir(t)
	gitFixture(t, inner, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(inner, "f"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, inner, "add", "f")
	gitFixture(t, inner, "commit", "-m", "init")
	root := resolvedDir(t)
	gitFixture(t, root, "init", "-b", "main")
	gitFixture(t, root, "-c", "protocol.file.allow=always", "submodule", "add", inner, "vendor/lib")
	gitFixture(t, root, "commit", "-m", "add submodule")
	sub := filepath.Join(root, "vendor", "lib")
	if overlap(t, root, sub) {
		t.Error("a real submodule overlaps its superproject")
	}
	deep := filepath.Join(sub, "deep")
	if err := os.Mkdir(deep, 0o750); err != nil {
		t.Fatal(err)
	}
	if overlap(t, root, deep) {
		t.Error("a directory inside a real submodule overlaps its superproject")
	}
}

// A marker the outer index cannot be asked about is no boundary: with git
// unavailable, a genuine lane and a forged marker are both overlap.
func TestBoundaryFailsClosedWhenGitCannotBeRun(t *testing.T) {
	root, lane, _ := nestedRepo(t)
	if overlap(t, root, lane) {
		t.Fatal("precondition: a linked worktree is a separate tree")
	}
	t.Setenv("PATH", t.TempDir())
	if !overlap(t, root, lane) {
		t.Error("a marker that could not be checked against the outer index separated the trees")
	}
}

// With no repository to consult, a well-formed marker alone separates, and a
// malformed one never does.
func TestBoundaryInAPlainOuterNeedsAWellFormedMarker(t *testing.T) {
	root := resolvedDir(t)
	forged := filepath.Join(root, "forged")
	if err := os.Mkdir(forged, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(forged, ".git"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !overlap(t, root, forged) {
		t.Error("an empty .git file separated a directory of a plain outer")
	}
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if overlap(t, root, real) {
		t.Error("a repository in a plain outer did not separate it")
	}
}

// HarnessBusy on a root whose live run sits in a directory with no marker (the
// fast path: Lstat only) and in a genuine nested linked worktree (a marker, so one
// git process asks the root's index).
func BenchmarkHarnessBusyBoundary(b *testing.B) {
	if _, err := exec.LookPath("git"); err != nil {
		b.Skip("git is not installed")
	}
	root, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	gitFixture(b, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("x\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	gitFixture(b, root, "add", "README")
	gitFixture(b, root, "commit", "-m", "init")
	plain := filepath.Join(root, "pkg")
	if err := os.Mkdir(plain, 0o750); err != nil {
		b.Fatal(err)
	}
	lane := filepath.Join(root, ".claude", "worktrees", "lane")
	gitFixture(b, root, "worktree", "add", "-b", "lane", lane)
	for name, live := range map[string]string{"no marker": plain, "linked worktree marker": lane} {
		r := runnerWith(liveJob(live, true))
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				_ = r.HarnessBusy(root)
			}
		})
	}
}

// A forged marker at D does not separate D when the outer index tracks files under
// it, even when a gitlink sorts before them: only a gitlink at D itself is exempt.
func TestPlantedMarkerWithAGitlinkBesideTrackedFilesIsStillOverlap(t *testing.T) {
	root := resolvedDir(t)
	gitFixture(t, root, "init", "-b", "main")
	d := filepath.Join(root, "D")
	if err := os.MkdirAll(d, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "file.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, root, "add", "D/file.txt")
	// A gitlink under D that sorts before file.txt.
	gitFixture(t, root, "update-index", "--add", "--cacheinfo", "160000,1111111111111111111111111111111111111111,D/0sub")
	gitFixture(t, root, "commit", "-m", "tracked files and a gitlink under D")
	if err := os.MkdirAll(filepath.Join(d, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !overlap(t, root, d) {
		t.Error("a planted marker at a directory whose files the outer index tracks separated it, through a gitlink beside them")
	}
}
