package devcouncil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests drive the lease-free check against a real repository, because the
// two things most likely to be wrong about it are what it refuses to look at
// and what it says when it could not look at anything.

// The fixture is the package's own: a real git repository, a real store and
// the real verifier binary. Standing up a second one here would have been a
// second answer to "what does a configured registry look like", and the first
// thing to drift.

// writeRepoFile puts a file in the fixture's repository.
func writeRepoFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withoutVerifier detaches the content gates, so a test can assert what the
// report says when they could not run.
func withoutVerifier(f *fixture) *fixture {
	f.reg.deps.VerifierBinary = ""
	return f
}

// The headline property: with no verifier binary configured, the content gates
// did not run, and a report that says "passed" would be claiming they did.
func TestVerifyPathsWithoutTheVerifierIsDegradedNotPassed(t *testing.T) {
	f := withoutVerifier(newFixture(t))
	writeRepoFile(t, f.root, "a.go", "package a\n\nfunc A() {}\n")

	got := f.reg.VerifyPaths(context.Background(), []string{"a.go"}, "")
	if got.Verdict != VerdictDegraded {
		t.Fatalf("verdict = %q, want degraded — the gates did not run", got.Verdict)
	}
	if got.Passed() {
		t.Fatal("a degraded report reported itself as passed")
	}
	if len(got.Degraded) == 0 {
		t.Fatal("a degraded verdict named nothing that failed to run")
	}
}

// Harness bookkeeping is excluded, and the exclusion is stated. A short
// examined list that does not say why is indistinguishable from a quiet turn.
func TestVerifyPathsExcludesHarnessStateAndSaysSo(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, ".devcouncil/artifacts/plan.md", "# plan\n")
	writeRepoFile(t, f.root, "a.go", "package a\n")

	got := f.reg.VerifyPaths(context.Background(),
		[]string{".devcouncil/artifacts/plan.md", "a.go"}, "")
	if len(got.Examined) != 1 || got.Examined[0] != "a.go" {
		t.Fatalf("examined = %v, want only the repository source", got.Examined)
	}
	var named bool
	for _, s := range got.Skipped {
		if strings.Contains(s, ".devcouncil/artifacts/plan.md") {
			named = true
		}
	}
	if !named {
		t.Fatalf("the exclusion was silent: %v", got.Skipped)
	}
}

// The exclusion is by path component, not by string prefix. A file whose name
// merely starts with the same letters is ordinary source, and a filter that
// dropped it would quietly stop verifying real code.
func TestVerifyPathsDoesNotExcludeNeighboursOfTheHarnessDirectory(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, ".devcouncilish/a.go", "package a\n")

	got := f.reg.VerifyPaths(context.Background(), []string{".devcouncilish/a.go"}, "")
	if len(got.Examined) != 1 {
		t.Fatalf("examined = %v, want the neighbouring file to be checked", got.Examined)
	}
}

// A path list longer than the cap is truncated, and the truncation is reported
// per dropped path rather than left for the reader to infer from a count.
func TestVerifyPathsCapsThePathListAudibly(t *testing.T) {
	f := newFixture(t)
	var paths []string
	for i := range maxVerifiedPaths + 10 {
		rel := fmt.Sprintf("f%03d.go", i)
		writeRepoFile(t, f.root, rel, "package a\n")
		paths = append(paths, rel)
	}

	got := f.reg.VerifyPaths(context.Background(), paths, "")
	if len(got.Examined) != maxVerifiedPaths {
		t.Fatalf("examined %d paths, want the cap of %d", len(got.Examined), maxVerifiedPaths)
	}
	if len(got.Skipped) != 10 {
		t.Fatalf("skipped = %d, want the 10 paths over the cap named", len(got.Skipped))
	}
}

// Handlers reported a change and git sees none. Not a pass: the gates read the
// diff, and an empty diff gives them nothing to judge.
func TestVerifyPathsTreatsAnEmptyDiffAsDegraded(t *testing.T) {
	f := newFixture(t)

	got := f.reg.VerifyPaths(context.Background(), []string{"seed.txt"}, "")
	if got.Verdict != VerdictDegraded {
		t.Fatalf("verdict = %q, want degraded for an unchanged file", got.Verdict)
	}
}

// A path that shares a name with a git revision must be read as a path. Without
// the `--` separator git resolves it as a revision and the diff describes
// something else entirely.
func TestVerifyPathsIsNotConfusedByAPathThatLooksLikeARevision(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "HEAD", "not a revision\n")

	got := f.reg.VerifyPaths(context.Background(), []string{"HEAD"}, "")
	// The verdict is degraded either way here (no verifier binary), so the
	// assertion is that the call completed against the path rather than
	// erroring out or reporting on the whole tree.
	if len(got.Examined) != 1 || got.Examined[0] != "HEAD" {
		t.Fatalf("examined = %v, want the file named HEAD", got.Examined)
	}
	for _, d := range got.Degraded {
		if strings.Contains(d, "could not be produced") {
			t.Fatalf("the scoped diff failed on a path that looks like a revision: %v", got.Degraded)
		}
	}
}

// The operator's command is the project's own definition of working, and a
// non-zero exit is a failure with the output attached.
func TestVerifyPathsFailsOnANonZeroVerificationCommand(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "a.go", "package a\n")

	got := f.reg.VerifyPaths(context.Background(), []string{"a.go"},
		"echo 'build failed: undefined x' >&2; exit 2")
	if got.Verdict != VerdictFailed {
		t.Fatalf("verdict = %q, want failed", got.Verdict)
	}
	joined := strings.Join(got.Findings, "\n")
	if !strings.Contains(joined, "undefined x") {
		t.Fatalf("the command's output did not reach the findings: %v", got.Findings)
	}
}

// A command that cannot start is not the same fact as a build that is broken,
// and folding them teaches an operator to ignore the one that matters.
func TestVerifyPathsDegradesWhenTheCommandCannotRun(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "a.go", "package a\n")

	got := f.reg.VerifyPaths(context.Background(), []string{"a.go"},
		"this-command-does-not-exist-anywhere")
	// `sh -c` reports a missing command as exit 127, which is a failure of the
	// command rather than of starting the shell — so this is a failed verdict
	// carrying the shell's own explanation, not a silent pass.
	if got.Verdict == VerdictPassed {
		t.Fatalf("a command that does not exist produced a pass: %+v", got)
	}
}

// A command that never finishes must not hang the turn, and what it proves is
// nothing.
func TestVerifyPathsBoundsTheVerificationCommand(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "a.go", "package a\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the same path a turn's cancellation takes
	got := f.reg.VerifyPaths(ctx, []string{"a.go"}, "sleep 60")
	if got.Verdict == VerdictPassed {
		t.Fatalf("a command that never completed produced a pass: %+v", got)
	}
}

// Every changed path filtered away is not a pass either: the turn reported
// changes and nothing here is evidence about any of them.
func TestVerifyPathsDegradesWhenEverythingWasFiltered(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, ".devcouncil/artifacts/plan.md", "# plan\n")

	got := f.reg.VerifyPaths(context.Background(), []string{".devcouncil/artifacts/plan.md"}, "")
	if got.Verdict != VerdictDegraded {
		t.Fatalf("verdict = %q, want degraded", got.Verdict)
	}
}

// ExistingPaths has to tell a deleted file from an unreadable one: folding them
// drops a file silently out of every check downstream.
func TestExistingPathsSeparatesMissingFromUnreadable(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "there.go", "package a\n")

	present, missing := f.reg.ExistingPaths([]string{"there.go", "gone.go", "there.go"})
	if len(present) != 1 || present[0] != "there.go" {
		t.Fatalf("present = %v, want the one existing path, de-duplicated", present)
	}
	if len(missing) != 1 || missing[0] != "gone.go" {
		t.Fatalf("missing = %v, want the deleted path", missing)
	}
}

// Output handed to a model is bounded, cut on rune boundaries, and says that it
// was cut. A silently shortened build log reads as a complete one.
func TestTruncateOutputIsBoundedAndRuneSafe(t *testing.T) {
	long := strings.Repeat("日本語エラー", maxVerifyCommandOutputRunes)
	got := truncateOutput(long)
	if len([]rune(got)) > maxVerifyCommandOutputRunes+64 {
		t.Fatalf("truncated output is %d runes, past the cap", len([]rune(got)))
	}
	if !strings.Contains(got, "omitted") {
		t.Fatal("a shortened output did not say it was shortened")
	}
	if strings.ContainsRune(got, '�') {
		t.Fatal("truncation cut a rune in half")
	}
	// The tail is kept as well as the head: a compiler prints its summary last.
	if !strings.HasSuffix(got, "エラー") {
		t.Fatalf("the tail of the output was discarded: %q", got[len(got)-40:])
	}
}

func TestTruncateOutputLeavesShortOutputAlone(t *testing.T) {
	if got := truncateOutput("all good"); got != "all good" {
		t.Fatalf("truncateOutput mangled short output: %q", got)
	}
}

// A file the turn created is untracked, and `git diff HEAD` says nothing about
// an untracked file. This is the most ordinary thing an agent does, and before
// the untracked pass existed it produced an empty diff — so the gates read
// nothing, every new file came back unverified, and the report looked exactly
// like a check that had run.
func TestVerifyPathsSeesANewlyCreatedFile(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "brand_new.go", "package a\n\nfunc New() int { return 1 }\n")

	got := f.reg.VerifyPaths(context.Background(), []string{"brand_new.go"}, "")
	for _, d := range got.Degraded {
		if strings.Contains(d, "empty diff") {
			t.Fatalf("a newly created file produced an empty diff: %+v", got)
		}
	}
}

// The untracked pass must stay scoped to this turn's paths. An operator's own
// new files are none of the check's business, and rendering them would put work
// the turn did not do in front of the gates — and blame the turn for it.
func TestVerifyPathsIgnoresUnrelatedUntrackedFiles(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "mine.go", "package a\n\nfunc Mine() {}\n")
	writeRepoFile(t, f.root, "operator_wip.go", "package a\n\nfunc WIP() {}\n")

	got := f.reg.VerifyPaths(context.Background(), []string{"mine.go"}, "")
	if len(got.Examined) != 1 || got.Examined[0] != "mine.go" {
		t.Fatalf("examined = %v, want only this turn's file", got.Examined)
	}
	for _, finding := range got.Findings {
		if strings.Contains(finding, "operator_wip.go") {
			t.Fatalf("the check reported on a file the turn never touched: %v", got.Findings)
		}
	}
}

// A failure must never be downgraded to a degradation on the way out. The two
// mean opposite things to an operator: one says the work is wrong, the other
// says nobody looked.
func TestVerifyPathsNeverDowngradesAFailure(t *testing.T) {
	f := withoutVerifier(newFixture(t))
	writeRepoFile(t, f.root, "a.go", "package a\n")

	// The command fails; the content gates cannot run. Both are true, and the
	// verdict has to be the one that says the work is broken.
	got := f.reg.VerifyPaths(context.Background(), []string{"a.go"}, "exit 1")
	if got.Verdict != VerdictFailed {
		t.Fatalf("verdict = %q, want failed — a degradation erased a real failure", got.Verdict)
	}
	if len(got.Degraded) == 0 {
		t.Fatal("the failure swallowed the degradation; both are facts about this turn")
	}
}

// The path set the check is handed comes from tool handlers, which are driven
// by a model, which is driven by whatever it read. So the set is untrusted
// input and these are the shapes that would hurt.

// A traversal must never reach the check. The gate refuses these on the write
// path, but the check has to hold on its own: a path outside the repository is
// not in a repository diff, and handing it to git would either error or,
// worse, resolve somewhere.
func TestVerifyPathsRefusesTraversalAndAbsolutePaths(t *testing.T) {
	f := newFixture(t)
	hostile := []string{
		"../../../etc/passwd",
		"/etc/passwd",
		"a/../../outside.go",
		"..",
	}
	got := f.reg.VerifyPaths(context.Background(), hostile, "")
	for _, e := range got.Examined {
		for _, h := range hostile {
			if e == h {
				t.Fatalf("the check examined %q, which is outside the repository", e)
			}
		}
		if strings.HasPrefix(e, "..") || strings.HasPrefix(e, "/") {
			t.Fatalf("the check examined %q", e)
		}
	}
	if got.Verdict == VerdictPassed {
		t.Fatalf("a set of unusable paths produced a pass: %+v", got)
	}
	if len(got.Skipped) == 0 {
		t.Fatal("paths were dropped without a word")
	}
}

// A path carrying a newline would split a diff header if it were ever rendered
// into one. It must be handled as a path, not as two.
func TestVerifyPathsHandlesHostileFilenames(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{
		"weird name with spaces.go",
		"unicode_日本語.go",
		"dash-leading.go",
		"--not-a-flag.go",
	} {
		writeRepoFile(t, f.root, name, "package a\n")
	}
	got := f.reg.VerifyPaths(context.Background(), []string{
		"weird name with spaces.go", "unicode_日本語.go", "dash-leading.go", "--not-a-flag.go",
	}, "")
	// The assertion is that the call completed and attributed each path to
	// itself rather than erroring or reporting on the tree.
	if len(got.Examined) != 4 {
		t.Fatalf("examined = %v, want all four", got.Examined)
	}
	for _, d := range got.Degraded {
		if strings.Contains(d, "could not be produced") {
			t.Fatalf("a hostile filename broke the diff: %v", got.Degraded)
		}
	}
}

// Duplicates in the input are one file to check, and must not consume the cap.
func TestVerifyPathsDeduplicatesItsInput(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "one.go", "package a\n")

	paths := make([]string, 0, 50)
	for range 50 {
		paths = append(paths, "one.go")
	}
	got := f.reg.VerifyPaths(context.Background(), paths, "")
	if len(got.Examined) != 1 {
		t.Fatalf("examined = %v, want one entry for one file", got.Examined)
	}
}

// The empty set is not a pass. A caller that lost its path list must not be
// told everything is fine.
func TestVerifyPathsOnNothingIsNotAPass(t *testing.T) {
	f := newFixture(t)
	got := f.reg.VerifyPaths(context.Background(), nil, "")
	if got.Verdict == VerdictPassed {
		t.Fatalf("verifying nothing produced a pass: %+v", got)
	}
}

// A verification command must not be able to hold a turn open indefinitely.
// The bound is the harness's, not the command's.
func TestVerifyCommandTimeoutIsBounded(t *testing.T) {
	if verifyCommandTimeout <= 0 {
		t.Fatal("the verification command has no timeout, so a hanging check hangs the turn")
	}
	if verifyCommandTimeout > 10*time.Minute {
		t.Fatalf("the verification command may run for %s at the end of every mutating turn",
			verifyCommandTimeout)
	}
}
