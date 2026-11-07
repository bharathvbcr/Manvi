package devcouncil

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

func headOf(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// shellCommitFixture is a dev-posture checkout with a .env on disk, one clean
// file (app.js) staged, and notes.txt, a copy of the .env value under another
// name, either staged (leak) or left in the working tree.
func shellCommitFixture(t *testing.T, stageLeak bool) *fixture {
	t.Helper()
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	write(t, f.root, ".env", secretBody)
	checkout(t, f)
	write(t, f.root, "app.js", "const mode = 'development';\n")
	run(t, f.root, "add", "app.js")
	run(t, f.root, "config", "alias.ci", "commit")
	run(t, f.root, "config", "alias.cx", "!git commit")
	write(t, f.root, "msg.txt", "from a file\n")
	write(t, f.root, "notes.txt", secretBody)
	if stageLeak {
		run(t, f.root, "add", "notes.txt")
	}
	return f
}

func shell(f *fixture, command string) (res struct {
	Text    string
	IsError bool
	Rule    string
}) {
	r := f.call("devcouncil_exec_command", map[string]string{"command": command})
	res.Text, res.IsError, res.Rule = r.Text, r.IsError, r.Rule
	return res
}

// The agent shell must not commit what the commit tool would refuse.
func TestShellCommitIsScannedForLeakedContent(t *testing.T) {
	for _, command := range []string{
		"git commit -m x",
		"git commit --no-verify -m x",
		"git commit --amend --no-edit",
		"git commit -S -m x",
		"git commit --allow-empty -m x",
		"git -c user.name=a -c user.email=a@b.c commit -m x",
		"GIT_AUTHOR_NAME=a git commit -m x",
		"env GIT_AUTHOR_NAME=a git commit -m x",
		"command git commit -m x",
		"/usr/bin/git commit -m x",
		"(git commit -m x)",
		"sh -c 'git commit -m x'",
		`bash -lc "git commit --no-verify -m x"`,
		"timeout 30 git commit -m x",
		"git ci -m x",
		"git cx -m x",
	} {
		t.Run(command, func(t *testing.T) {
			f := shellCommitFixture(t, true)
			before := headOf(t, f.root)
			res := shell(f, command)
			if !res.IsError || res.Rule != string(policy.RuleSecretPath) {
				t.Fatalf("the shell committed a staged copy of a secret: %+v", res)
			}
			if !strings.Contains(res.Text, "devcouncil_git_commit") {
				t.Errorf("the refusal does not point at the commit tool: %s", res.Text)
			}
			if strings.Contains(res.Text, "AUDITsecretVALUE") {
				t.Errorf("the refusal carries the value: %s", res.Text)
			}
			if after := headOf(t, f.root); after != before {
				t.Fatalf("HEAD moved from %s to %s", before, after)
			}
		})
	}
}

// Forms the scan cannot vouch for are refused even when the index is clean,
// because the commit would not record the index that was scanned.
func TestShellCommitRefusesFormsTheScanCannotVouchFor(t *testing.T) {
	for _, command := range []string{
		"git add notes.txt && git commit -m x",
		"git rm --cached app.js; git commit -m x",
		"git status && git commit -m x",
		"git commit -m x && git status",
		"git commit -m x & git add notes.txt",
		"git commit -am x",
		"git commit -a -m x",
		"git commit --all -m x",
		"git commit --al -m x",
		"git commit -m x -- app.js",
		"git commit -m x app.js",
		"git commit -i app.js -m x",
		"git commit --include app.js -m x",
		"git commit -o app.js -m x",
		"git commit --only app.js -m x",
		"git commit --pathspec-from-file=paths.txt -m x",
		"git commit -p -m x",
		"git commit --frobnicate -m x",
		"GIT_INDEX_FILE=/tmp/other-index git commit -m x",
		"GIT_DIR=.git git commit -m x",
		"GIT_WORK_TREE=. git commit -m x",
		"env GIT_INDEX_FILE=/tmp/other-index git commit -m x",
		"git -c core.worktree=. commit -m x",
		"git -c include.path=/tmp/x commit -m x",
		`git commit -m "$(git add notes.txt)"`,
		"sh -c 'git add notes.txt && git commit -m x'",
		"echo x | xargs git commit -m",
		"git commit -m 'unterminated",
	} {
		t.Run(command, func(t *testing.T) {
			f := shellCommitFixture(t, false)
			before := headOf(t, f.root)
			res := shell(f, command)
			if !res.IsError || !strings.Contains(res.Text, "devcouncil_git_commit") {
				t.Fatalf("the form was not refused with a pointer to the commit tool: %+v", res)
			}
			if after := headOf(t, f.root); after != before {
				t.Fatalf("HEAD moved from %s to %s", before, after)
			}
		})
	}
}

// The scan must not turn the owner's policy into a refusal: a clean index
// still commits through the shell, --no-verify included.
func TestShellCommitStillCommitsCleanContent(t *testing.T) {
	for _, command := range []string{
		"git commit -m clean",
		"git commit --no-verify -m clean",
		"git commit -nm clean",
		"git commit -m 'two words' --allow-empty",
		"git -c user.name=a -c user.email=a@b.c commit -m clean",
		"sh -c 'git commit -m clean'",
		"git commit -m clean 2>&1",
		"git commit -F msg.txt",
	} {
		t.Run(command, func(t *testing.T) {
			f := shellCommitFixture(t, false)
			before := headOf(t, f.root)
			res := shell(f, command)
			if res.IsError {
				t.Fatalf("a clean commit through the shell was refused or failed: %s", res.Text)
			}
			if after := headOf(t, f.root); after == before {
				t.Fatalf("HEAD did not move: %s", res.Text)
			}
		})
	}
}

// Commands that mention git or commit without creating one are untouched.
func TestShellCommitScanLeavesOtherCommandsAlone(t *testing.T) {
	f := shellCommitFixture(t, true)
	for _, command := range []string{"git status", "git log --oneline -1 && git status", "echo git commit", "git diff --cached --stat"} {
		if res := shell(f, command); res.IsError {
			t.Errorf("%q was refused: %s", command, res.Text)
		}
	}
}

// Residual, pinned: subcommands that make a commit from something other than
// the index are not scanned and not denied (the owner's policy allows them).
func TestShellCommitResidualSubcommandsStillRun(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	checkout(t, f)
	write(t, f.root, "a.txt", "benign\n")
	run(t, f.root, "add", "a.txt")
	run(t, f.root, "commit", "-qm", "benign")
	before := headOf(t, f.root)
	res := shell(f, "git revert --no-edit HEAD")
	if res.IsError {
		t.Fatalf("git revert was refused or failed: %s", res.Text)
	}
	if after := headOf(t, f.root); after == before {
		t.Fatalf("revert did not create a commit: %s", res.Text)
	}
}

// The command policy already hard-denies these spellings (command.directory_change,
// command.reparse); the scan reads them too, so it does not depend on that
// ladder staying as it is. The first test pins the end-to-end refusal, the
// second pins the scan's own verdict with no policy in front of it.
func TestShellCommitPolicyDeniedFormsNeverCommit(t *testing.T) {
	for _, command := range []string{
		"git -C . commit -m x",
		"git --git-dir=.git commit -m x",
		"git --work-tree=. commit -m x",
		"eval 'git commit -m x'",
		"eval 'git add -A && git commit -m x'",
	} {
		t.Run(command, func(t *testing.T) {
			f := shellCommitFixture(t, true)
			before := headOf(t, f.root)
			if res := shell(f, command); !res.IsError {
				t.Fatalf("the command ran: %s", res.Text)
			}
			if after := headOf(t, f.root); after != before {
				t.Fatalf("HEAD moved from %s to %s", before, after)
			}
		})
	}
}

func TestShellCommitScanReadsFormsWithoutThePolicyInFront(t *testing.T) {
	noAliases := func() (map[string]string, error) { return map[string]string{}, nil }
	for command, wantReason := range map[string]bool{
		"git -C . commit -m x":                     true,
		"git --git-dir=.git commit -m x":           true,
		"git --work-tree=. commit -m x":            true,
		"eval 'git commit -m x'":                   false,
		"eval 'git add -A && git commit -m x'":     true,
		"git commit -m x":                          false,
		"git status":                               false,
		"git -c alias.x=commit x -m y":             true,
		"git -c core.hooksPath=/tmp/h commit -m x": true,
	} {
		s := &shellCommitScan{raw: command, aliases: noAliases}
		s.inspect(command, 0)
		multi := s.commits > 1 || (s.commits > 0 && s.others > 0)
		if got := len(s.reasons) > 0 || multi; got != wantReason {
			t.Errorf("%q: refused = %v (reasons %v, commits %d, others %d), want %v", command, got, s.reasons, s.commits, s.others, wantReason)
		}
	}
}
