package devcouncil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// upstreamWithCommits builds a repository of n clean commits on main with
// `git fast-import` (n separate `git commit`s would take minutes) and returns
// its path. Nothing in it is a secret.
func upstreamWithCommits(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "h@test")
	run(t, dir, "config", "user.name", "h")
	var in strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&in, "commit refs/heads/main\nmark :%d\ncommitter h <h@test> %d +0000\ndata 2\nc\n", i, 1700000000+i)
		if i > 1 {
			fmt.Fprintf(&in, "from :%d\n", i-1)
		}
		in.WriteString("M 100644 inline f.txt\ndata 2\nx\n\n")
	}
	cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "fast-import", "--quiet") // #nosec G204 -- the binary is the literal "git"; the arguments are this test's own.
	cmd.Stdin = strings.NewReader(in.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v\n%s", err, out)
	}
	return dir
}

// requireUnscanned asserts the call was blocked as an unscanned set: the
// unscanned rule, hard, and never the credential rule or its wording.
func requireUnscanned(t *testing.T, res shellResult, ref string) {
	t.Helper()
	if !res.IsError || !res.Blocked || res.Rule != ruleRefUnscanned || !strings.Contains(res.Text, `"severity":"hard"`) {
		t.Fatalf("want a blocked %s result, got %+v", ruleRefUnscanned, res)
	}
	if res.Rule == string(policy.RuleSecretPath) || strings.Contains(res.Text, string(policy.RuleSecretPath)) {
		t.Errorf("an unscanned set is labelled path.secret: %s", res.Text)
	}
	if strings.Contains(res.Text, "carry a credential") || strings.Contains(res.Text, "carries a credential") {
		t.Errorf("an unscanned set is described as a credential leak: %s", res.Text)
	}
	if !strings.Contains(res.Text, "could not scan") || !strings.Contains(res.Text, ref) {
		t.Errorf("the result does not say what could not be scanned, or where: %s", res.Text)
	}
}

// A fetch that brings in more than maxGuardCommits clean commits is blocked as
// unscanned, not reported as a credential leak.
func TestOverCapFetchIsUnscannedNotACredentialLeak(t *testing.T) {
	f := shellCommitFixture(t, false)
	run(t, f.root, "remote", "add", "up", upstreamWithCommits(t, maxGuardCommits+5))
	res := shell(f, "git fetch -q up")
	requireUnscanned(t, res, "refs/remotes/up/main")
	if !strings.Contains(res.Text, fmt.Sprint(maxGuardCommits)) {
		t.Errorf("the cap is not named: %s", res.Text)
	}
}

// The same set, arriving between calls, is reported once as unscanned and does
// not roll anything back.
func TestOverCapBetweenCallsIsUnscannedNotACredentialLeak(t *testing.T) {
	f := shellCommitFixture(t, false)
	run(t, f.root, "remote", "add", "up", upstreamWithCommits(t, maxGuardCommits+5))
	if res := shell(f, "echo one"); res.IsError {
		t.Fatalf("a plain call failed: %+v", res)
	}
	run(t, f.root, "fetch", "-q", "up")
	tip := gitOut(t, f.root, "rev-parse", "refs/remotes/up/main")

	res := shell(f, "echo two")
	requireUnscanned(t, res, "refs/remotes/up/main")
	if !strings.Contains(res.Text, "between agent calls") || !strings.Contains(res.Text, "two") {
		t.Errorf("not reported as a between-call result, or the command did not run: %s", res.Text)
	}
	if got := gitOut(t, f.root, "rev-parse", "refs/remotes/up/main"); got != tip {
		t.Fatalf("the operator's ref was rewritten: %s, want %s", got, tip)
	}
	if again := shell(f, "echo three"); again.IsError {
		t.Fatalf("the same unscanned set was reported twice: %+v", again)
	}
}

// A fetch under the cap of clean commits is still silent.
func TestUnderCapFetchOfCleanCommitsIsSilent(t *testing.T) {
	f := shellCommitFixture(t, false)
	run(t, f.root, "remote", "add", "up", upstreamWithCommits(t, 5))
	if res := shell(f, "git fetch -q up"); res.IsError || res.Blocked {
		t.Fatalf("a clean fetch was refused: %+v", res)
	}
}

// A real leak keeps the credential rule, and a leak beside an over-cap ref is
// still a leak: the finding wins the label.
func TestLeakBesideAnOverCapRefStaysACredentialLeak(t *testing.T) {
	f := shellCommitFixture(t, true)
	run(t, f.root, "remote", "add", "up", upstreamWithCommits(t, maxGuardCommits+5))
	headBefore := headOf(t, f.root)
	res := shell(f, "git fetch -q up && git commit -qm leak")
	if !res.IsError || !res.Blocked || res.Rule != string(policy.RuleSecretPath) {
		t.Fatalf("a leak beside an unscanned ref lost its label: %+v", res)
	}
	if got := headOf(t, f.root); got != headBefore {
		t.Fatalf("the leaking commit was not rolled back: %s -> %s", headBefore, got)
	}
}

// A linked worktree whose directory exists but is no longer a repository does
// not refuse exec_command, and a leak in the main worktree is still caught.
func TestLinkedWorktreeThatIsNoLongerAWorkingTreeDoesNotRefuseExec(t *testing.T) {
	f := shellCommitFixture(t, true)
	wt := filepath.Join(filepath.Dir(f.root), "wt-gone")
	run(t, f.root, "worktree", "add", "-q", "--detach", wt)
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o750); err != nil {
		t.Fatal(err)
	}

	snap, err := f.reg.snapshotRefs(t.Context())
	if err != nil || snap == nil {
		t.Fatalf("snapshot with a broken linked worktree: %v", err)
	}
	// Its own refs are read through its admin directory, which needs no working
	// tree, so the worktree is scanned rather than skipped.
	if len(snap.scanned) != 1 || len(snap.skipped) != 0 {
		t.Errorf("the worktree's admin directory was not scanned: scanned=%v skipped=%v", snap.scanned, snap.skipped)
	}

	headBefore := headOf(t, f.root)
	res := shell(f, "git commit -qm leak")
	if res.Rule != string(policy.RuleSecretPath) || !res.Blocked {
		t.Fatalf("the leak in the main worktree was not caught beside a broken worktree: %+v", res)
	}
	if got := headOf(t, f.root); got != headBefore {
		t.Fatalf("not rolled back: %s -> %s", headBefore, got)
	}
	if ok := shell(f, "echo fine"); ok.IsError || !strings.Contains(ok.Text, "fine") {
		t.Fatalf("exec was refused beside a broken worktree: %+v", ok)
	}
}

// The same with a `.git` file deleted inside an otherwise real directory.
func TestLinkedWorktreeWithoutItsGitFileDoesNotRefuseExec(t *testing.T) {
	f := shellCommitFixture(t, false)
	wt := filepath.Join(filepath.Dir(f.root), "wt-nogit")
	run(t, f.root, "worktree", "add", "-q", "--detach", wt)
	if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
		t.Fatal(err)
	}
	if res := shell(f, "echo fine"); res.IsError || !strings.Contains(res.Text, "fine") {
		t.Fatalf("exec was refused: %+v", res)
	}
}

// A command that breaks a linked worktree's admin directory (so git can no
// longer read the refs in it) is blocked as unscanned, not allowed: the refs the
// guard read before it cannot be read after it.
func TestWorktreeAdminDirBrokenByTheCommandIsBlockedUnscanned(t *testing.T) {
	f := shellCommitFixture(t, false)
	wt := filepath.Join(filepath.Dir(f.root), "wt-hidden")
	run(t, f.root, "worktree", "add", "-q", "--detach", wt)
	admin := filepath.Join(gitOut(t, f.root, "rev-parse", "--path-format=absolute", "--git-common-dir"), "worktrees", "wt-hidden")
	write(t, f.root, "hide.sh", "#!/bin/sh\nrm -f "+filepath.Join(admin, "HEAD")+"\n")
	res := shell(f, "sh hide.sh")
	if !res.IsError || (res.Rule != ruleRefUnscanned && !strings.Contains(res.Text, "snapshot")) {
		t.Fatalf("a broken admin directory was allowed: %+v", res)
	}
	if res.Rule == string(policy.RuleSecretPath) {
		t.Errorf("an unreadable worktree is labelled path.secret: %s", res.Text)
	}
}

// A worktree git reports as prunable with its directory gone is read through
// its admin directory like any other, and the decision to skip one whose refs
// git cannot read is made only for that state.
func TestPrunableAndGoneIsTheOnlySkip(t *testing.T) {
	admin := t.TempDir()
	write(t, admin, "gitdir", "/gone/wt/.git\n")
	list := "worktree /repo\nHEAD 1\nbranch refs/heads/main\n\nworktree /gone/wt\nHEAD 2\ndetached\nprunable gitdir file points to non-existent location\n"
	if why := prunableAndGone(parseWorktrees(list), admin); why == "" {
		t.Fatal("a prunable worktree whose directory is gone was not skippable")
	}
	live := "worktree /repo\nHEAD 1\nbranch refs/heads/main\n\nworktree /gone/wt\nHEAD 2\ndetached\n"
	if why := prunableAndGone(parseWorktrees(live), admin); why != "" {
		t.Fatalf("an unreadable admin directory of a worktree git does not call prunable was skippable: %s", why)
	}
	here := t.TempDir()
	write(t, admin, "gitdir", filepath.Join(here, ".git")+"\n")
	existing := "worktree " + here + "\nHEAD 2\ndetached\nprunable x\n"
	if why := prunableAndGone(parseWorktrees(existing), admin); why != "" {
		t.Fatalf("a prunable worktree whose directory exists was skippable: %s", why)
	}
}

// A linked worktree whose .git is garbage no longer refuses exec: its refs are
// read through the admin directory.
func TestLinkedWorktreeWithAGarbageGitFileDoesNotRefuseExec(t *testing.T) {
	f := shellCommitFixture(t, false)
	wt := filepath.Join(filepath.Dir(f.root), "wt-garbage")
	run(t, f.root, "worktree", "add", "-q", "--detach", wt)
	write(t, wt, ".git", "not a gitdir line\n")
	if res := shell(f, "echo fine"); res.IsError || !strings.Contains(res.Text, "fine") {
		t.Fatalf("exec was refused: %+v", res)
	}
}

// The fail-open the adversary found, asserted for what the guard does: a leaking
// commit parked in a per-worktree namespace of a linked worktree that has no
// usable working tree, through its admin directory (`git --git-dir` needs no
// working tree), is found, rolled back through the same --git-dir, and blocked
// as path.secret.
func TestPerWorktreeRefParkedThroughTheAdminDirOfAnUnusableWorktreeIsRolledBack(t *testing.T) {
	for _, tc := range []struct {
		name, ref string
		outside   bool
		hide      func(t *testing.T, wt string)
	}{
		{"nogit_worktree", "refs/worktree/leak", false, func(t *testing.T, wt string) { removeFile(t, filepath.Join(wt, ".git")) }},
		{"nogit_bisect", "refs/bisect/bad", false, func(t *testing.T, wt string) { removeFile(t, filepath.Join(wt, ".git")) }},
		{"dir_missing", "refs/worktree/leak", false, func(t *testing.T, wt string) {
			if err := os.Rename(wt, wt+".away"); err != nil {
				t.Fatal(err)
			}
		}},
		{"outside_root_nogit", "refs/worktree/leak", true, func(t *testing.T, wt string) { removeFile(t, filepath.Join(wt, ".git")) }},
		{"garbage_gitfile", "refs/worktree/leak", false, func(t *testing.T, wt string) { write(t, wt, ".git", "not a gitdir line\n") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shellCommitFixture(t, true)
			base := f.root
			if tc.outside {
				base = filepath.Dir(f.root)
			}
			wt := filepath.Join(base, "wt-park-"+tc.name)
			run(t, f.root, "worktree", "add", "-q", "--detach", wt)
			admin := filepath.Join(gitOut(t, f.root, "rev-parse", "--path-format=absolute", "--git-common-dir"), "worktrees", filepath.Base(wt))
			tc.hide(t, wt)
			write(t, f.root, "park.sh", "set -e\nc=$(git commit-tree $(git write-tree) -m leak)\ngit --git-dir="+admin+" update-ref "+tc.ref+" $c\n")
			res := shell(f, "sh park.sh")
			if !res.IsError || !res.Blocked || res.Rule != string(policy.RuleSecretPath) {
				t.Fatalf("the parked leak was allowed or mislabelled: %+v", res)
			}
			if strings.Contains(res.Text, "AUDITsecretVALUE") {
				t.Errorf("the result carries the value: %s", res.Text)
			}
			if out := gitOut(t, admin, "for-each-ref", tc.ref); out != "" {
				t.Fatalf("the parked ref was not rolled back: %s", out)
			}
		})
	}
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
