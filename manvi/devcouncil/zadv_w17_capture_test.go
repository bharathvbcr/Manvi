package devcouncil

import (
	"context"
	"strings"
	"testing"
)

// The serve hold treats a linked worktree nested in a checkout (GitPulse puts them
// under .claude/worktrees) as another working tree, on the premise that the diff
// captured for the root does not contain the lane's files. This pins the premise:
// gitDiff, which work.verify.task reads, names neither a nested lane's files nor an
// embedded repository's.
func TestGitDiffOfARootDoesNotCaptureANestedLanesFiles(t *testing.T) {
	root := newGitRepo(t)
	run(t, root, "worktree", "add", "-q", "-b", "lane", ".claude/worktrees/lane")
	write(t, root, ".claude/worktrees/lane/lane-only.txt", "LANE-SECRET-MARKER\n")
	run(t, root, "init", "-q", "-b", "main", "embedded")
	write(t, root, "embedded/embedded-only.txt", "EMBEDDED-SECRET-MARKER\n")
	write(t, root, "seed.txt", "seed\nchanged\n")

	diff, _, err := gitDiff(context.Background(), root)
	if err != nil {
		t.Fatalf("gitDiff: %v", err)
	}
	if !strings.Contains(diff, "seed.txt") {
		t.Fatalf("the root's own change is missing from its diff:\n%s", diff)
	}
	for _, leaked := range []string{"lane-only.txt", "LANE-SECRET-MARKER", "embedded-only.txt", "EMBEDDED-SECRET-MARKER"} {
		if strings.Contains(diff, leaked) {
			t.Errorf("the root's diff contains %q from a nested working tree:\n%s", leaked, diff)
		}
	}
}
