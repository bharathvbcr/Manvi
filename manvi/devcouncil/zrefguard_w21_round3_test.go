package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// An id that parses but names no object is not pushable, and is not a blind
// spot: when a later command writes the object, the unchanged file now names a
// commit, the entry is a different value, and it is judged. The file did not
// change, so restoring it removes nothing: the leak is reported as manual and
// every later command is refused until it is gone.
func TestW21EntryWhoseObjectAppearsLaterIsJudged(t *testing.T) {
	f := shellCommitFixture(t, true)
	// Build the leaking commit in a scratch clone with fixed dates, so its id is
	// known before it exists in the repository under guard.
	scratch := filepath.Join(t.TempDir(), "scratch")
	run(t, filepath.Dir(scratch), "clone", "-q", f.root, scratch)
	write(t, scratch, "notes.txt", secretBody)
	run(t, scratch, "add", "notes.txt")
	tree := gitOut(t, scratch, "write-tree")
	leak := gitOut(t, scratch, "-c", "user.name=a", "-c", "user.email=a@a", "commit-tree", tree, "-p", headOf(t, scratch), "-m", "leak")
	if _, err := os.Stat(filepath.Join(gitCommon(t, f.root), "objects", leak[:2], leak[2:])); err == nil {
		t.Fatal("bad test: the object already exists in the repository under guard")
	}

	// 1. The id is written while its object does not exist: allowed, silent.
	write(t, f.root, "name.sh", "d=$(git rev-parse --git-common-dir)\necho "+leak+" > $d/LATE_HEAD\n")
	if res := shell(f, "sh name.sh"); res.IsError || res.Blocked {
		t.Fatalf("an id that names no object was refused: %+v", res)
	}
	if res := shell(f, "echo fine"); res.IsError || !strings.Contains(res.Text, "fine") {
		t.Fatalf("the next command was refused: %+v", res)
	}

	// 2. The object arrives (its blobs, tree and commit are copied in).
	write(t, f.root, "arrive.sh", "cp -R "+filepath.Join(scratch, ".git", "objects")+"/. \"$(git rev-parse --git-common-dir)/objects/\"\n")
	res := shell(f, "sh arrive.sh")
	if !res.IsError || !res.Blocked || res.Rule != string(policy.RuleSecretPath) {
		t.Fatalf("FAIL-OPEN: the object a ref-source file already named arrived and was not judged: %+v", res)
	}
	if !strings.Contains(res.Text, "MANUAL INTERVENTION") {
		t.Errorf("restoring an unchanged file removes nothing, so this must be manual: %s", res.Text)
	}

	// 3. Nothing runs, and so nothing pushes it, until the file is gone.
	remote := bareRemote(t)
	push := shell(f, "git push -q "+remote+" LATE_HEAD:refs/heads/leaked")
	if !push.IsError || !strings.Contains(push.Text, "ref snapshot") {
		t.Fatalf("a command ran beside a leak that could not be put back: %+v", push)
	}
	requireNothingArrived(t, remote)
	removeFile(t, filepath.Join(gitCommon(t, f.root), "LATE_HEAD"))
	if ok := shell(f, "echo fine"); ok.IsError || !strings.Contains(ok.Text, "fine") {
		t.Fatalf("commands stay refused after the leak is gone: %+v", ok)
	}
}

// A leak the rollback cannot put back (the file is a symbolic link, which is never
// replaced or written through) keeps every later command refused until it is no
// longer reachable, and is never absorbed into the next call's baseline.
func TestW21UnrestorableLeakRefusesEveryLaterCommand(t *testing.T) {
	f := shellCommitFixture(t, true)
	head := headOf(t, f.root)
	write(t, f.root, "sidecar.txt", head+"\n")
	link := filepath.Join(gitCommon(t, f.root), "SIDELINK")
	if err := os.Symlink(filepath.Join(f.root, "sidecar.txt"), link); err != nil {
		t.Fatal(err)
	}
	write(t, f.root, "park.sh", parkLeak+"echo $c > sidecar.txt\n")
	res := shell(f, "sh park.sh")
	if !res.IsError || !res.Blocked || res.Rule != string(policy.RuleSecretPath) || !strings.Contains(res.Text, "MANUAL INTERVENTION") {
		t.Fatalf("a leak behind a symlink was not reported for manual repair: %+v", res)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the user's symlink was replaced: %v %v", fi, err)
	}
	remote := bareRemote(t)
	for i := 0; i < 2; i++ {
		push := shell(f, "git push -q "+remote+" SIDELINK:refs/heads/leaked")
		if !push.IsError || !strings.Contains(push.Text, "ref snapshot") {
			t.Fatalf("call %d: a command ran beside a leak that could not be put back: %+v", i+2, push)
		}
	}
	requireNothingArrived(t, remote)
	// The operator puts the file back by hand, and commands run again.
	write(t, f.root, "sidecar.txt", head+"\n")
	if ok := shell(f, "echo fine"); ok.IsError || !strings.Contains(ok.Text, "fine") {
		t.Fatalf("commands stay refused after the leak is gone: %+v", ok)
	}
}

// A symlinked directory in the git dir is common (`.git/hooks -> ../.githooks`):
// it is followed, a link cycle does not hang the walk, and neither refuses exec.
func TestW21DirectorySymlinksInTheGitDirAreFollowedWithoutRefusing(t *testing.T) {
	f := shellCommitFixture(t, false)
	common := gitCommon(t, f.root)
	hooks := filepath.Join(f.root, ".githooks")
	if err := os.MkdirAll(hooks, 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, hooks, "pre-commit", "#!/bin/sh\nexit 0\n")
	if err := os.RemoveAll(filepath.Join(common, "hooks")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hooks, filepath.Join(common, "hooks")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(common, filepath.Join(common, "cycle")); err != nil {
		t.Fatal(err)
	}
	if res := shell(f, "echo fine"); res.IsError || !strings.Contains(res.Text, "fine") {
		t.Fatalf("a symlinked hooks directory or a link cycle refused exec: %+v", res)
	}
}
