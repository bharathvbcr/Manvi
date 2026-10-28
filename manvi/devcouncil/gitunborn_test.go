package devcouncil

import (
	"context"
	"strings"
	"testing"
)

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
