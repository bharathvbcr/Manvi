package devcouncil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// Cards ft-1ba663b7 (pseudo-ref follow-up): git resolves, and pushes, any file
// in a git directory whose first line begins with an object id. Each test parks a
// leaking commit in one such file, asserts the call is blocked as path.secret and
// the file is put back, and that a later push by that name delivers nothing.

const parkLeak = "set -e\nc=$(git commit-tree $(git write-tree) -m leak)\nh=$(git rev-parse HEAD)\nd=$(git rev-parse --git-common-dir)\n"

type pseudoCase struct {
	name string
	// park is the shell that parks $c (the leaking commit) and may use $h (a clean
	// one) and $d (the common git dir).
	park string
	// by is the name a push uses, and that must not resolve after the call.
	by string
}

func runPseudoCase(t *testing.T, f *fixture, root string, tc pseudoCase) {
	t.Helper()
	remote := bareRemote(t)
	before, _ := exec.CommandContext(t.Context(), "git", "-C", root, "rev-parse", "--verify", "-q", tc.by).Output() // #nosec G204 -- the binary is the literal "git"; the arguments are this test's own.
	resolvedBefore := strings.TrimSpace(string(before))
	write(t, root, "park.sh", parkLeak+tc.park+"\n")
	res := shell(f, "sh park.sh")
	if !res.IsError || !res.Blocked || res.Rule != string(policy.RuleSecretPath) {
		t.Fatalf("FAIL-OPEN: a leak parked as %s was allowed or mislabelled: %+v", tc.by, res)
	}
	if strings.Contains(res.Text, "AUDITsecretVALUE") {
		t.Errorf("the result carries the value: %s", res.Text)
	}
	if strings.Contains(res.Text, "MANUAL INTERVENTION") {
		t.Errorf("a false manual intervention: %s", res.Text)
	}
	after, err := exec.CommandContext(t.Context(), "git", "-C", root, "rev-parse", "--verify", "-q", tc.by).CombinedOutput() // #nosec G204 -- the binary is the literal "git"; the arguments are this test's own.
	if err == nil && strings.TrimSpace(string(after)) != resolvedBefore {
		t.Fatalf("%s resolves to %s after the call, was %q before", tc.by, after, resolvedBefore)
	}
	push := shell(f, "git push -q "+remote+" "+tc.by+":refs/heads/leaked")
	if resolvedBefore == "" && !push.IsError {
		t.Errorf("a push of %s succeeded after the rollback: %+v", tc.by, push)
	}
	if resolvedBefore != "" {
		if got := gitOut(t, remote, "for-each-ref", "--format=%(objectname)"); got != "" && got != resolvedBefore {
			t.Fatalf("a ref arrived at the remote that is not the pre-existing one: %s", got)
		}
		return
	}
	requireNothingArrived(t, remote)
}

// From the main worktree's root.
func TestW21PseudoRefsParkedFromTheMainRootAreRolledBack(t *testing.T) {
	for _, tc := range []pseudoCase{
		{"ORIG_HEAD", "git update-ref ORIG_HEAD $c", "ORIG_HEAD"},
		{"arbitrary_root_name", "git update-ref MYLEAK_HEAD $c", "MYLEAK_HEAD"},
		{"FETCH_HEAD_multiline_leak_first", "printf '%s\\t\\tbranch a of x\\n%s\\tnot-for-merge\\tbranch b of x\\n' $c $h > $d/FETCH_HEAD", "FETCH_HEAD"},
		{"FETCH_HEAD_multiline_leak_second", "printf '%s\\t\\tbranch a of x\\n%s\\tnot-for-merge\\tbranch b of x\\n' $h $c > $d/FETCH_HEAD", "FETCH_HEAD"},
		{"MERGE_HEAD_two_parents", "printf '%s\\n%s\\n' $h $c > $d/MERGE_HEAD", "MERGE_HEAD"},
		{"CHERRY_PICK_HEAD", "echo $c > $d/CHERRY_PICK_HEAD", "CHERRY_PICK_HEAD"},
		{"lowercase_root_file", "echo $c > $d/leakfile", "leakfile"},
		{"nested_file", "mkdir -p $d/foo && echo $c > $d/foo/bar", "foo/bar"},
		{"admin_dir_file_from_main", "git worktree add -q --detach wts-a && git update-ref worktrees/wts-a/ORIG_HEAD $c", "worktrees/wts-a/ORIG_HEAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shellCommitFixture(t, true)
			runPseudoCase(t, f, f.root, tc)
		})
	}
}

// From a linked worktree's root (every GitPulse lane): the main worktree's
// pseudo-refs (`main-worktree/NAME`) and the root's own.
func TestW21PseudoRefsParkedFromALinkedRootAreRolledBack(t *testing.T) {
	for _, tc := range []pseudoCase{
		{"main_worktree_ORIG_HEAD", "git update-ref main-worktree/ORIG_HEAD $c", "main-worktree/ORIG_HEAD"},
		{"main_worktree_arbitrary", "git update-ref main-worktree/MYLEAK_HEAD $c", "main-worktree/MYLEAK_HEAD"},
		{"main_worktree_FETCH_HEAD_multiline", "printf '%s\\t\\tbranch a of x\\n%s\\tnot-for-merge\\tbranch b of x\\n' $c $h > $d/FETCH_HEAD", "main-worktree/FETCH_HEAD"},
		{"own_ORIG_HEAD", "git update-ref ORIG_HEAD $c", "ORIG_HEAD"},
		{"own_arbitrary", "git update-ref MYLEAK_HEAD $c", "MYLEAK_HEAD"},
		{"common_lowercase_file", "echo $c > $d/leakfile", "leakfile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, wt := linkedRootFixture(t, "wt-w21-pseudo-"+tc.name)
			runPseudoCase(t, f, wt, tc)
		})
	}
}

// A pseudo-ref a command moves onto a leaking commit is put back to the single
// object id it held; one that held several is reported and left.
func TestW21PseudoRefThatExistedBeforeIsRestoredOrReported(t *testing.T) {
	f := shellCommitFixture(t, true)
	common := gitCommon(t, f.root)
	head := headOf(t, f.root)

	write(t, f.root, "park.sh", parkLeak+"echo $c > $d/ORIG_HEAD\n")
	if err := os.WriteFile(filepath.Join(common, "ORIG_HEAD"), []byte(head+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := shell(f, "sh park.sh")
	if !res.IsError || !res.Blocked || res.Rule != string(policy.RuleSecretPath) || strings.Contains(res.Text, "MANUAL INTERVENTION") {
		t.Fatalf("a moved ORIG_HEAD was not rolled back cleanly: %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(common, "ORIG_HEAD")) // #nosec G304 -- a file in the git dir this test created.
	if err != nil || strings.TrimSpace(string(got)) != head {
		t.Fatalf("ORIG_HEAD was not put back to %s: %q (%v)", head, got, err)
	}

	multi := head + "\t\tbranch a of x\n" + head + "\tnot-for-merge\tbranch b of x\n"
	if err := os.WriteFile(filepath.Join(common, "FETCH_HEAD"), []byte(multi), 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, f.root, "park2.sh", parkLeak+"printf '%s\\t\\tbranch a of x\\n' $c > $d/FETCH_HEAD\n")
	res = shell(f, "sh park2.sh")
	if !res.IsError || !res.Blocked || res.Rule != string(policy.RuleSecretPath) || !strings.Contains(res.Text, "MANUAL INTERVENTION") {
		t.Fatalf("a moved multi-line FETCH_HEAD was not reported for manual repair: %+v", res)
	}
}

// Legitimate churn is silent: ORIG_HEAD set by a reset, FETCH_HEAD by a fetch of
// clean commits, and the usual files of a git directory.
func TestW21LegitimateGitFilesAreSilent(t *testing.T) {
	f := shellCommitFixture(t, false)
	if res := shell(f, "git commit -qm clean && git reset -q --soft HEAD~1"); res.IsError || res.Blocked {
		t.Fatalf("a clean commit and soft reset was refused: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(gitCommon(t, f.root), "ORIG_HEAD")); err != nil {
		t.Fatalf("bad test: the reset did not write ORIG_HEAD: %v", err)
	}
	run(t, f.root, "remote", "add", "up", upstreamWithCommits(t, 5))
	if res := shell(f, "git fetch -q up"); res.IsError || res.Blocked {
		t.Fatalf("a clean fetch was refused: %+v", res)
	}
}
