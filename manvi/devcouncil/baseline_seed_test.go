package devcouncil

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The baseline's scratch index is seeded from the repository's own so that
// `add -A` reuses git's stat cache instead of hashing the whole tree every
// turn. What must not change is the answer: the same tree id the cold route
// (read-tree HEAD, then add -A) gives, on every shape of working tree.

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// messyRepo leaves root with every state a baseline has to flatten: a staged
// edit, an unstaged edit, a staged-then-deleted file, a deletion, an
// executable, a symlink, untracked and ignored files.
func messyRepo(t *testing.T) string {
	t.Helper()
	root := benchRepo(t, 30, 0, 0)
	write(t, root, ".gitignore", "ignored/\n*.log\n")
	write(t, root, "ignored/skip.txt", "never in the tree\n")
	write(t, root, "debug.log", "ignored\n")
	gitIn(t, root, "add", ".gitignore")
	gitIn(t, root, "commit", "-qm", "ignore rules")

	write(t, root, "pkg1/file1.go", "package p\n// staged edit\n")
	gitIn(t, root, "add", "pkg1/file1.go")
	write(t, root, "pkg2/file2.go", "package p\n// unstaged edit\n")
	write(t, root, "pkg3/file3.go", "package p\n// staged then removed\n")
	gitIn(t, root, "add", "pkg3/file3.go")
	if err := os.Remove(filepath.Join(root, "pkg3/file3.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "pkg4/file4.go")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "run.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pkg1/file1.go", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "fresh/new file.go", "package fresh\n")
	return root
}

func TestSeededBaselineGivesTheColdRoutesTreeID(t *testing.T) {
	root := messyRepo(t)
	r := benchRegistry(root)
	ctx := context.Background()
	cold, err := r.worktreeTree(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := r.worktreeTree(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if seeded != cold {
		t.Fatalf("seeded tree %s differs from the cold tree %s", seeded, cold)
	}
	// The tree is the working tree: the staged-then-deleted file is not in it.
	if listing := gitIn(t, root, "ls-tree", "-r", "--name-only", seeded); bytes.Contains([]byte(listing), []byte("file3.go")) ||
		!bytes.Contains([]byte(listing), []byte("fresh/new file.go")) {
		t.Fatalf("the baseline is not the working tree:\n%s", listing)
	}
}

func TestSeededBaselineLeavesTheRealIndexUntouched(t *testing.T) {
	root := messyRepo(t)
	indexPath := filepath.Join(root, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	status := gitIn(t, root, "status", "--porcelain")
	if baseline := benchRegistry(root).CaptureBaseline(context.Background()); !baseline.Captured() {
		t.Fatalf("no baseline: %s", baseline.Note)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("capturing the baseline rewrote the repository's own index")
	}
	if got := gitIn(t, root, "status", "--porcelain"); got != status {
		t.Fatalf("status changed:\nbefore %q\nafter  %q", status, got)
	}
}

func TestSeedScratchIndexCopiesTheRepositoryIndex(t *testing.T) {
	root := messyRepo(t)
	dst := filepath.Join(t.TempDir(), "index")
	if !seedScratchIndex(context.Background(), root, dst) {
		t.Fatal("the seed was not taken")
	}
	want, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the scratch index is not a copy of the real one (err %v)", err)
	}
}

// In a linked worktree the index is not at .git/index; a guessed path would
// seed from nothing or from another checkout.
func TestSeedScratchIndexFollowsALinkedWorktree(t *testing.T) {
	root := messyRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	gitIn(t, root, "worktree", "add", "-q", "-b", "side", linked)
	write(t, linked, "pkg1/file1.go", "package p\n// edited in the linked tree\n")
	gitIn(t, linked, "add", "pkg1/file1.go")

	r := benchRegistry(linked)
	dst := filepath.Join(t.TempDir(), "index")
	if !seedScratchIndex(context.Background(), linked, dst) {
		t.Fatal("the seed was not taken in a linked worktree")
	}
	out := gitIn(t, linked, "rev-parse", "--git-path", "index")
	indexPath := trimNL(out)
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(linked, indexPath)
	}
	want, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, want) {
		t.Fatal("seeded from a different index than the linked worktree's own")
	}
	cold, err := r.worktreeTree(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if seeded, err := r.worktreeTree(context.Background(), true); err != nil || seeded != cold {
		t.Fatalf("linked worktree: seeded %s (err %v), cold %s", seeded, err, cold)
	}
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// A seed that git refuses must cost the turn nothing: the baseline falls back
// to the cold route and still answers.
func TestSeededBaselineFallsBackWhenTheRealIndexIsCorrupt(t *testing.T) {
	root := messyRepo(t)
	r := benchRegistry(root)
	ctx := context.Background()
	want, err := r.worktreeTree(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "index"), []byte("not an index"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := r.worktreeTree(ctx, true)
	if err != nil {
		t.Fatalf("a corrupt index must fall back, not fail: %v", err)
	}
	if got != want {
		t.Fatalf("fallback tree %s differs from the cold tree %s", got, want)
	}
}

// A repository with no commits and nothing staged has no index to copy and no
// HEAD to read; the baseline is still the working tree.
func TestSeededBaselineInAnUnbornRepository(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	write(t, root, "a.txt", "a\n")
	r := benchRegistry(root)
	ctx := context.Background()
	cold, err := r.worktreeTree(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if seeded, err := r.worktreeTree(ctx, true); err != nil || seeded != cold {
		t.Fatalf("unborn repo: seeded %s (err %v), cold %s", seeded, err, cold)
	}
}

// The seeded route is three git processes — locate the index, add, write-tree —
// and no more. A fourth is the cost this change removed coming back.
func TestSeededBaselineSpawnBound(t *testing.T) {
	root := messyRepo(t)
	r := benchRegistry(root)
	before := procSpawns.Load()
	if _, err := r.writeWorktreeTree(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := procSpawns.Load() - before; got > 3 {
		t.Fatalf("the seeded baseline spawned %d git processes, bound is 3", got)
	}
}
