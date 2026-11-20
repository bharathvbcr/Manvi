package devcouncil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// shimGit puts a git on PATH that runs script's body for the subcommands it
// names and the real git for everything else.
func shimGit(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the git shim is a shell script")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found: %v", err)
	}
	shim := t.TempDir()
	script := "#!/bin/sh\n" + body + "\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // a git stand-in under t.TempDir must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
}

const gitDiffThenFail128 = `for a in "$@"; do
  if [ "$a" = "diff" ]; then
    printf 'diff --git a/seed.txt b/seed.txt\n--- a/seed.txt\n+++ b/seed.txt\n@@ -1 +1 @@\n-seed\n+changed\n'
    echo 'fatal: cannot read the object database' >&2
    exit 128
  fi
done`

// A git that wrote part of a diff and then died is not a git that answered. The
// gates would read the part as the whole change, so the exit status decides, not
// whether stdout had bytes in it.
func TestGitDiffRefusesADiffFromAGitThatExited128(t *testing.T) {
	root := newGitRepo(t)
	shimGit(t, gitDiffThenFail128)

	diff, _, err := gitDiff(context.Background(), root)
	if err == nil {
		t.Fatalf("gitDiff returned a diff from a git that exited 128:\n%s", diff)
	}
	if !strings.Contains(err.Error(), "128") {
		t.Errorf("the error does not carry the exit status: %v", err)
	}
}

// Only the caller knows which non-zero statuses are an answer: --no-index exits
// 1 when the files differ, which is its normal result, and 128 is still a failure.
func TestRunGitCaptureTakesOnlyTheExitCodesTheCallerListed(t *testing.T) {
	root := newGitRepo(t)
	write(t, root, "new.txt", "x\n")

	c, err := runGitCapture(context.Background(), root, maxGitCaptureBytes, gitDiffNoIndexExit, untrackedDiffArgs("new.txt")...)
	if err != nil || !strings.Contains(c.out, "+x") {
		t.Fatalf("--no-index exit 1 with a diff must be accepted when listed: out=%q err=%v", c.out, err)
	}
	if _, err := runGitCapture(context.Background(), root, maxGitCaptureBytes, gitExitOK, untrackedDiffArgs("new.txt")...); err == nil {
		t.Fatal("exit 1 was accepted by a caller that listed only 0")
	}

	shimGit(t, gitDiffThenFail128)
	if c, err := runGitCapture(context.Background(), root, maxGitCaptureBytes, gitDiffNoIndexExit, "diff", "HEAD"); err == nil {
		t.Fatalf("exit 128 was accepted by a caller that listed {0,1}: %q", c.out)
	}
}
