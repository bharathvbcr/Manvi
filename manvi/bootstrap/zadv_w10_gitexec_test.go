package bootstrap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Adversarial review of the w10-gitexec lane: every test here builds a
// repository whose config or hooks the agent could author, runs the helper's
// git over it, and checks that no program the repository names ran. A test that
// finds a program running fails, except for the channels the owner decided to
// leave to the operator's config (filters, diff drivers, ssh, editor, gpg): those
// tests pin that behaviour with zadvPinned, so changing it fails them.

type zadvRun func(t *testing.T, dir string, args ...string) error

// zadvHelper runs the production helper under test.
func zadvHelper(t *testing.T, dir string, args ...string) error {
	t.Helper()
	_, err := git(context.Background(), dir, args...)
	return err
}

type zadvRepo struct {
	root, markers string
}

// zadvSh runs plain git (the test's own, not the helper) with a hermetic config.
func zadvSh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	// #nosec G204 -- the binary is the literal "git"; the arguments are this test's own.
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func zadvNew(t *testing.T) zadvRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	markers, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	zadvSh(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	zadvSh(t, root, "add", "f.txt")
	zadvSh(t, root, "commit", "-q", "-m", "base")
	return zadvRepo{root: root, markers: markers}
}

// script writes an executable that touches the named marker and returns its path.
func (r zadvRepo) script(t *testing.T, name string) (path, marker string) {
	t.Helper()
	marker = filepath.Join(r.markers, name+".marker")
	path = filepath.Join(r.markers, name+".sh")
	body := "#!/bin/sh\ntouch '" + marker + "'\nexit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // a program git runs has to be executable
		t.Fatal(err)
	}
	return path, marker
}

func (r zadvRepo) hooksDir(t *testing.T, dir, marker string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit", "post-index-change", "fsmonitor-watchman", "reference-transaction", "pre-auto-gc"} {
		body := "#!/bin/sh\ntouch '" + marker + "'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil { //nolint:gosec // a hook has to be executable
			t.Fatal(err)
		}
	}
}

func zadvVerdict(t *testing.T, marker, what string) {
	t.Helper()
	if _, err := os.Stat(marker); err != nil {
		return
	}
	t.Fatalf("%s", what)
}

// zadvPinned asserts the decided behaviour for a channel that is operator
// config: ran says whether the program named in the repository's config runs
// under the helper. A change in either direction fails.
func zadvPinned(t *testing.T, marker string, ran bool, what string) {
	t.Helper()
	_, err := os.Stat(marker)
	if got := err == nil; got != ran {
		t.Fatalf("%s: ran=%v, decided behaviour is ran=%v", what, got, ran)
	}
}

var zadvCommit = []string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "x"}

// ---- config sources that outrank or sit beside -c ------------------------

func zadvIncludeCase(t *testing.T, run zadvRun, cond string) {
	r := zadvNew(t)
	_, mon := r.script(t, "mon")
	monPath := filepath.Join(r.markers, "mon.sh")
	hookMarker := filepath.Join(r.markers, "hook")
	hooks := filepath.Join(r.markers, "hooks")
	r.hooksDir(t, hooks, hookMarker)
	inc := filepath.Join(r.markers, "inc.cfg")
	body := "[core]\n\thooksPath = " + hooks + "\n\tfsmonitor = " + monPath + "\n"
	if err := os.WriteFile(inc, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	section := "[include]\n\tpath = " + inc + "\n"
	if cond != "" {
		section = "[includeIf \"" + cond + "\"]\n\tpath = " + inc + "\n"
	}
	cfg := filepath.Join(r.root, ".git", "config")
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(section); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := zadvSh(t, r.root, "config", "--get", "core.hooksPath"); got != hooks {
		t.Fatalf("premise: the include is not read (hooksPath=%q)", got)
	}
	_ = os.Remove(hookMarker)
	_ = os.Remove(mon)
	if err := run(t, r.root, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if err := run(t, r.root, zadvCommit...); err != nil {
		t.Fatal(err)
	}
	zadvVerdict(t, hookMarker, "a hook from an include.path'd hooksPath ran")
	zadvVerdict(t, mon, "an fsmonitor named by an include.path'd file ran")
}

func TestAdvIncludePathCannotOutrankDashC(t *testing.T) { zadvIncludeCase(t, zadvHelper, "") }

func TestAdvIncludeIfCannotOutrankDashC(t *testing.T) {
	zadvIncludeCase(t, zadvHelper, "gitdir:**")
}

// config.worktree (extensions.worktreeConfig) is read after the shared config,
// so it is the highest-precedence file; -c must still beat it.
func zadvWorktreeConfigCase(t *testing.T, run zadvRun) {
	r := zadvNew(t)
	wt := filepath.Join(r.markers, "linked")
	zadvSh(t, r.root, "worktree", "add", "-q", "--detach", wt)
	zadvSh(t, r.root, "config", "extensions.worktreeConfig", "true")
	hookMarker := filepath.Join(r.markers, "hook")
	hooks := filepath.Join(r.markers, "hooks")
	r.hooksDir(t, hooks, hookMarker)
	monPath, mon := r.script(t, "mon")
	zadvSh(t, wt, "config", "--worktree", "core.hooksPath", hooks)
	zadvSh(t, wt, "config", "--worktree", "core.fsmonitor", monPath)
	if got := zadvSh(t, wt, "config", "--get", "core.hooksPath"); got != hooks {
		t.Fatalf("premise: config.worktree not read (%q)", got)
	}
	_ = os.Remove(hookMarker)
	_ = os.Remove(mon)
	if err := run(t, wt, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if err := run(t, wt, zadvCommit...); err != nil {
		t.Fatal(err)
	}
	zadvVerdict(t, hookMarker, "a hook from config.worktree's hooksPath ran")
	zadvVerdict(t, mon, "an fsmonitor named in config.worktree ran")
}

func TestAdvWorktreeConfigCannotOutrankDashC(t *testing.T) { zadvWorktreeConfigCase(t, zadvHelper) }

// A .git FILE redirects the repository to a directory the agent made, so the
// "repository config" is wholly agent-authored (own hooks dir and config).
func zadvGitFileCase(t *testing.T, run zadvRun) {
	r := zadvNew(t)
	fake := filepath.Join(r.markers, "fakegit")
	zadvSh(t, r.markers, "clone", "-q", "--bare", r.root, fake)
	hookMarker := filepath.Join(r.markers, "hook")
	r.hooksDir(t, filepath.Join(fake, "hooks"), hookMarker)
	monPath, mon := r.script(t, "mon")
	zadvSh(t, fake, "config", "core.fsmonitor", monPath)
	zadvSh(t, fake, "config", "core.bare", "false")
	work := filepath.Join(r.markers, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: "+fake+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	zadvSh(t, work, "config", "core.worktree", work)
	zadvSh(t, work, "reset", "-q", "--hard", "HEAD")
	_ = os.Remove(hookMarker)
	_ = os.Remove(mon)
	if err := run(t, work, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if err := run(t, work, zadvCommit...); err != nil {
		t.Fatal(err)
	}
	zadvVerdict(t, hookMarker, "a hook in a .git-file-redirected gitdir ran")
	zadvVerdict(t, mon, "an fsmonitor from a .git-file-redirected gitdir ran")
}

func TestAdvDotGitFileRedirectRunsNothing(t *testing.T) { zadvGitFileCase(t, zadvHelper) }

// An inherited GIT_CONFIG_PARAMETERS / GIT_CONFIG_COUNT would be parsed by git
// before -c; the allowlist must keep them out, and in any case -c must win.
func zadvEnvCase(t *testing.T, run zadvRun) {
	r := zadvNew(t)
	hookMarker := filepath.Join(r.markers, "hook")
	hooks := filepath.Join(r.markers, "hooks")
	r.hooksDir(t, hooks, hookMarker)
	monPath, mon := r.script(t, "mon")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.hooksPath'='"+hooks+"' 'core.fsmonitor'='"+monPath+"'")
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", hooks)
	t.Setenv("GIT_CONFIG_KEY_1", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_1", monPath)
	if err := run(t, r.root, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if err := run(t, r.root, zadvCommit...); err != nil {
		t.Fatal(err)
	}
	zadvVerdict(t, hookMarker, "a hook named by GIT_CONFIG_PARAMETERS/COUNT in the host env ran")
	zadvVerdict(t, mon, "an fsmonitor named by GIT_CONFIG_PARAMETERS/COUNT ran")
}

func TestAdvEnvConfigInjectionCannotReachGit(t *testing.T) { zadvEnvCase(t, zadvHelper) }

// GIT_CONFIG_GLOBAL is on the allowlist; a file it names is lower precedence
// than -c, so the helper must still find no hook.
func zadvGlobalFileCase(t *testing.T, run zadvRun) {
	r := zadvNew(t)
	hookMarker := filepath.Join(r.markers, "hook")
	hooks := filepath.Join(r.markers, "hooks")
	r.hooksDir(t, hooks, hookMarker)
	monPath, mon := r.script(t, "mon")
	global := filepath.Join(r.markers, "global.cfg")
	if err := os.WriteFile(global, []byte("[core]\n\thooksPath = "+hooks+"\n\tfsmonitor = "+monPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	if err := run(t, r.root, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if err := run(t, r.root, zadvCommit...); err != nil {
		t.Fatal(err)
	}
	zadvVerdict(t, hookMarker, "a hook from the allowlisted GIT_CONFIG_GLOBAL file ran")
	zadvVerdict(t, mon, "an fsmonitor from the allowlisted GIT_CONFIG_GLOBAL file ran")
}

func TestAdvGlobalConfigFileCannotOutrankDashC(t *testing.T) { zadvGlobalFileCase(t, zadvHelper) }

// core.fsmonitor=true selects the hook-based monitor (fsmonitor-watchman, found
// through hooksPath); a differently-cased key must not slip past the =false.
func zadvFsmonitorSpellings(t *testing.T, run zadvRun) {
	for _, key := range []string{"core.fsmonitor", "Core.FsMonitor", "CORE.FSMONITOR"} {
		t.Run(key, func(t *testing.T) {
			r := zadvNew(t)
			hookMarker := filepath.Join(r.markers, "hook")
			r.hooksDir(t, filepath.Join(r.root, ".git", "hooks"), hookMarker)
			zadvSh(t, r.root, "config", key, "true")
			zadvSh(t, r.root, "config", "core.fsmonitorHookVersion", "2")
			if err := run(t, r.root, "status", "--porcelain"); err != nil {
				t.Fatal(err)
			}
			if err := run(t, r.root, "diff", "--name-only", "HEAD"); err != nil {
				t.Fatal(err)
			}
			zadvVerdict(t, hookMarker, "the hook-based fsmonitor ran under "+key+"=true")
		})
	}
}

func TestAdvFsmonitorSpellings(t *testing.T) { zadvFsmonitorSpellings(t, zadvHelper) }

// ---- other execution channels --------------------------------------------

// filter.<x>.clean runs whenever git compares a modified work-tree file with
// the index, so a plain `diff --name-only HEAD` (the exact shape the verify and
// scan callers use, with --no-ext-diff --no-textconv) reaches it. The helper
// pins neither filters nor the diff drivers.
func zadvFilterSetup(t *testing.T, kind string) (zadvRepo, string) {
	r := zadvNew(t)
	prog, marker := r.script(t, "filter")
	if err := os.WriteFile(filepath.Join(r.root, ".gitattributes"), []byte("*.txt filter=zadv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	zadvSh(t, r.root, "add", ".gitattributes")
	zadvSh(t, r.root, "commit", "-q", "-m", "attrs")
	if kind == "process" {
		zadvSh(t, r.root, "config", "filter.zadv.process", prog)
	} else {
		zadvSh(t, r.root, "config", "filter.zadv.clean", prog+" %f")
	}
	if err := os.WriteFile(filepath.Join(r.root, "f.txt"), []byte("two, longer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(marker)
	return r, marker
}

func zadvFilterCase(t *testing.T, run zadvRun) {
	for _, kind := range []string{"clean", "process"} {
		for _, args := range [][]string{
			{"diff", "--name-only", "-z", "--no-renames", "HEAD"},
			{"diff", "--name-only", "-z", "--no-ext-diff", "--no-textconv", "--diff-filter=ACMR", "HEAD", "--"},
			{"diff", "--no-textconv", "--no-ext-diff", "--no-color", "HEAD", "--", "f.txt"},
			{"ls-files", "-z", "--others", "--exclude-standard"},
		} {
			t.Run(kind+"/"+strings.Join(args[:2], "_"), func(t *testing.T) {
				r, marker := zadvFilterSetup(t, kind)
				_ = run(t, r.root, args...)
				// ls-files only lists; it never compares content, so no filter runs.
				zadvPinned(t, marker, args[0] == "diff", "filter."+kind+" named in operator config under helper args "+strings.Join(args, " "))
			})
		}
	}
}

func TestAdvOperatorFilterRunsUnderHelperByDecision(t *testing.T) { zadvFilterCase(t, zadvHelper) }

func zadvDiffDriverCase(t *testing.T, run zadvRun, extra ...string) {
	for _, driver := range []string{"external", "textconv"} {
		t.Run(driver, func(t *testing.T) {
			r := zadvNew(t)
			prog, marker := r.script(t, "drv")
			if err := os.WriteFile(filepath.Join(r.root, ".gitattributes"), []byte("*.txt diff=zadv\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if driver == "external" {
				zadvSh(t, r.root, "config", "diff.external", prog)
			} else {
				zadvSh(t, r.root, "config", "diff.zadv.textconv", prog)
			}
			if err := os.WriteFile(filepath.Join(r.root, "f.txt"), []byte("two\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"diff", "--no-color", "HEAD", "--"}, extra...)
			_ = run(t, r.root, args...)
			zadvPinned(t, marker, true, "diff."+driver+" named in operator config, caller flags absent")
		})
	}
}

// The callers pass --no-ext-diff/--no-textconv; this is their exact shape.
func TestAdvDiffDriversWithCallerFlags(t *testing.T) {
	r := zadvNew(t)
	prog, marker := r.script(t, "drv")
	if err := os.WriteFile(filepath.Join(r.root, ".gitattributes"), []byte("*.txt diff=zadv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	zadvSh(t, r.root, "config", "diff.external", prog)
	zadvSh(t, r.root, "config", "diff.zadv.textconv", prog)
	if err := os.WriteFile(filepath.Join(r.root, "f.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := zadvHelper(t, r.root, "diff", "--no-textconv", "--no-ext-diff", "--no-color", "--end-of-options", "HEAD", "--", "f.txt"); err != nil {
		t.Fatal(err)
	}
	zadvVerdict(t, marker, "a diff driver ran despite --no-ext-diff --no-textconv")
}

// A diff that omits the flags: the helper itself does not stop diff.external or
// a textconv. By decision they are the operator's configured commands, and the
// callers' --no-ext-diff --no-textconv (pinned by the test above) are what keep
// them from running today.
func TestAdvDiffDriversRunWithoutCallerFlagsByDecision(t *testing.T) { zadvDiffDriverCase(t, zadvHelper) }

func zadvTransportCase(t *testing.T, run zadvRun) {
	t.Run("sshCommand", func(t *testing.T) {
		r := zadvNew(t)
		prog, marker := r.script(t, "ssh")
		zadvSh(t, r.root, "config", "core.sshCommand", prog)
		_ = run(t, r.root, "ls-remote", "ssh://zadv.invalid/x")
		zadvPinned(t, marker, true, "core.sshCommand from repository config ran")
	})
	t.Run("editor", func(t *testing.T) {
		r := zadvNew(t)
		prog, marker := r.script(t, "ed")
		zadvSh(t, r.root, "config", "core.editor", prog)
		_ = run(t, r.root, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty")
		zadvPinned(t, marker, true, "core.editor from repository config ran")
	})
	t.Run("gpg.program", func(t *testing.T) {
		r := zadvNew(t)
		prog, marker := r.script(t, "gpg")
		zadvSh(t, r.root, "config", "gpg.program", prog)
		_ = run(t, r.root, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=true", "commit", "--allow-empty", "-m", "x")
		zadvPinned(t, marker, true, "gpg.program from repository config ran on a signed commit")
	})
	t.Run("packObjectsHook", func(t *testing.T) {
		r := zadvNew(t)
		prog, marker := r.script(t, "pack")
		zadvSh(t, r.root, "config", "uploadpack.packObjectsHook", prog)
		_ = run(t, r.root, "fetch", "--no-write-fetch-head", r.root, "HEAD")
		zadvPinned(t, marker, false, "uploadpack.packObjectsHook from repository config ran")
	})
	t.Run("pager", func(t *testing.T) {
		r := zadvNew(t)
		prog, marker := r.script(t, "pager")
		zadvSh(t, r.root, "config", "core.pager", prog)
		zadvSh(t, r.root, "config", "pager.log", prog)
		_ = run(t, r.root, "log", "-1")
		zadvPinned(t, marker, false, "core.pager from repository config ran")
	})
	t.Run("alias", func(t *testing.T) {
		r := zadvNew(t)
		prog, marker := r.script(t, "alias")
		zadvSh(t, r.root, "config", "alias.status", "!"+prog)
		_ = run(t, r.root, "status", "--porcelain")
		zadvPinned(t, marker, false, "an alias shadowing a builtin ran")
	})
}

func TestAdvOperatorConfigChannelsByDecision(t *testing.T) { zadvTransportCase(t, zadvHelper) }
