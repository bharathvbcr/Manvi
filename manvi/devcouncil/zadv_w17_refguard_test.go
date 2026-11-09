package devcouncil

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Adversarial regression tests for the ref guard's narrowing (card ft-cab98c76).
// They use the literal "ref.unscanned" and "path.secret" so the same file also
// compiles against the base commit, to compare behaviour.

const (
	advRuleUnscanned = "ref.unscanned"
	advRuleSecret    = "path.secret"
)

// advBlocked asserts the GUARD (not the command policy) stopped the call: a
// path.secret or ref.unscanned block, or the refusal to run without a
// recorded ref snapshot. A policy refusal (path.outside_root and friends) means
// the attack never reached the guard and is a bad test, so it fails.
func advBlocked(t *testing.T, res shellResult, what string) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("%s: the guard let it through: %+v", what, res)
	}
	guard := res.Blocked && (res.Rule == advRuleSecret || res.Rule == advRuleUnscanned)
	refused := strings.Contains(res.Text, "ref snapshot")
	if !guard && !refused {
		t.Fatalf("%s: stopped by something other than the guard (rule %q): %s", what, res.Rule, res.Text)
	}
	if strings.Contains(res.Text, "AUDITsecretVALUE") {
		t.Errorf("%s: the result carries the secret value: %s", what, res.Text)
	}
}

// advScript writes a shell script into the root and returns the command that
// runs it, so the policy gate sees one plain command and the guard is what is
// under test.
func advScript(t *testing.T, f *fixture, body string) string {
	t.Helper()
	write(t, f.root, "adv.sh", "set -e\n"+body+"\n")
	return "sh adv.sh"
}

// advParked asserts the leak was actually parked in the worktree's ref (so the
// attack ran) and, if it was, that the guard stopped the call. A parked leak
// beside an allowed call is a FAIL-OPEN.
func advParked(t *testing.T, admin, ref string, res shellResult) {
	t.Helper()
	out := gitOut(t, admin, "for-each-ref", ref)
	if !strings.Contains(out, ref) {
		t.Fatalf("bad test: the attack did not run, %s is absent (call: %+v)", ref, res)
	}
	if !res.IsError {
		t.Fatalf("FAIL-OPEN: a leaking commit sits in %s of an unreadable linked worktree and the guard allowed the call: %+v", ref, res)
	}
	advBlocked(t, res, "parked leak")
}

func advAddWorktree(t *testing.T, f *fixture, name string) (wt, admin string) {
	t.Helper()
	wt = filepath.Join(f.root, "wts", name)
	run(t, f.root, "worktree", "add", "-q", "--detach", wt)
	common := gitOut(t, f.root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	return wt, filepath.Join(common, "worktrees", name)
}

// leakObject is a shell fragment that builds a commit holding the staged leak
// without moving any shared ref and leaves its id in $c.
const leakObject = "c=$(git commit-tree $(git write-tree) -m leak)"

// A leak committed on the main ref in the same command that moves a linked
// worktree away is still a credential finding, not just an unscanned worktree.
func TestZadvW17LeakBesideAWorktreeMovedAwayIsStillPathSecret(t *testing.T) {
	f := shellCommitFixture(t, true)
	wt, _ := advAddWorktree(t, f, "wt-adv-away")
	head := headOf(t, f.root)
	res := shell(f, advScript(t, f, "git commit -qm leak && mv "+wt+" "+wt+".away"))
	advBlocked(t, res, "leak + worktree moved away")
	if res.Rule != advRuleSecret {
		t.Fatalf("a leak beside an unreadable worktree lost its label: %q", res.Rule)
	}
	if got := headOf(t, f.root); got != head {
		t.Fatalf("not rolled back: %s -> %s", head, got)
	}
}

// A worktree removed during the command, with a leak on main in the same
// command, is still caught.
func TestZadvW17LeakWhileAWorktreeIsRemovedIsCaught(t *testing.T) {
	f := shellCommitFixture(t, true)
	wt, _ := advAddWorktree(t, f, "wt-adv-rm")
	head := headOf(t, f.root)
	res := shell(f, advScript(t, f, "git commit -qm leak && git worktree remove --force "+wt))
	advBlocked(t, res, "leak + worktree removed")
	if got := headOf(t, f.root); got != head {
		t.Fatalf("not rolled back: %s -> %s", head, got)
	}
}

// A worktree created by the command and committed in (its detached HEAD is a
// ref that did not exist before) is caught.
func TestZadvW17LeakCommittedInAWorktreeAddedDuringTheCommand(t *testing.T) {
	f := shellCommitFixture(t, false)
	wt := filepath.Join(filepath.Dir(f.root), "wt-adv-new")
	res := shell(f, advScript(t, f, "git worktree add -q --detach "+wt+" && cp notes.txt "+wt+"/n.txt && git -C "+wt+" add n.txt && git -C "+wt+" commit -qm leak"))
	advBlocked(t, res, "leak in a worktree created by the command")
}

// The guard's own two-step: a worktree made unreadable in one call, then a leak
// parked in that worktree's per-worktree namespace in the next, through the
// worktree's administrative directory (git --git-dir needs no working tree).
// The old code refused every exec while the .git file was missing; the new one
// skips the worktree, so this must still be caught or reported.
func TestZadvW17PerWorktreeRefMovedWhileWorktreeUnreadable(t *testing.T) {
	for _, tc := range []struct {
		name, ref string
		hide      func(t *testing.T, wt string)
	}{
		{"nogit_worktree", "refs/worktree/leak", func(t *testing.T, wt string) {
			if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
				t.Fatal(err)
			}
		}},
		{"nogit_bisect", "refs/bisect/bad", func(t *testing.T, wt string) {
			if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
				t.Fatal(err)
			}
		}},
		{"dir_missing", "refs/worktree/leak", func(t *testing.T, wt string) {
			if err := os.Rename(wt, wt+".away"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shellCommitFixture(t, true)
			wt, admin := advAddWorktree(t, f, "wt-adv-"+tc.name)
			// The worktree is made unreadable by the operator, outside any guarded call.
			tc.hide(t, wt)
			cmd := leakObject + " && git --git-dir=" + admin + " update-ref " + tc.ref + " $c"
			res := shell(f, advScript(t, f, cmd))
			advParked(t, admin, tc.ref, res)
		})
	}
}

// The same hiding, done by the agent's own earlier call: call 1 makes the
// worktree unreadable (reported), call 2 parks the leak. Call 2 must not be a
// silent pass.
func TestZadvW17TwoStepHideThenParkLeak(t *testing.T) {
	f := shellCommitFixture(t, true)
	wt, admin := advAddWorktree(t, f, "wt-adv-twostep")
	first := shell(f, advScript(t, f, "rm -f "+filepath.Join(wt, ".git")))
	advBlocked(t, first, "step 1 (worktree hidden by the command)")
	res := shell(f, advScript(t, f, leakObject+" && git --git-dir="+admin+" update-ref refs/worktree/leak $c"))
	advParked(t, admin, "refs/worktree/leak", res)
}

// A detached HEAD of a worktree that is skipped is read from `worktree list`,
// not from the directory, so a leak parked there is still seen.
func TestZadvW17DetachedHeadOfAPrunableWorktreeIsStillSeen(t *testing.T) {
	f := shellCommitFixture(t, true)
	wt, admin := advAddWorktree(t, f, "wt-adv-head")
	if err := os.Rename(wt, wt+".away"); err != nil {
		t.Fatal(err)
	}
	res := shell(f, advScript(t, f, leakObject+" && git --git-dir="+admin+" update-ref --no-deref HEAD $c"))
	advBlocked(t, res, "FAIL-OPEN")
}

// Before unreadable, after readable: the worktree's refs appear as new and are
// scanned.
func TestZadvW17WorktreeUnreadableBeforeReadableAfterIsScanned(t *testing.T) {
	f := shellCommitFixture(t, true)
	wt, admin := advAddWorktree(t, f, "wt-adv-back")
	if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
		t.Fatal(err)
	}
	cmd := leakObject + " && git --git-dir=" + admin + " update-ref refs/worktree/leak $c && printf 'gitdir: %s\\n' " + admin + " > " + filepath.Join(wt, ".git")
	res := shell(f, advScript(t, f, cmd))
	advBlocked(t, res, "worktree repaired and holding a leak")
}

// Mixed set: a ref whose commits cannot be scanned (its tree object is
// missing) and a real leak in one command stays path.secret and is rolled back.
func TestZadvW17UnscannableRefBesideALeakIsPathSecret(t *testing.T) {
	f := shellCommitFixture(t, true)
	head := headOf(t, f.root)
	broken := `b=$(printf 'tree 1111111111111111111111111111111111111111\nauthor a <a@a> 1 +0000\ncommitter a <a@a> 1 +0000\n\nbroken\n' | git hash-object -t commit -w --stdin --literally) && git update-ref refs/heads/zbroken $b`
	res := shell(f, advScript(t, f, broken+" && git commit -qm leak"))
	advBlocked(t, res, "unscannable ref beside a leak")
	if res.Rule != advRuleSecret {
		t.Fatalf("the finding lost to the unscanned ref: rule %q\n%s", res.Rule, res.Text)
	}
	if got := headOf(t, f.root); got != head {
		t.Fatalf("not rolled back: %s -> %s", head, got)
	}
}

// The unscannable ref alone is ref.unscanned, hard, and blocks.
func TestZadvW17UnscannableRefAloneIsUnscannedAndBlocks(t *testing.T) {
	f := shellCommitFixture(t, false)
	broken := `b=$(printf 'tree 1111111111111111111111111111111111111111\nauthor a <a@a> 1 +0000\ncommitter a <a@a> 1 +0000\n\nbroken\n' | git hash-object -t commit -w --stdin --literally) && git update-ref refs/heads/zbroken $b`
	res := shell(f, advScript(t, f, broken))
	advBlocked(t, res, "unscannable ref")
	if res.Rule != advRuleUnscanned || !strings.Contains(res.Text, `"severity":"hard"`) {
		t.Fatalf("want hard %s, got rule %q: %s", advRuleUnscanned, res.Rule, res.Text)
	}
}

// A leaking commit padded over the cap (a branch of maxGuardCommits+5 commits
// whose tip carries the leak) must still block and never allow.
func TestZadvW17LeakPaddedOverTheCapStillBlocks(t *testing.T) {
	f := shellCommitFixture(t, false)
	up := upstreamWithCommits(t, maxGuardCommits+5)
	write(t, up, "notes.txt", secretBody)
	run(t, up, "add", "notes.txt")
	run(t, up, "commit", "-qm", "leak")
	run(t, f.root, "remote", "add", "up", up)
	res := shell(f, "git fetch -q up")
	advBlocked(t, res, "over-cap set with a leaking tip")
	if res.Rule != advRuleUnscanned && res.Rule != advRuleSecret {
		t.Fatalf("unexpected rule %q", res.Rule)
	}
}

// A context cancelled before the after-scan must not stop it: a leak is still
// found and rolled back.
func TestZadvW17CancelledContextStillScansAndRollsBack(t *testing.T) {
	f := shellCommitFixture(t, true)
	before, err := f.reg.snapshotRefs(t.Context())
	if err != nil || before == nil {
		t.Fatalf("snapshot: %v", err)
	}
	head := headOf(t, f.root)
	run(t, f.root, "commit", "-qm", "leak")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, _ := f.reg.verifyRefs(ctx, before, "git commit")
	if v == nil || !v.leaked() {
		t.Fatalf("a cancelled context made the after-scan pass: %+v", v)
	}
	if got := headOf(t, f.root); got != head {
		t.Fatalf("not rolled back under a cancelled context: %s -> %s", head, got)
	}
}

// An empty directory `.git` (git ignores it and looks higher) is a repository
// as far as the guard is concerned, so a failing git is an error, not "none".
func TestZadvW17ReplacingDotGitWithAnEmptyDirFailsClosed(t *testing.T) {
	f := shellCommitFixture(t, true)
	res := shell(f, advScript(t, f, "git commit -qm leak && mv .git ../gitkept && mkdir .git"))
	advBlocked(t, res, "commit then .git replaced by an empty directory")
	again := shell(f, "echo should-not-run > ran.txt")
	advBlocked(t, again, "a command in a repository whose refs cannot be read")
	if _, err := os.Stat(filepath.Join(f.root, "ran.txt")); err == nil {
		t.Fatal("the command ran")
	}
}

// Removing .git during the command: the after snapshot cannot be taken and the
// call is blocked, not passed as "not a repository".
func TestZadvW17RenamingDotGitDuringTheCommandIsBlocked(t *testing.T) {
	f := shellCommitFixture(t, true)
	res := shell(f, advScript(t, f, "git commit -qm leak && mv .git ../gitkept"))
	advBlocked(t, res, "commit then .git renamed away")
}

// A decoy bare-repository shape in the root (HEAD, objects, refs) while .git is
// gone is a "repository", so the failing/empty after-state is not silently clean.
func TestZadvW17BareShapedDecoyDoesNotMakeAMovedGitDirInvisible(t *testing.T) {
	f := shellCommitFixture(t, true)
	res := shell(f, advScript(t, f, "git commit -qm leak && mv .git ../gitkept && mkdir objects refs && echo garbage > HEAD"))
	advBlocked(t, res, "commit then .git replaced by a decoy bare shape")
}

// Environment variables that redirect git in the command do not matter to a
// leak committed into this repository.
func TestZadvW17GitDirEnvAndDashDashGitDirCommitsAreCaught(t *testing.T) {
	for name, cmd := range map[string]string{
		"env":      "GIT_DIR=$PWD/.git GIT_WORK_TREE=$PWD git commit -qm leak",
		"coreWT":   "git -c core.worktree=$PWD commit -qm leak",
		"commonEV": "GIT_COMMON_DIR=$PWD/.git git commit -qm leak",
	} {
		t.Run(name, func(t *testing.T) {
			f := shellCommitFixture(t, true)
			head := headOf(t, f.root)
			res := shell(f, cmd)
			advBlocked(t, res, name)
			if got := headOf(t, f.root); got != head {
				t.Fatalf("not rolled back: %s -> %s", head, got)
			}
		})
	}
}

// A root whose .git is a file pointing elsewhere (separate git dir) is guarded.
func TestZadvW17GitFilePointingElsewhereIsStillGuarded(t *testing.T) {
	f := shellCommitFixture(t, true)
	sep := filepath.Join(filepath.Dir(f.root), "sep.git")
	if err := os.Rename(filepath.Join(f.root, ".git"), sep); err != nil {
		t.Fatal(err)
	}
	write(t, f.root, ".git", "gitdir: "+sep+"\n")
	run(t, f.root, "config", "core.worktree", f.root)
	head := headOf(t, f.root)
	res := shell(f, "git commit -qm leak")
	advBlocked(t, res, "leak in a separate-git-dir repository")
	if got := headOf(t, f.root); got != head {
		t.Fatalf("not rolled back: %s -> %s", head, got)
	}
}

// A worktree whose .git is garbage (present but unusable) must never let a leak
// through: it may refuse the command, but it may not allow a leak.
func TestZadvW17GarbageGitFileInALinkedWorktreeNeverAllowsALeak(t *testing.T) {
	f := shellCommitFixture(t, true)
	wt, _ := advAddWorktree(t, f, "wt-adv-garbage")
	write(t, wt, ".git", "not a gitdir line\n")
	head := headOf(t, f.root)
	res := shell(f, "git commit -qm leak")
	advBlocked(t, res, "leak beside a worktree with a garbage .git")
	if got := headOf(t, f.root); got != head {
		t.Fatalf("a leaking commit stayed: %s -> %s", head, got)
	}
}

// A symlink in place of a linked worktree's .git, pointing at the main
// repository's git dir, must not let a leak through either.
func TestZadvW17SymlinkedGitInALinkedWorktreeNeverAllowsALeak(t *testing.T) {
	f := shellCommitFixture(t, true)
	wt, _ := advAddWorktree(t, f, "wt-adv-symlink")
	if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.root, ".git"), filepath.Join(wt, ".git")); err != nil {
		t.Skipf("no symlinks: %v", err)
	}
	head := headOf(t, f.root)
	res := shell(f, "git commit -qm leak")
	advBlocked(t, res, "leak beside a worktree with a symlinked .git")
	if got := headOf(t, f.root); got != head {
		t.Fatalf("a leaking commit stayed: %s -> %s", head, got)
	}
}
