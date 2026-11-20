package devcouncil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
)

// A git that exits non-zero has not answered, whatever it wrote first: the git
// runners take the exit statuses a caller lists and refuse the rest. That reaches
// every git call the ref guard, the commit scan and the git tools make, so these
// tests put a repository in each ordinary state git reports through a non-zero
// status somewhere (no commits yet, a first commit, a merge stopped on a
// conflict) and require that those calls still answer. A runner that refuses one
// of them has turned a state agents are routinely in into a failed tool.

type repoStateCase struct {
	name string
	// born is whether HEAD names a commit after setup.
	born bool
	// conflicted is whether the index holds unmerged paths after setup.
	conflicted bool
	setup      func(t *testing.T, f *fixture)
}

func repoStateCases() []repoStateCase {
	unborn := func(t *testing.T, f *fixture) {
		t.Helper()
		if err := os.RemoveAll(filepath.Join(f.root, ".git")); err != nil {
			t.Fatal(err)
		}
		run(t, f.root, "init", "-q")
		run(t, f.root, "config", "user.email", "h@test")
		run(t, f.root, "config", "user.name", "h")
		nameBranchMain(t, f.root)
	}
	return []repoStateCase{
		{name: "no commits", setup: unborn},
		{name: "no commits, a file staged", setup: func(t *testing.T, f *fixture) {
			t.Helper()
			unborn(t, f)
			write(t, f.root, "first.txt", "first\n")
			run(t, f.root, "add", "first.txt")
		}},
		{name: "one commit", born: true, setup: func(t *testing.T, f *fixture) {
			t.Helper()
			nameBranchMain(t, f.root)
		}},
		{name: "merge stopped on a conflict", born: true, conflicted: true, setup: func(t *testing.T, f *fixture) {
			t.Helper()
			nameBranchMain(t, f.root)
			run(t, f.root, "checkout", "-q", "-b", "side")
			write(t, f.root, "seed.txt", "side\n")
			run(t, f.root, "commit", "-qam", "side")
			run(t, f.root, "checkout", "-q", "main")
			write(t, f.root, "seed.txt", "main\n")
			run(t, f.root, "commit", "-qam", "main")
			// Exit 1 on the conflict is the state being built, not a failure.
			merge := exec.CommandContext(t.Context(), "git", "-C", f.root, "merge", "side") // #nosec G204 -- literal binary, fixed arguments.
			if err := merge.Run(); err == nil {
				t.Fatal("the merge did not conflict")
			}
		}},
	}
}

// TestRefGuardAndExecCommandAnswerInOrdinaryRepositoryStates runs the exec tool,
// whose ref guard lists refs, worktrees and per-worktree namespaces before and
// after the command, and judges any commit the command made.
func TestRefGuardAndExecCommandAnswerInOrdinaryRepositoryStates(t *testing.T) {
	for _, tc := range repoStateCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
			tc.setup(t, f)
			checkout(t, f)

			snap, err := f.reg.snapshotRefs(t.Context())
			if err != nil || snap == nil {
				t.Fatalf("snapshotRefs: %v, %v", snap, err)
			}
			if res := shell(f, "true"); res.IsError || strings.Contains(res.Text, "shell_commit") {
				t.Fatalf("a command that does nothing was refused: %+v", res)
			}
			if tc.conflicted {
				return
			}
			// A commit the command makes is judged by what it adds over its
			// parents, so rev-list, diff and cat-file run on the new commit.
			write(t, f.root, "added.txt", "added\n")
			res := shell(f, "git add added.txt && git commit -m clean")
			if res.IsError || strings.Contains(res.Text, "shell_commit") {
				t.Fatalf("a clean commit was refused or reported as a violation: %+v", res)
			}
			if got := gitOut(t, f.root, "log", "-1", "--format=%s"); got != "clean" {
				t.Fatalf("the commit did not stay: HEAD is %q", got)
			}
		})
	}
}

// TestCommitSecretScanAnswersInOrdinaryRepositoryStates runs the scan the commit
// tool makes over what is staged: write-tree, the staged name list, the base the
// content is compared with (the empty tree when there is no HEAD), and the patch.
func TestCommitSecretScanAnswersInOrdinaryRepositoryStates(t *testing.T) {
	for _, tc := range repoStateCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
			tc.setup(t, f)
			write(t, f.root, "clean.txt", "nothing secret\n")
			if !tc.conflicted {
				run(t, f.root, "add", "clean.txt")
			}

			refusal, tree, _ := f.reg.checkStagedForCommit(t.Context())
			switch {
			case tc.conflicted:
				// git cannot write a tree from an index with unmerged paths; the
				// scan must say it could not check, never report a clean pass.
				if refusal == nil {
					t.Fatalf("a scan that could not read the staged tree passed (tree %q)", tree)
				}
			case refusal != nil:
				t.Fatalf("a clean staged file was refused: %s", refusal.Text)
			case tree == "":
				t.Fatal("no checked tree came back from a scan that passed")
			}
		})
	}
}

// TestGitToolsAnswerInOrdinaryRepositoryStates runs every git tool in each state.
func TestGitToolsAnswerInOrdinaryRepositoryStates(t *testing.T) {
	for _, tc := range repoStateCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtureWith(t, map[string]string{
				flags.HarnessPosture:      flags.PostureYolo,
				flags.GrantsAgentCommands: "true",
			})
			tc.setup(t, f)
			checkout(t, f)

			for _, tool := range []string{"devcouncil_git_status", "devcouncil_git_log", "devcouncil_git_branches"} {
				if res := f.call(tool, map[string]any{}); res.IsError {
					t.Errorf("%s failed: %s", tool, res.Text)
				}
			}
			show := f.call("devcouncil_git_show", map[string]any{"object": "HEAD"})
			if tc.born && show.IsError {
				t.Errorf("git_show HEAD failed with a commit: %s", show.Text)
			}
			if !tc.born && !show.IsError {
				t.Errorf("git_show HEAD answered with no commit: %s", show.Text)
			}
			if tc.conflicted {
				return
			}

			write(t, f.root, "tool-added.txt", "added\n")
			if res := f.call("devcouncil_git_stage", map[string]any{"paths": []string{"tool-added.txt"}}); res.IsError {
				t.Fatalf("git_stage failed: %s", res.Text)
			}
			if res := f.call("devcouncil_git_commit", map[string]any{"message": "tool commit"}); res.IsError {
				t.Fatalf("git_commit failed: %s", res.Text)
			}
			if got := gitOut(t, f.root, "log", "-1", "--format=%s"); got != "tool commit" {
				t.Fatalf("HEAD is %q after git_commit", got)
			}
			if res := f.call("devcouncil_git_show", map[string]any{"object": "HEAD"}); res.IsError {
				t.Errorf("git_show HEAD failed after the commit: %s", res.Text)
			}
		})
	}
}

// TestGitDiffRendersEveryShapeOfUntrackedFile: git diff --no-index exits 1 for
// each of these, with a header behind it even when the file is empty, so the exit
// status the renderer lists must keep all of them.
func TestGitDiffRendersEveryShapeOfUntrackedFile(t *testing.T) {
	root := newGitRepo(t)
	write(t, root, "empty.txt", "")
	write(t, root, "binary.bin", "\x00\x01\x02")
	write(t, root, "no-newline.txt", "a")
	if err := os.Symlink("empty.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent-target", filepath.Join(root, "dangling.txt")); err != nil {
		t.Fatal(err)
	}

	diff, notes, err := gitDiff(t.Context(), root)
	if err != nil || len(notes) != 0 {
		t.Fatalf("gitDiff: %v, notes %v", err, notes)
	}
	for _, name := range []string{"empty.txt", "binary.bin", "no-newline.txt", "link.txt", "dangling.txt"} {
		if !strings.Contains(diff, "b/"+name) {
			t.Errorf("the diff the gates read lacks the untracked %s:\n%s", name, diff)
		}
	}
}
