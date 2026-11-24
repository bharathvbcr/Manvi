package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// Cards ft-1ba663b7 and ft-1eb121d4: a ref git can resolve from the root must be
// in the guard's snapshot, or the command is refused.

// bareRemote makes an empty bare repository and returns its path.
func bareRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "--bare", "-b", "main")
	return dir
}

// requireNothingArrived asserts the bare remote holds no ref and no object that
// is not its own: a push that delivered the leak would add a ref.
func requireNothingArrived(t *testing.T, remote string) {
	t.Helper()
	if out := gitOut(t, remote, "for-each-ref"); out != "" {
		t.Fatalf("a ref arrived at the remote: %s", out)
	}
}

// linkedRootFixture is a dev-posture checkout whose Registry root is a linked
// worktree (every GitPulse and Manvi lane), holding its own staged leak.
func linkedRootFixture(t *testing.T, name string) (f *fixture, wt string) {
	t.Helper()
	f = shellCommitFixture(t, true)
	wt = filepath.Join(filepath.Dir(f.root), name)
	run(t, f.root, "worktree", "add", "-q", "--detach", wt)
	write(t, wt, ".env", secretBody)
	write(t, wt, "notes.txt", secretBody)
	run(t, wt, "add", "notes.txt")
	f.reg.deps.Root = wt
	return f, wt
}

// From a linked-worktree root, a leaking commit parked in the MAIN worktree's
// own per-worktree refs (`main-worktree/refs/<ns>/x`) is rolled back and blocked
// as path.secret, and a later push of that name delivers nothing.
func TestW21MainWorktreePerWorktreeRefsFromALinkedRootAreRolledBack(t *testing.T) {
	for _, ns := range []string{"bisect", "worktree", "rewritten"} {
		t.Run(ns, func(t *testing.T) {
			f, wt := linkedRootFixture(t, "wt-w21-"+ns)
			main := f.root // the fixture root stays the main worktree
			remote := bareRemote(t)
			ref := "refs/" + ns + "/x"
			write(t, wt, "park.sh", "set -e\nc=$(git commit-tree $(git write-tree) -m leak)\ngit update-ref main-worktree/"+ref+" $c\n")
			res := shell(f, "sh park.sh")
			if !res.IsError || !res.Blocked || res.Rule != string(policy.RuleSecretPath) {
				t.Fatalf("FAIL-OPEN: a leak parked in main-worktree/%s was allowed or mislabelled: %+v", ref, res)
			}
			if strings.Contains(res.Text, "AUDITsecretVALUE") {
				t.Errorf("the result carries the value: %s", res.Text)
			}
			if out := gitOut(t, main, "for-each-ref", ref); out != "" {
				t.Fatalf("the parked ref was not rolled back: %s", out)
			}
			push := shell(f, "git push -q "+remote+" main-worktree/"+ref+":refs/heads/leaked")
			if !push.IsError {
				t.Errorf("the push of a rolled-back ref succeeded: %+v", push)
			}
			requireNothingArrived(t, remote)
		})
	}
}

// The main worktree's refs read before and after are one entry each, not two,
// and a command that does not touch them is not blocked.
func TestW21LinkedRootSnapshotCarriesTheMainWorktreesPerWorktreeRefs(t *testing.T) {
	f, _ := linkedRootFixture(t, "wt-w21-snap")
	run(t, f.root, "update-ref", "refs/bisect/seen", headOf(t, f.root))
	snap, err := f.reg.snapshotRefs(t.Context())
	if err != nil || snap == nil {
		t.Fatalf("snapshot: %v", err)
	}
	n := 0
	for name := range snap.refs {
		if strings.HasSuffix(name, ":refs/bisect/seen") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the main worktree's refs/bisect/seen is in the snapshot %d times, want once: %v", n, snap.refs)
	}
	if res := shell(f, "echo fine"); res.IsError || !strings.Contains(res.Text, "fine") {
		t.Fatalf("a clean call beside main-worktree refs was refused: %+v", res)
	}
}

// danglingAdminScript builds the exact shape of card ft-1eb121d4: an admin dir
// with commondir and refs/worktree/leak, no HEAD, and a gitdir that points
// nowhere.
func danglingAdminScript(common string) string {
	return "set -e\n" +
		"c=$(git commit-tree $(git write-tree) -m leak)\n" +
		"a=" + filepath.ToSlash(filepath.Join(common, "worktrees", "evil")) + "\n" +
		"mkdir -p $a/refs/worktree\n" +
		"echo ../.. > $a/commondir\n" +
		"echo /nonexistent/evil/.git > $a/gitdir\n" +
		"echo $c > $a/refs/worktree/leak\n"
}

func gitCommon(t *testing.T, root string) string {
	t.Helper()
	return gitOut(t, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
}

// An admin dir the command creates (absent from the before snapshot) with
// commondir and refs but no HEAD is a ref source git resolves
// (`git rev-parse worktrees/evil/refs/worktree/leak`). The command is blocked as
// ref.unscanned, and every later command is refused, so the push never runs.
func TestW21CommandCreatedDanglingAdminDirIsRefusedUnscanned(t *testing.T) {
	f := shellCommitFixture(t, true)
	remote := bareRemote(t)
	write(t, f.root, "mk.sh", danglingAdminScript(gitCommon(t, f.root)))
	res := shell(f, "sh mk.sh")
	if !res.IsError || !res.Blocked || res.Rule != ruleRefUnscanned {
		t.Fatalf("FAIL-OPEN: a command-created dangling admin dir holding a ref was allowed: %+v", res)
	}
	if !strings.Contains(res.Text, "evil") {
		t.Errorf("the result does not name the admin directory: %s", res.Text)
	}
	// The shape is real: git resolves the ref through the dangling dir.
	if got := gitOut(t, f.root, "rev-parse", "worktrees/evil/refs/worktree/leak"); len(got) < 40 {
		t.Fatalf("bad test: git does not resolve the ref: %q", got)
	}
	push := shell(f, "git push -q "+remote+" worktrees/evil/refs/worktree/leak:refs/heads/leaked")
	if !push.IsError || !strings.Contains(push.Text, "ref snapshot") {
		t.Fatalf("a command ran beside an unscanned ref source: %+v", push)
	}
	requireNothingArrived(t, remote)
}

// The same dir present before the command refuses it, before it runs.
func TestW21DanglingAdminDirWithRefsRefusesTheCommandBeforeItRuns(t *testing.T) {
	f := shellCommitFixture(t, true)
	common := gitCommon(t, f.root)
	a := filepath.Join(common, "worktrees", "evil")
	if err := os.MkdirAll(filepath.Join(a, "refs", "bisect"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, a, "commondir", "../..\n")
	write(t, a, "gitdir", "/nonexistent/evil/.git\n")
	write(t, filepath.Join(a, "refs", "bisect"), "bad", headOf(t, f.root)+"\n")
	res := shell(f, "echo should-not-run > ran.txt")
	if !res.IsError || !strings.Contains(res.Text, "ref snapshot") {
		t.Fatalf("a command ran beside an unreadable ref source: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(f.root, "ran.txt")); err == nil {
		t.Fatal("the command ran")
	}
}

// A stale admin dir with no ref in it (what `git worktree prune` removes) is
// not a ref source and does not refuse exec: the refusal is for a ref git can
// read, not for every dir that looks prunable.
func TestW21DanglingAdminDirWithoutRefsDoesNotRefuseExec(t *testing.T) {
	f := shellCommitFixture(t, false)
	a := filepath.Join(gitCommon(t, f.root), "worktrees", "stale")
	if err := os.MkdirAll(filepath.Join(a, "refs", "worktree"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, a, "commondir", "../..\n")
	write(t, a, "gitdir", "/nonexistent/stale/.git\n")
	if res := shell(f, "echo fine"); res.IsError || !strings.Contains(res.Text, "fine") {
		t.Fatalf("a stale admin dir without refs refused exec: %+v", res)
	}
}
