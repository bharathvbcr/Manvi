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
	// git prints the worktree's real path (/private/var on macOS), not the
	// spelling the test built, so the record is found by its directory name.
	recorded := false
	for path, why := range snap.skipped {
		recorded = recorded || (filepath.Base(path) == filepath.Base(wt) && why != "")
	}
	if !recorded {
		t.Errorf("the skipped worktree is not recorded: %+v", snap.skipped)
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

// A worktree the guard read before the command and cannot read after it is
// reported as unscanned: its own refs could have moved where nothing sees them.
func TestWorktreeMadeUnreadableByTheCommandIsReportedUnscanned(t *testing.T) {
	f := shellCommitFixture(t, false)
	wt := filepath.Join(filepath.Dir(f.root), "wt-hidden")
	run(t, f.root, "worktree", "add", "-q", "--detach", wt)
	write(t, f.root, "hide.sh", "#!/bin/sh\nrm -f "+filepath.Join(wt, ".git")+"\n")
	res := shell(f, "sh hide.sh")
	requireUnscanned(t, res, wt)
}
