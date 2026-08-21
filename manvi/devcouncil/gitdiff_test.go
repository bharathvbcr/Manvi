package devcouncil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newGitRepo makes a real repository. gitDiff shells out to git, so a fake
// would be testing the fake.
func newGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not on PATH: %v", err)
	}
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "gate@example.test"},
		{"config", "user.name", "gate"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	// One commit so HEAD exists and the ordinary `git diff HEAD` path is taken.
	write(t, root, "seed.txt", "seed\n")
	run(t, root, "add", "seed.txt")
	run(t, root, "commit", "-qm", "seed")
	return root
}

func run(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func write(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGitDiffSeesUntrackedPathsWithAwkwardNames is the one that matters for
// safety. The diff this produces is what the rigor gates read: a file missing
// from it is a file the secret scanner never looks at. Splitting git's output
// on whitespace turned "my notes.go" into two paths that both fail to render,
// and the file disappeared from the diff with no diagnosis anywhere.
func TestGitDiffSeesUntrackedPathsWithAwkwardNames(t *testing.T) {
	root := newGitRepo(t)

	awkward := []string{
		"my notes.go",
		"dir with space/inner.go",
		"héllo.go",
		"two  spaces.go",
	}
	for _, name := range awkward {
		write(t, root, name, "package main // "+name+"\n")
	}

	diff, notes, err := gitDiff(context.Background(), root)
	if err != nil {
		t.Fatalf("gitDiff: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("unexpected degradations: %v", notes)
	}

	files, err := changedFiles(diff)
	if err != nil {
		t.Fatalf("changedFiles: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		seen[f] = true
	}
	for _, name := range awkward {
		if !seen[name] {
			t.Errorf("untracked file %q is missing from the diff the gates read; got %v", name, files)
		}
	}
}

// TestGitDiffBoundsAndReportsItsFanOut covers the other half. Rendering one
// subprocess per untracked file is unbounded work driven by whatever is sitting
// in the working tree. It has to be capped — and the cap has to be *reported*,
// because a truncated diff that reads as complete is a set of gates that
// silently stopped covering most of the tree.
func TestGitDiffBoundsAndReportsItsFanOut(t *testing.T) {
	root := newGitRepo(t)

	const created = maxUntrackedRendered + 25
	for i := 0; i < created; i++ {
		write(t, root, filepath.Join("many", "f"+itoa(i)+".go"), "package many\n")
	}

	start := time.Now()
	_, notes, err := gitDiff(context.Background(), root)
	if err != nil {
		t.Fatalf("gitDiff: %v", err)
	}
	t.Logf("rendered in %s", time.Since(start))

	if len(notes) == 0 {
		t.Fatalf("%d untracked files exceeded the cap of %d but nothing was reported as degraded",
			created, maxUntrackedRendered)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "untracked") {
		t.Errorf("the degradation does not name what was dropped: %q", joined)
	}
	// Both numbers, per the rule that a capped sample is never presented as
	// complete coverage.
	if !strings.Contains(joined, itoa(created)) {
		t.Errorf("the degradation does not carry the total (%d): %q", created, joined)
	}
}

// TestRunGitStopsWhenTheContextIsDone asserts a git invocation is bounded by
// the caller's context rather than running to completion regardless.
func TestRunGitStopsWhenTheContextIsDone(t *testing.T) {
	root := newGitRepo(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runGit(ctx, root, "diff", "HEAD")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runGit ignored a cancelled context")
	}
}

// TestGitDiffAppliesItsOwnDeadline asserts gitDiff bounds itself even when the
// caller hands it a context that never expires. The tool dispatch path does
// exactly that.
func TestGitDiffAppliesItsOwnDeadline(t *testing.T) {
	root := newGitRepo(t)
	write(t, root, "untracked.go", "package main\n")

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = gitDiff(context.Background(), root)
	}()
	select {
	case <-done:
	case <-time.After(gitTimeout + 30*time.Second):
		t.Fatal("gitDiff never returned; it has no bound of its own")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
