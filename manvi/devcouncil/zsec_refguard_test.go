package devcouncil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

func headOf(t *testing.T, root string) string {
	t.Helper()
	return gitOut(t, root, "rev-parse", "HEAD")
}

func gitOut(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", root}, args...)...).CombinedOutput() // #nosec G204 -- the binary is the literal "git"; the arguments are this test's own.
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// shellCommitFixture is a dev-posture checkout with a .env on disk, one clean
// file (app.js) staged, and notes.txt, a copy of the .env value under another
// name, either staged (a leak ready to commit) or left in the working tree.
func shellCommitFixture(t *testing.T, stageLeak bool) *fixture {
	t.Helper()
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	// These tests name the branch "main". `git init` names the first branch from
	// init.defaultBranch, which verify.sh's isolated environment does not set
	// (it gets "master"), so the fixture names it rather than inherit it.
	nameBranchMain(t, f.root)
	write(t, f.root, ".env", secretBody)
	checkout(t, f)
	write(t, f.root, "app.js", "const mode = 'development';\n")
	run(t, f.root, "add", "app.js")
	write(t, f.root, "msg.txt", "from a file\n")
	write(t, f.root, "notes.txt", secretBody)
	if stageLeak {
		run(t, f.root, "add", "notes.txt")
	}
	return f
}

// nameBranchMain makes the checked-out branch "main", born or unborn.
func nameBranchMain(t *testing.T, root string) {
	t.Helper()
	// #nosec G204 -- the binary is the literal "git" and the arguments are fixed.
	if err := exec.CommandContext(t.Context(), "git", "-C", root, "rev-parse", "--verify", "-q", "HEAD").Run(); err != nil {
		run(t, root, "symbolic-ref", "HEAD", "refs/heads/main")
		return
	}
	run(t, root, "branch", "-M", "main")
}

type shellResult struct {
	Text    string
	IsError bool
	Rule    string
	Blocked bool
}

func shell(f *fixture, command string) shellResult {
	r := f.call("devcouncil_exec_command", map[string]string{"command": command})
	return shellResult{Text: r.Text, IsError: r.IsError, Rule: r.Rule, Blocked: r.Blocked}
}

// requireViolation asserts the call reported the hard violation, that its text
// carries no secret value, and that the repository is back where it started.
func requireViolation(t *testing.T, res shellResult, root, headBefore string) {
	t.Helper()
	if !res.IsError || !res.Blocked || res.Rule != string(policy.RuleSecretPath) || !strings.Contains(res.Text, `"severity":"hard"`) {
		t.Fatalf("no hard violation was reported: %+v", res)
	}
	if strings.Contains(res.Text, "AUDITsecretVALUE") {
		t.Errorf("the violation carries the value: %s", res.Text)
	}
	if !strings.Contains(res.Text, "notes.txt") || !strings.Contains(res.Text, "refs/heads/") {
		t.Errorf("the violation does not name the ref and the file: %s", res.Text)
	}
	if after := headOf(t, root); after != headBefore {
		t.Fatalf("HEAD stayed moved: %s -> %s", headBefore, after)
	}
}

func TestShellCommitOfALeakIsRolledBack(t *testing.T) {
	for _, command := range []string{
		"git commit -m x",
		"git commit --no-verify -m x",
		"git add notes.txt && git commit -m x",
		"git commit -am x",
		"git commit -m x | tail -1",
	} {
		t.Run(command, func(t *testing.T) {
			f := shellCommitFixture(t, true)
			before := headOf(t, f.root)
			res := shell(f, command)
			requireViolation(t, res, f.root, before)
			if !strings.Contains(res.Text, `"restored":true`) {
				t.Errorf("the ref was not reported restored: %s", res.Text)
			}
			if staged := gitStagedNames(t, f.root); !strings.Contains(staged, "notes.txt") {
				t.Errorf("the rollback should leave the index as it was, got %q", staged)
			}
		})
	}
}

// A clean commit is not a violation and stays, however it is spelled.
func TestShellCommitOfCleanContentStays(t *testing.T) {
	for _, command := range []string{
		"git commit -m clean",
		"git commit --no-verify -m clean",
		"git add app.js && git commit -m clean",
		"git commit -m clean | tail -1",
		`git commit -m "$(cat msg.txt)"`,
		"git commit -F msg.txt",
		"sh -c 'git commit -m clean'",
	} {
		t.Run(command, func(t *testing.T) {
			f := shellCommitFixture(t, false)
			before := headOf(t, f.root)
			res := shell(f, command)
			if res.Rule == string(policy.RuleSecretPath) || strings.Contains(res.Text, "shell_commit") {
				t.Fatalf("a clean commit was reported as a violation: %s", res.Text)
			}
			if after := headOf(t, f.root); after == before {
				t.Fatalf("a clean commit did not stay: %+v", res)
			}
		})
	}
}

// Commit-creating routes other than `git commit` are caught the same way.
func TestShellCommitRoutesBeyondCommitAreCaught(t *testing.T) {
	setup := func(t *testing.T) *fixture {
		f := shellCommitFixture(t, false)
		// A side branch whose tip leaks, made before the call, so it is already
		// in the recorded refs: only what the command newly creates is judged.
		run(t, f.root, "checkout", "-q", "-b", "side")
		run(t, f.root, "add", "notes.txt")
		run(t, f.root, "commit", "-qm", "side leaks")
		run(t, f.root, "checkout", "-q", "main")
		// main moves on, so replaying side's commit makes a commit of its own
		// instead of reusing side's tip.
		run(t, f.root, "commit", "-q", "--allow-empty", "-m", "main moves on")
		return f
	}
	cases := map[string]string{
		"cherry-pick": "git cherry-pick side",
		"rebase":      "git rebase main side",
		"commit-tree": `git update-ref refs/heads/zz "$(git commit-tree "$(git rev-parse side^{tree})" -p HEAD -m x)"`,
	}
	for name, command := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			headBefore := headOf(t, f.root)
			refs := gitOut(t, f.root, "for-each-ref", "--format=%(objectname) %(refname)")
			res := shell(f, command)
			if !res.IsError || res.Rule != string(policy.RuleSecretPath) {
				t.Fatalf("no violation: %+v", res)
			}
			if after := headOf(t, f.root); name == "cherry-pick" && after != headBefore {
				t.Errorf("HEAD moved: %s -> %s", headBefore, after)
			}
			if got := gitOut(t, f.root, "for-each-ref", "--format=%(objectname) %(refname)"); got != refs {
				t.Errorf("refs not restored:\nbefore %s\nafter  %s", refs, got)
			}
		})
	}
}

// Something that is not a commit, or a commit already reachable, is nobody's
// violation: the operator's own refs are left alone.
func TestShellRefGuardLeavesUnrelatedRefsAlone(t *testing.T) {
	f := shellCommitFixture(t, false)
	run(t, f.root, "checkout", "-q", "-b", "operator")
	run(t, f.root, "add", "notes.txt")
	run(t, f.root, "commit", "-qm", "operator's own leaking branch, made outside the call")
	run(t, f.root, "checkout", "-q", "main")
	run(t, f.root, "tag", "v1")
	run(t, f.root, "stash", "-q", "--include-untracked")
	before := gitOut(t, f.root, "for-each-ref", "--format=%(objectname) %(refname)")
	res := shell(f, "git branch harmless main && git tag v2 && echo done")
	if res.IsError {
		t.Fatalf("a harmless command was reported: %+v", res)
	}
	after := gitOut(t, f.root, "for-each-ref", "--format=%(objectname) %(refname)")
	for _, line := range strings.Split(before, "\n") {
		if !strings.Contains(after, line) {
			t.Errorf("ref %q was touched:\n%s\n--\n%s", line, before, after)
		}
	}
	for _, added := range []string{"refs/heads/harmless", "refs/tags/v2"} {
		if !strings.Contains(after, added) {
			t.Errorf("the harmless command's ref %s was undone:\n%s", added, after)
		}
	}
}

// A ref that moved again between the scan and the restore is not clobbered; the
// result says manual intervention is needed.
func TestRestoreRefDoesNotClobberAMovedRef(t *testing.T) {
	f := shellCommitFixture(t, true)
	was := headOf(t, f.root)
	run(t, f.root, "commit", "-qm", "leak")
	leak := headOf(t, f.root)
	run(t, f.root, "commit", "-q", "--allow-empty", "-m", "operator moved it on")
	moved := headOf(t, f.root)

	g := guardedRef{Ref: "refs/heads/main", Was: was, Now: leak}
	v := &refViolation{}
	f.reg.restoreRef(t.Context(), &g, v)
	if g.Restored || !v.manual || g.Problem == "" {
		t.Fatalf("a stale restore was not reported: %+v manual=%v", g, v.manual)
	}
	if got := headOf(t, f.root); got != moved {
		t.Fatalf("the operator's ref was clobbered: %s, want %s", got, moved)
	}
	v.refs = []guardedRef{g}
	out := v.withViolation(tools.Result{Text: "ran"})
	if !strings.Contains(out.Text, "MANUAL INTERVENTION NEEDED") || !strings.Contains(out.Text, leak) || !strings.Contains(out.Text, was) {
		t.Fatalf("the manual-intervention report does not carry the state: %s", out.Text)
	}
}

// A repository where the refs cannot be read is never a pass.
func TestShellRefGuardFailsClosedWhenTheSnapshotCannotBeTaken(t *testing.T) {
	f := shellCommitFixture(t, false)
	write(t, f.root, ".git/config", "[broken\n")
	res := shell(f, "echo should-not-run > ran.txt")
	if !res.IsError || !strings.Contains(res.Text, "snapshot") {
		t.Fatalf("a command ran with no ref snapshot: %+v", res)
	}
}

// A directory that is not a repository is not guarded and does not fail.
func TestShellRefGuardIgnoresANonRepository(t *testing.T) {
	f := shellCommitFixture(t, false)
	f.reg.deps.Root = t.TempDir()
	if res := shell(f, "echo hi"); res.IsError || !strings.Contains(res.Text, "hi") {
		t.Fatalf("a command in a non-repository failed: %+v", res)
	}
}

// commit-and-push in one call is a stated residual: the guard rolls back the
// local ref and says where the leak went, but cannot recall the push.
func TestShellCommitAndPushIsReportedWithItsRemote(t *testing.T) {
	f := shellCommitFixture(t, true)
	remote := t.TempDir()
	gitOut(t, remote, "init", "-q", "--bare")
	run(t, f.root, "remote", "add", "origin", remote)
	before := headOf(t, f.root)
	res := shell(f, "git commit -m x && git push origin main")
	requireViolation(t, res, f.root, before)
	if !strings.Contains(res.Text, `\"origin\"`) || !strings.Contains(res.Text, "push") {
		t.Errorf("the report does not name the remote or the push: %s", res.Text)
	}
}

// The guard's cost per exec_command in a repository with 1000 refs: the
// snapshot before and the unchanged-refs check after, against the same command
// without them. Logged, with a loose ceiling so a regression to something
// seconds-long fails.
func TestShellRefGuardLatencyWith1000Refs(t *testing.T) {
	f := shellCommitFixture(t, false)
	head := headOf(t, f.root)
	var in strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&in, "create refs/heads/bulk/b%04d %s\n", i, head)
	}
	cmd := exec.CommandContext(t.Context(), "git", "-C", f.root, "update-ref", "--stdin") // #nosec G204 -- the binary is the literal "git"; the arguments are this test's own.
	cmd.Stdin = strings.NewReader(in.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating refs: %v\n%s", err, out)
	}
	const n = 20
	ctx := t.Context()
	start := time.Now()
	for i := 0; i < n; i++ {
		snap, err := f.reg.snapshotRefs(ctx)
		if err != nil || snap == nil {
			t.Fatalf("snapshot: %v", err)
		}
		if v, _ := f.reg.verifyRefs(ctx, snap, "true"); v != nil {
			t.Fatalf("unchanged refs reported: %+v", v)
		}
	}
	perCall := time.Since(start) / n
	t.Logf("guard overhead with %d refs: %s per exec_command (snapshot + check, refs unchanged)", len(strings.Split(gitOut(t, f.root, "for-each-ref"), "\n")), perCall)
	if perCall > 500*time.Millisecond {
		t.Errorf("the guard costs %s per command", perCall)
	}
}

// A commit made outside any call (standing in for a descendant that left the
// process group and outlived the call) is reported by the next call, is not
// rolled back, and is reported once.
func TestRefChangedBetweenCallsIsReportedNotRolledBack(t *testing.T) {
	f := shellCommitFixture(t, true)
	if res := shell(f, "echo one"); res.IsError {
		t.Fatalf("a plain call failed: %+v", res)
	}
	run(t, f.root, "commit", "-qm", "leak made between calls")
	leak := headOf(t, f.root)

	res := shell(f, "echo two")
	if !res.IsError || !res.Blocked || res.Rule != string(policy.RuleSecretPath) ||
		!strings.Contains(res.Text, "ref_changed_between_calls") || !strings.Contains(res.Text, "between agent calls") ||
		!strings.Contains(res.Text, leak) || !strings.Contains(res.Text, "notes.txt") {
		t.Fatalf("the between-call leak was not reported: %+v", res)
	}
	if strings.Contains(res.Text, "AUDITsecretVALUE") || strings.Contains(res.Text, `"restored":true`) {
		t.Errorf("value shown or a rollback claimed: %s", res.Text)
	}
	if !strings.Contains(res.Text, "two") {
		t.Errorf("the command itself did not run: %s", res.Text)
	}
	if got := headOf(t, f.root); got != leak {
		t.Fatalf("the operator's ref was rewritten: %s, want %s", got, leak)
	}
	if again := shell(f, "echo three"); again.IsError {
		t.Fatalf("the same leak was reported twice: %+v", again)
	}
}

func TestCleanCommitsBetweenCallsAreNotReported(t *testing.T) {
	f := shellCommitFixture(t, false)
	shell(f, "echo one")
	run(t, f.root, "commit", "-qm", "operator's clean commit")
	before := headOf(t, f.root)
	if res := shell(f, "echo two"); res.IsError || res.Rule != "" && res.Rule != string(policy.RuleCommandNotAllowed) {
		t.Fatalf("a clean commit between calls was reported: %+v", res)
	}
	if got := headOf(t, f.root); got != before {
		t.Fatalf("HEAD moved: %s -> %s", before, got)
	}
}

// A baseline that cannot be compared (the repository was re-created) is a
// reported problem, not a silent pass.
func TestBaselineThatCannotBeComparedIsReported(t *testing.T) {
	f := shellCommitFixture(t, false)
	shell(f, "echo one")
	if err := os.RemoveAll(filepath.Join(f.root, ".git")); err != nil {
		t.Fatal(err)
	}
	run(t, f.root, "init", "-q")
	run(t, f.root, "config", "user.email", "h@test")
	run(t, f.root, "config", "user.name", "h")
	run(t, f.root, "add", "app.js")
	run(t, f.root, "commit", "-qm", "fresh history")
	res := shell(f, "echo two")
	if !res.IsError || !strings.Contains(res.Text, "could not be listed") {
		t.Fatalf("an incomparable baseline was passed silently: %+v", res)
	}
}

// Overlapping calls drop the baseline and say so on the next call.
func TestOverlappingCallsDropTheBaselineAndSayWhy(t *testing.T) {
	f := shellCommitFixture(t, false)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shell(f, "sleep 1; echo overlapped")
		}()
	}
	wg.Wait()
	r := f.call("devcouncil_exec_command", map[string]string{"command": "echo after"})
	if len(r.Degraded) == 0 || !strings.Contains(strings.Join(r.Degraded, " "), "overlapped") {
		t.Fatalf("the dropped baseline was not noted: %+v", r)
	}
}

// A commit on a detached HEAD in a linked worktree, created inside the call, is
// caught and the worktree's HEAD is put back where the new commits began.
func TestLeakOnADetachedHeadOfALinkedWorktreeIsRestored(t *testing.T) {
	f := shellCommitFixture(t, false)
	base := headOf(t, f.root)
	wt := filepath.Join(filepath.Dir(f.root), "wt-guard")
	write(t, f.root, "mk.sh", "#!/bin/sh\nset -e\ngit worktree add -q --detach "+wt+"\ncp notes.txt "+wt+"/leak.txt\ncd "+wt+"\ngit add leak.txt\ngit commit -qm leak-in-worktree\n")
	res := shell(f, "sh mk.sh")
	if !res.IsError || res.Rule != string(policy.RuleSecretPath) || !strings.Contains(res.Text, "worktree:") || !strings.Contains(res.Text, "leak.txt") {
		t.Fatalf("the worktree leak was not reported: %+v", res)
	}
	if got := gitOut(t, wt, "rev-parse", "HEAD"); got != base {
		t.Fatalf("the worktree HEAD was not restored: %s, want %s", got, base)
	}
	if strings.Contains(res.Text, "AUDITsecretVALUE") {
		t.Errorf("value shown: %s", res.Text)
	}
}

// A per-worktree ref (refs/worktree/*) of another worktree is seen and removed.
func TestLeakOnAPerWorktreeRefOfALinkedWorktreeIsRestored(t *testing.T) {
	f := shellCommitFixture(t, false)
	wt := filepath.Join(filepath.Dir(f.root), "wt-own")
	run(t, f.root, "worktree", "add", "-q", "--detach", wt)
	write(t, f.root, "mk.sh", "#!/bin/sh\nset -e\ncp notes.txt "+wt+"/leak.txt\ncd "+wt+"\ngit add leak.txt\ngit update-ref refs/worktree/leak \"$(git commit-tree \"$(git write-tree)\" -p HEAD -m x)\"\n")
	res := shell(f, "sh mk.sh")
	if !res.IsError || res.Rule != string(policy.RuleSecretPath) || !strings.Contains(res.Text, "refs/worktree/leak") {
		t.Fatalf("the per-worktree ref leak was not reported: %+v", res)
	}
	if out := gitOut(t, wt, "for-each-ref", "refs/worktree"); out != "" {
		t.Fatalf("the per-worktree ref survived: %s", out)
	}
}
