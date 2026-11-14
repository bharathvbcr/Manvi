package devcouncil

import (
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// Reviewer probes for the admin-directory holes C1, C3 (with LOW-2, the junction
// form) and C4. Each asserts that a push on the next call delivers nothing: the
// guard refuses (or rolls back) rather than leaving a ref git resolves unread.

// parkLeak (zrefguard_w21_files_test.go) builds a leaking commit in $c, a clean
// one in $h and names the common git dir $d.

func probePush(t *testing.T, f *fixture, by string) (arrived string) {
	t.Helper()
	remote := bareRemote(t)
	push := shell(f, "git push -q "+remote+" "+by+":refs/heads/leaked")
	t.Logf("push %s -> blocked=%v err=%v text=%.300s", by, push.Blocked, push.IsError, push.Text)
	return gitOut(t, remote, "for-each-ref")
}

// C1: `git update-ref worktrees/<id>/refs/heads/y` writes
// <common>/worktrees/<id>/refs/heads/y, which git resolves by that literal path
// and the admin directory's for-each-ref (refs/worktree, bisect, rewritten) does
// not list. From the root's own admin directory too.
func TestProbeWorktreesSharedPathOwnAdmin(t *testing.T) {
	f, wt := linkedRootFixture(t, "wt-probe")
	own := gitOut(t, wt, "rev-parse", "--absolute-git-dir")
	id := filepath.Base(own)
	name := "worktrees/" + id + "/refs/heads/y"
	write(t, wt, "park.sh", parkLeak+"git update-ref "+name+" $c\n")
	res := shell(f, "sh park.sh")
	t.Logf("park: blocked=%v rule=%s text=%.300s", res.Blocked, res.Rule, res.Text)
	if out := probePush(t, f, name); out != "" {
		t.Errorf("PROBE-FAILOPEN own-admin shared path %s: %s", name, out)
	}
}

func TestProbeWorktreesSharedPathSibling(t *testing.T) {
	f := shellCommitFixture(t, true)
	sib := filepath.Join(filepath.Dir(f.root), "sibprobe")
	run(t, f.root, "worktree", "add", "-q", "--detach", sib)
	name := "worktrees/sibprobe/refs/heads/y"
	write(t, f.root, "park.sh", parkLeak+"git update-ref "+name+" $c\n")
	res := shell(f, "sh park.sh")
	t.Logf("park: blocked=%v rule=%s text=%.300s", res.Blocked, res.Rule, res.Text)
	if out := probePush(t, f, name); out != "" {
		t.Errorf("PROBE-FAILOPEN sibling shared path %s: %s", name, out)
	}
}

// The same class under the other literal paths git resolves: a tag or remote ref
// in the admin directory, a nested admin directory, and main-worktree/.
func TestW21LiteralPathRefsInAdminDirsAreRefused(t *testing.T) {
	for _, tc := range []struct{ name, ref string }{
		{"tag", "worktrees/lit/refs/tags/t"},
		{"remote", "worktrees/lit/refs/remotes/o/z"},
		{"nested_admin", "worktrees/lit/worktrees/other/refs/heads/y"},
		{"main_worktree", "main-worktree/refs/heads/y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shellCommitFixture(t, true)
			sib := filepath.Join(filepath.Dir(f.root), "lit")
			run(t, f.root, "worktree", "add", "-q", "--detach", sib)
			write(t, f.root, "park.sh", parkLeak+"git update-ref "+tc.ref+" $c\n")
			res := shell(f, "sh park.sh")
			if !res.IsError || !res.Blocked {
				t.Errorf("a ref parked at %s was allowed: %+v", tc.ref, res)
			}
			if out := probePush(t, f, tc.ref); out != "" {
				t.Errorf("PROBE-FAILOPEN %s: %s", tc.ref, out)
			}
		})
	}
}

// CRITICAL-1 of the second review: main-worktree/ is a literal path under the
// common dir, and a checkout with no linked worktree has no <common>/worktrees
// at all. The ref-source walk reads the common dir in every checkout
// (refguard_rootfiles.go), so a leak parked there is blocked as path.secret and
// put back, and a later push by that name delivers nothing.
func TestW21MainWorktreeLiteralPathInASingleWorktreeCheckout(t *testing.T) {
	f := shellCommitFixture(t, true)
	write(t, f.root, "park.sh", parkLeak+"git update-ref main-worktree/refs/heads/y $c\n")
	res := shell(f, "sh park.sh")
	if !res.IsError || !res.Blocked || res.Rule != string(policy.RuleSecretPath) {
		t.Errorf("a ref parked at main-worktree/refs/heads/y in a checkout with no linked worktree was not blocked as %s: %+v", policy.RuleSecretPath, res)
	}
	if out := probePush(t, f, "main-worktree/refs/heads/y"); out != "" {
		t.Errorf("PROBE-FAILOPEN main-worktree literal path, no linked worktree: %s", out)
	}
}

// C3: a symlinked admin directory is not a directory to os.ReadDir's entry, and
// was skipped silently; git resolves worktrees/evil/ORIG_HEAD through it.
func TestProbeSymlinkedAdminDir(t *testing.T) {
	f := shellCommitFixture(t, true)
	outside := filepath.Join(t.TempDir(), "evil")
	write(t, f.root, "park.sh", parkLeak+"mkdir -p "+outside+" $d/worktrees\nln -s "+outside+" $d/worktrees/evil\necho $c > "+outside+"/ORIG_HEAD\n")
	res := shell(f, "sh park.sh")
	t.Logf("park: blocked=%v rule=%s text=%.300s", res.Blocked, res.Rule, res.Text)
	if !res.IsError || res.Rule != ruleRefUnscanned {
		t.Errorf("a symlinked admin dir was not refused as %s: %+v", ruleRefUnscanned, res)
	}
	if out := probePush(t, f, "worktrees/evil/ORIG_HEAD"); out != "" {
		t.Errorf("PROBE-FAILOPEN symlinked admin dir: %s", out)
	}
}

// C4: an admin directory with commondir and HEAD but no gitdir file is omitted by
// `git worktree list`, and git resolves worktrees/<id>/HEAD through it.
func TestProbeUnlistedAdminHEAD(t *testing.T) {
	f := shellCommitFixture(t, true)
	write(t, f.root, "park.sh", parkLeak+"mkdir -p $d/worktrees/ev2\necho ../.. > $d/worktrees/ev2/commondir\necho $c > $d/worktrees/ev2/HEAD\n")
	res := shell(f, "sh park.sh")
	t.Logf("park: blocked=%v rule=%s text=%.300s", res.Blocked, res.Rule, res.Text)
	if !res.IsError || res.Rule != ruleRefUnscanned {
		t.Errorf("an unlisted admin dir holding a HEAD was not refused as %s: %+v", ruleRefUnscanned, res)
	}
	if out := probePush(t, f, "worktrees/ev2/HEAD"); out != "" {
		t.Errorf("PROBE-FAILOPEN unlisted admin dir HEAD: %s", out)
	}
}

// The refusals are for what git cannot account for, not for every admin dir: a
// real worktree added, used and removed stays allowed.
func TestW21OrdinaryWorktreeLifecycleStaysAllowed(t *testing.T) {
	f := shellCommitFixture(t, false)
	for _, cmd := range []string{
		"git worktree add -q --detach wts-ok",
		"git worktree lock wts-ok && git worktree unlock wts-ok",
		"git worktree remove --force wts-ok",
		"git worktree prune",
	} {
		if res := shell(f, cmd); res.IsError || res.Blocked {
			t.Errorf("%q was refused: %+v", cmd, res)
		}
	}
}
