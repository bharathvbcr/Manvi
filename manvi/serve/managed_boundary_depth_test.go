package serve

import (
	"os"
	"path/filepath"
	"testing"
)

// The path hasGitBoundary asks the outer index about is the path of the marker's
// directory below the outer checkout, from the outer down, at every depth. These
// tests put the marker two and three directories down, where a path built from
// the wrong end of the walk names a different directory.

func writeTracked(t *testing.T, root, rel string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, root, "add", rel)
}

func plantMarker(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func trackedRoot(t *testing.T, files ...string) string {
	t.Helper()
	root := resolvedDir(t)
	gitFixture(t, root, "init", "-b", "main")
	for _, f := range files {
		writeTracked(t, root, f)
	}
	gitFixture(t, root, "commit", "-m", "init")
	return root
}

func TestForgedMarkerAtDepthTwoIsOverlap(t *testing.T) {
	root := trackedRoot(t, "pkg/sub/file.go")
	sub := filepath.Join(root, "pkg", "sub")
	plantMarker(t, sub)
	if !overlap(t, root, sub) {
		t.Error("a marker planted two directories down, over files the outer tracks, separated the trees")
	}
}

func TestForgedMarkerAtDepthThreeIsOverlap(t *testing.T) {
	root := trackedRoot(t, "a/b/c/file.go")
	c := filepath.Join(root, "a", "b", "c")
	plantMarker(t, c)
	if !overlap(t, root, c) {
		t.Error("a marker planted three directories down, over files the outer tracks, separated the trees")
	}
	// The same marker is judged from a deeper inner, too.
	deep := filepath.Join(c, "d")
	if err := os.Mkdir(deep, 0o750); err != nil {
		t.Fatal(err)
	}
	if !overlap(t, root, deep) {
		t.Error("a directory under a forged depth-three marker was separated")
	}
}

// A real submodule two directories down is exempt only through its own gitlink
// entry. The root also tracks a top-level lib/ and a sibling vendor/other.txt: a
// query for "lib" (the basename alone) would find lib/x.go and read the submodule
// as overlap, so this passes only when the query is "vendor/lib".
func TestRealSubmoduleAtDepthTwoIsABoundaryThroughItsGitlink(t *testing.T) {
	inner := resolvedDir(t)
	gitFixture(t, inner, "init", "-b", "main")
	writeTracked(t, inner, "f")
	gitFixture(t, inner, "commit", "-m", "init")
	root := resolvedDir(t)
	gitFixture(t, root, "init", "-b", "main")
	writeTracked(t, root, "lib/x.go")
	writeTracked(t, root, "vendor/other.txt")
	gitFixture(t, root, "commit", "-m", "base")
	gitFixture(t, root, "-c", "protocol.file.allow=always", "submodule", "add", inner, "vendor/lib")
	gitFixture(t, root, "commit", "-m", "add submodule")
	sub := filepath.Join(root, "vendor", "lib")
	if !indexTracksUnder(root, "lib") {
		t.Fatal("precondition: the root tracks a file under lib")
	}
	if indexTracksUnder(root, "vendor/lib") {
		t.Fatal("precondition: vendor/lib is a gitlink and not a tracked file")
	}
	if overlap(t, root, sub) {
		t.Error("a real submodule two directories down overlaps its superproject")
	}
}

// A genuine linked worktree three directories down (the GitPulse layout) is a
// boundary.
func TestGenuineLaneAtDepthThreeIsABoundary(t *testing.T) {
	root := trackedRoot(t, "README")
	lane := filepath.Join(root, ".claude", "worktrees", "lane")
	gitFixture(t, root, "worktree", "add", "-b", "lane", lane)
	if overlap(t, root, lane) {
		t.Error("a genuine linked worktree at .claude/worktrees/lane overlaps its repository")
	}
}

// The outer tracks a top-level path with the lane's own basename. Only the full
// path from the outer, wt/lane, is asked about, and the outer tracks nothing there.
func TestGenuineLaneIsABoundaryWhenTheOuterTracksItsBasenameAtTheTop(t *testing.T) {
	root := trackedRoot(t, "lane/f")
	lane := filepath.Join(root, "wt", "lane")
	gitFixture(t, root, "worktree", "add", "-b", "lane", lane)
	if !indexTracksUnder(root, "lane") {
		t.Fatal("precondition: the root tracks a file under lane")
	}
	if overlap(t, root, lane) {
		t.Error("a genuine lane was read as overlap because the outer tracks a top-level path with its basename")
	}
}
