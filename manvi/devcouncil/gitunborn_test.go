package devcouncil

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A repository's own config can name a program git runs on an ordinary read:
// core.fsmonitor runs on diff, ls-files, status and add, and one that answers
// "nothing changed" also hides changes from the read. Like hooks it runs outside
// the command gate, so every git this package starts switches it off.
func TestGitChildrenRunNoFsmonitor(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	write(t, root, "a.txt", "a\n")
	gitIn(t, root, "add", "a.txt")
	gitIn(t, root, "commit", "-qm", "a")
	write(t, root, "a.txt", "a, edited\n")
	write(t, root, "new.txt", "new\n")

	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	script := filepath.Join(t.TempDir(), "fsmonitor.sh")
	body := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // the program has to be executable
		t.Fatal(err)
	}
	gitIn(t, root, "config", "core.fsmonitor", script)

	ctx := context.Background()
	diff, _, err := gitDiff(ctx, root)
	if err != nil || !strings.Contains(diff, "+a, edited") || !strings.Contains(diff, "+new") {
		t.Fatalf("gitDiff: %v\n%s", err, diff)
	}
	for _, args := range [][]string{
		{"status", "--porcelain"}, {"ls-files", "-z", "--others", "--exclude-standard"}, {"diff", "--cached", "--name-only"},
	} {
		if _, err := runGit(ctx, root, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a repository-configured fsmonitor program ran on a gated git read")
	}
}

// In a repository with no commits `git diff HEAD` has nothing to name, and the
// fallback compared the worktree to the index: a file that was staged and not
// edited since matched itself and read as no change. Those are exactly the files
// a first commit is about to make permanent, and the rigor gates, the secret
// scan and the coverage gate read this diff.
func TestGitDiffSeesStagedFilesInAnUnbornRepository(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	write(t, root, "staged.go", "package staged\n")
	write(t, root, "edited.go", "package edited\n")
	write(t, root, "loose.go", "package loose\n")
	gitIn(t, root, "add", "staged.go", "edited.go")
	write(t, root, "edited.go", "package edited // after staging\n")

	diff, notes, err := gitDiff(context.Background(), root)
	if err != nil || len(notes) != 0 {
		t.Fatalf("gitDiff: %v, notes %v", err, notes)
	}
	for _, want := range []string{"+package staged", "+package edited // after staging", "+package loose"} {
		if !strings.Contains(diff, want) {
			t.Errorf("the diff the gates read lacks %q:\n%s", want, diff)
		}
	}
	if strings.Contains(diff, "+package edited\n") {
		t.Errorf("the diff shows the staged copy of edited.go, not what is on disk:\n%s", diff)
	}

	r := benchRegistry(root)
	scoped, _, err := r.scopedDiff(context.Background(), []string{"staged.go", "edited.go"}, Baseline{})
	if err != nil {
		t.Fatalf("scopedDiff: %v", err)
	}
	for _, want := range []string{"+package staged", "+package edited // after staging"} {
		if !strings.Contains(scoped, want) {
			t.Errorf("the scoped diff lacks %q:\n%s", want, scoped)
		}
	}
}
