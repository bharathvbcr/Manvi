package devcouncil

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// textconvRepo gives the fixture's repository a diff driver whose textconv
// program records that it ran, for every .txt file: the shape of a hostile
// worktree, or of an operator's own config, that makes `git diff` run a program.
func textconvRepo(t *testing.T, f *fixture) (marker string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the textconv program is a shell script")
	}
	dir := t.TempDir()
	marker = filepath.Join(dir, "textconv-ran")
	script := filepath.Join(dir, "tc.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n: > '"+marker+"'\ncat \"$1\"\n"), 0o700); err != nil { // #nosec G306 -- a stand-in executable under t.TempDir must be executable
		t.Fatal(err)
	}
	writeRepoFile(t, f.root, ".gitattributes", "*.txt diff=audit\n")
	writeRepoFile(t, f.root, "shared.txt", "base\n")
	commitAll(t, f.root)
	gitIn(t, f.root, "config", "diff.audit.textconv", script)
	return marker
}

func requireNotRun(t *testing.T, marker, what string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("%s ran the repository's diff.audit.textconv program", what)
	}
}

// The scoped diff the end-of-turn check and work.verify.task both read is built
// through gitSpec, so a driver in the repository's config must never run there:
// against a baseline tree, against HEAD, and with no HEAD to compare to.
func TestScopedDiffNeverRunsTheRepositorysTextconvProgram(t *testing.T) {
	t.Run("against a baseline", func(t *testing.T) {
		f := newFixture(t)
		marker := textconvRepo(t, f)
		baseline := f.reg.CaptureBaseline(context.Background())
		if !baseline.Captured() {
			t.Fatalf("no baseline: %s", baseline.Note)
		}
		writeRepoFile(t, f.root, "shared.txt", "base\nAGENT_EDIT\n")
		diff, _, err := f.reg.scopedDiff(context.Background(), []string{"shared.txt"}, baseline)
		if err != nil {
			t.Fatal(err)
		}
		requireNotRun(t, marker, "the baseline diff")
		if !strings.Contains(diff, "+AGENT_EDIT") {
			t.Fatalf("the diff text changed:\n%s", diff)
		}
	})
	t.Run("against HEAD", func(t *testing.T) {
		f := newFixture(t)
		marker := textconvRepo(t, f)
		writeRepoFile(t, f.root, "shared.txt", "base\nAGENT_EDIT\n")
		diff, _, err := f.reg.scopedDiff(context.Background(), []string{"shared.txt"}, Baseline{})
		if err != nil {
			t.Fatal(err)
		}
		requireNotRun(t, marker, "the HEAD diff")
		if !strings.Contains(diff, "+AGENT_EDIT") {
			t.Fatalf("the diff text changed:\n%s", diff)
		}
	})
}

// gitSpec is the one place every git child here is built, so the guard lives
// there: every patch-producing subcommand, however it is reached, runs without
// the repository's textconv and external-diff programs.
func TestGitChildrenNeverRunTheRepositorysTextconvProgram(t *testing.T) {
	f := newFixture(t)
	marker := textconvRepo(t, f)
	writeRepoFile(t, f.root, "shared.txt", "base\nAGENT_EDIT\n")
	ctx := context.Background()
	for _, args := range [][]string{
		{"diff"},
		{"diff", "HEAD", "--", "shared.txt"},
		{"--literal-pathspecs", "diff", "--", "shared.txt"},
		{"show", "--format=", "HEAD"},
		{"log", "-p", "-1", "--format="},
		// Global options, with their values attached and separate, before the subcommand.
		{"--git-dir=" + filepath.Join(f.root, ".git"), "diff"},
		{"--git-dir", filepath.Join(f.root, ".git"), "--work-tree", f.root, "diff"},
		{"-C", f.root, "-c", "core.abbrev=8", "show", "--format=", "HEAD"},
		{"--namespace", "n", "log", "-p", "-1", "--format="},
		// blame runs a textconv driver by default too; diff-tree and diff-index
		// only when asked, which the flags keep from being added.
		{"blame", "shared.txt"},
		{"diff-tree", "-p", "HEAD"},
		{"diff-index", "-p", "HEAD"},
	} {
		out, err := runGit(ctx, f.root, args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		requireNotRun(t, marker, "git "+strings.Join(args, " "))
		if slices.Contains(args, "diff") && !strings.Contains(out, "+AGENT_EDIT") {
			t.Fatalf("git %v: the diff text changed:\n%s", args, out)
		}
	}
}

// What the guard adds is two flags after the subcommand and nothing else: a
// command that is not a patch producer, and an argument that merely looks like a
// subcommand, are left alone.
func TestWithoutDiffProgramsOnlyTouchesPatchSubcommands(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"diff HEAD --", "diff --no-textconv --no-ext-diff HEAD --"},
		{"--literal-pathspecs diff --", "--literal-pathspecs diff --no-textconv --no-ext-diff --"},
		{"-c a=b show -s", "-c a=b show --no-textconv --no-ext-diff -s"},
		{"log -p", "log --no-textconv --no-ext-diff -p"},
		{"--git-dir .git diff", "--git-dir .git diff --no-textconv --no-ext-diff"},
		{"--git-dir=.git --work-tree w --namespace n log -p", "--git-dir=.git --work-tree w --namespace n log --no-textconv --no-ext-diff -p"},
		{"diff-tree -p HEAD", "diff-tree --no-textconv --no-ext-diff -p HEAD"},
		{"diff-index -p HEAD", "diff-index --no-textconv --no-ext-diff -p HEAD"},
		{"diff-files -p", "diff-files --no-textconv --no-ext-diff -p"},
		{"format-patch -1", "format-patch --no-textconv --no-ext-diff -1"},
		{"whatchanged -p", "whatchanged --no-textconv --no-ext-diff -p"},
		{"blame f", "blame --no-textconv f"},
		{"stash show -p", "stash show --no-textconv --no-ext-diff -p"},
		{"stash list", "stash list"},
		{"stash", "stash"},
		{"-C dir", "-C dir"},
		{"rev-parse HEAD", "rev-parse HEAD"},
		{"ls-files -z -- diff", "ls-files -z -- diff"},
		{"", ""},
	} {
		got := strings.Join(withoutDiffPrograms(strings.Fields(tc.in)), " ")
		if got != tc.want {
			t.Errorf("withoutDiffPrograms(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
