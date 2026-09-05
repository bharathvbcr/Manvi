package devcouncil

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// commitAll commits whatever is in the fixture's tree, so a test can establish
// a HEAD to be wrong about before the baseline is taken.
func commitAll(t *testing.T, root string) {
	t.Helper()
	run := func(cmd *exec.Cmd) {
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", cmd.Args, err, out)
		}
	}
	ctx := context.Background()
	run(exec.CommandContext(ctx, "git", "add", "-A"))
	run(exec.CommandContext(ctx, "git", "commit", "-qm", "fixture baseline"))
}

// The turn's diff must describe the turn, not everything since the last commit.
//
// `git diff HEAD` cannot tell the two apart. The operator edits shared.txt and
// does not commit; the agent then edits the same file; the receipt claims both
// edits, and "the agent's change passed" certifies a line the agent never
// wrote. A baseline captured before the turn is what separates them.
func TestScopedDiffAttributesOnlyTheTurnsOwnChange(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "shared.txt", "base\n")
	commitAll(t, f.root)

	// The operator's own uncommitted work, already in the tree.
	writeRepoFile(t, f.root, "shared.txt", "base\nOPERATOR_WIP\n")

	baseline := f.reg.CaptureBaseline(context.Background())
	if !baseline.Captured() {
		t.Fatalf("no baseline was captured: %s", baseline.Note)
	}

	// Then the agent's edit, on top of it.
	writeRepoFile(t, f.root, "shared.txt", "base\nOPERATOR_WIP\nAGENT_EDIT\n")

	diff, _, err := f.reg.scopedDiff(context.Background(), []string{"shared.txt"}, baseline)
	if err != nil {
		t.Fatalf("scopedDiff: %v", err)
	}
	if !strings.Contains(diff, "AGENT_EDIT") {
		t.Fatalf("the turn's own change is missing from its diff:\n%s", diff)
	}
	if strings.Contains(diff, "+OPERATOR_WIP") {
		t.Fatalf("the diff claims the operator's pre-existing edit as this turn's:\n%s", diff)
	}
}

// Capturing a baseline must not disturb the tree it is recording. An operator's
// staged and unstaged work has to be exactly where they left it.
func TestCaptureBaselineLeavesTheTreeAlone(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "tracked.txt", "one\n")
	commitAll(t, f.root)
	writeRepoFile(t, f.root, "tracked.txt", "one\ntwo\n")
	writeRepoFile(t, f.root, "untracked.txt", "new\n")

	before := gitStatus(t, f.root)
	if !strings.Contains(before, "tracked.txt") || !strings.Contains(before, "untracked.txt") {
		t.Fatalf("the fixture did not produce the state under test: %q", before)
	}

	if b := f.reg.CaptureBaseline(context.Background()); !b.Captured() {
		t.Fatalf("no baseline was captured: %s", b.Note)
	}

	if after := gitStatus(t, f.root); after != before {
		t.Fatalf("recording the baseline changed the working tree:\nbefore %q\nafter  %q",
			before, after)
	}
}

// A file the turn creates is the most ordinary thing an agent does, and it has
// to appear in the diff the gates read.
func TestScopedDiffAgainstABaselineIncludesNewFiles(t *testing.T) {
	f := newFixture(t)
	// The fixture already committed its seed, so HEAD exists and the tree is
	// clean — which is exactly the state a turn usually starts from.
	baseline := f.reg.CaptureBaseline(context.Background())
	if !baseline.Captured() {
		t.Fatalf("no baseline was captured: %s", baseline.Note)
	}
	writeRepoFile(t, f.root, "created.go", "package a\n\nfunc New() {}\n")

	diff, _, err := f.reg.scopedDiff(context.Background(), []string{"created.go"}, baseline)
	if err != nil {
		t.Fatalf("scopedDiff: %v", err)
	}
	if !strings.Contains(diff, "created.go") || !strings.Contains(diff, "func New()") {
		t.Fatalf("a file the turn created is not in its diff:\n%s", diff)
	}
}

// Without a baseline the check still runs — a wider diff beats no diff — but it
// must not present the result as attribution.
func TestVerifyPathsWithoutABaselineSaysSo(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "a.go", "package a\n\nfunc A() {}\n")

	got := f.reg.VerifyPaths(context.Background(), []string{"a.go"}, "",
		Baseline{Note: "no pre-turn baseline was recorded"})
	if got.Verdict == VerdictPassed {
		t.Fatalf("a check that could not attribute its diff reported %q: %+v", got.Verdict, got)
	}
	if !strings.Contains(strings.Join(got.Degraded, "\n"), "baseline") {
		t.Errorf("the degradation should name the missing baseline: %v", got.Degraded)
	}
}

func gitStatus(t *testing.T, root string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "status", "--porcelain")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v %s", err, out)
	}
	return string(out)
}
