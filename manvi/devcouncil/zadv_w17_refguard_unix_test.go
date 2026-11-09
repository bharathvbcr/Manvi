//go:build unix

package devcouncil

import (
	"os"
	"path/filepath"
	"testing"
)

// A git that cannot list the commits of a moved ref (rev-list fails after the
// command) must leave the call blocked, never clean: an unreadable set is not
// a clean one.
func TestZadvW17ScanFailureAfterALeakingCommitBlocks(t *testing.T) {
	f := shellCommitFixture(t, true)
	fakeGit(t, `for a in "$@"; do [ "$a" = rev-list ] && exit 128; done
exec REALGIT "$@"`)
	res := shell(f, "git commit -qm leak")
	advBlocked(t, res, "rev-list failing after a leaking commit")
}

// A failing git in a subdirectory of a repository still refuses the command:
// the .git is found in an ancestor.
func TestZadvW17FailingGitInASubdirectoryOfARepoStillRefuses(t *testing.T) {
	f := shellCommitFixture(t, false)
	sub := filepath.Join(f.root, "sub")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	f.reg.deps.Root = sub
	fakeGit(t, `echo "fatal: kein Git-Repository" >&2; exit 128`)
	res := shell(f, "echo should-not-run > ran.txt")
	if !res.IsError {
		t.Fatalf("a command ran although git failed under a repository: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(sub, "ran.txt")); err == nil {
		t.Fatal("the command ran")
	}
}

// A symlinked Registry root whose lexical ancestors hold no .git but whose real
// directory is inside a repository: git works, so the guard sees refs and
// catches a leak (the lexical walk in insideRepository is only on failure).
func TestZadvW17SymlinkedRootIsGuarded(t *testing.T) {
	f := shellCommitFixture(t, true)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(f.root, link); err != nil {
		t.Fatal(err)
	}
	f.reg.deps.Root = link
	head := headOf(t, f.root)
	res := shell(f, "git commit -qm leak")
	advBlocked(t, res, "leak through a symlinked root")
	if got := headOf(t, f.root); got != head {
		t.Fatalf("not rolled back: %s -> %s", head, got)
	}
}
