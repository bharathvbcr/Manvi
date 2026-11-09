//go:build unix

package devcouncil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGit puts a `git` first on PATH (the guard's git children take PATH from
// the process, see childenv.Git) that records one line per invocation in the
// returned counter file and then runs body. The real git is named by absolute
// path so the script never finds itself.
func fakeGit(t *testing.T, body string) (counter string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	dir := t.TempDir()
	counter = filepath.Join(dir, "count")
	script := fmt.Sprintf("#!/bin/sh\necho x >> %q\n%s\n", counter, strings.ReplaceAll(body, "REALGIT", real))
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil { // #nosec G306 -- an executable test stub in a private temp dir.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return counter
}

func invocations(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter) // #nosec G304 -- the counter file this test's own fake git wrote in its temp dir.
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x\n")
}

// Non-repository detection does not read git's message: a git that answers in
// German (exit 128) in a directory that is not a repository is still "not a
// repository", and the command runs.
func TestNonRepoDetectionIsLocaleIndependent(t *testing.T) {
	f := shellCommitFixture(t, false)
	f.reg.deps.Root = t.TempDir()
	fakeGit(t, `echo "fatal: kein Git-Repository (oder irgendein Elternverzeichnis): .git" >&2; exit 128`)
	snap, err := f.reg.snapshotRefs(t.Context())
	if err != nil || snap != nil {
		t.Fatalf("a translated non-repo message was not recognised: snap=%v err=%v", snap, err)
	}
	if res := shell(f, "echo hi"); res.IsError || !strings.Contains(res.Text, "hi") {
		t.Fatalf("a command in a non-repository failed: %+v", res)
	}
}

// The same against the real git under a German locale.
func TestNonRepoDetectionUnderAGermanLocale(t *testing.T) {
	f := shellCommitFixture(t, false)
	f.reg.deps.Root = t.TempDir()
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de")
	if res := shell(f, "echo hi"); res.IsError || !strings.Contains(res.Text, "hi") {
		t.Fatalf("a command in a non-repository failed under de_DE: %+v", res)
	}
}

// A git that fails in a directory that IS a repository still refuses the
// command: only the absence of any repository is "not guarded".
func TestFailingGitInARepositoryStillRefusesTheCommand(t *testing.T) {
	f := shellCommitFixture(t, false)
	fakeGit(t, `echo "fatal: kein Git-Repository" >&2; exit 128`)
	res := shell(f, "echo should-not-run > ran.txt")
	if !res.IsError || !strings.Contains(res.Text, "snapshot") {
		t.Fatalf("a command ran although git failed inside a repository: %+v", res)
	}
}

// The guard's cost in git processes per exec_command (nothing moves), for one
// worktree and for 20 linked ones. Each snapshot is for-each-ref, worktree list
// and one for-each-ref per linked worktree (the per-worktree refs cannot be
// listed from elsewhere); the guard takes two snapshots. A regression fails.
func TestGuardGitProcessCostIsBounded(t *testing.T) {
	for _, linked := range []int{0, 20} {
		t.Run(fmt.Sprintf("%d_linked", linked), func(t *testing.T) {
			f := shellCommitFixture(t, false)
			for i := 0; i < linked; i++ {
				run(t, f.root, "worktree", "add", "-q", "--detach", filepath.Join(filepath.Dir(f.root), fmt.Sprintf("wt-cost-%d", i)))
			}
			counter := fakeGit(t, `exec REALGIT "$@"`)
			if res := shell(f, "echo hi"); res.IsError {
				t.Fatalf("the command failed: %+v", res)
			}
			got := invocations(t, counter)
			bound := 2 * (2 + linked)
			t.Logf("%d linked worktrees: %d git processes per exec_command (bound %d)", linked, got, bound)
			if got > bound {
				t.Errorf("the guard ran %d git processes with %d linked worktrees, bound %d", got, linked, bound)
			}
			if got == 0 {
				t.Errorf("the counter saw no git process: the fake git was not the one the guard ran")
			}
		})
	}
}
