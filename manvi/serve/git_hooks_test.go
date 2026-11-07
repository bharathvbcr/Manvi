package serve

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// hookRepo is a repository whose hooks and fsmonitor program each write their
// own marker file, so a test can tell which of them git ran.
type hookRepo struct {
	root, hookMarker, monitorMarker string
}

func newHookRepo(t *testing.T, tracked bool) hookRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := t.TempDir()
	markers := t.TempDir() // outside the work tree, so the markers never show as changes
	r := hookRepo{root: root, hookMarker: filepath.Join(markers, "hook"), monitorMarker: filepath.Join(markers, "monitor")}
	gitIn(t, root, "init", "-q")

	hooks := filepath.Join(root, ".git", "hooks")
	if tracked {
		hooks = filepath.Join(root, ".husky")
		gitIn(t, root, "config", "core.hooksPath", ".husky")
	}
	if err := os.MkdirAll(hooks, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-commit", "prepare-commit-msg", "post-commit", "post-index-change"} {
		script := "#!/bin/sh\ntouch '" + r.hookMarker + "'\n"
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(script), 0o755); err != nil { //nolint:gosec // a hook has to be executable
			t.Fatal(err)
		}
	}
	monitor := filepath.Join(markers, "monitor.sh")
	if err := os.WriteFile(monitor, []byte("#!/bin/sh\ntouch '"+r.monitorMarker+"'\n"), 0o755); err != nil { //nolint:gosec // an fsmonitor program has to be executable
		t.Fatal(err)
	}
	gitIn(t, root, "config", "core.fsmonitor", monitor)
	return r
}

func (r hookRepo) ran(t *testing.T, marker, what string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("%s ran from a repository the agent can write", what)
	}
}

// runGit is the host's one git child over agent-writable worktrees. Whatever
// subcommand a caller gives it, a repository hook must not be found: today's
// callers read (diff, ls-files, rev-parse), and this pins the helper so one
// that commits or merges cannot start a hook the command gate never saw.
func TestRunGitFindsNoRepositoryHook(t *testing.T) {
	for name, tracked := range map[string]bool{"tracked core.hooksPath dir": true, "dot-git hooks": false} {
		t.Run(name, func(t *testing.T) {
			r := newHookRepo(t, tracked)
			_, _, err := runGit(context.Background(), r.root, 1<<16,
				"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false",
				"commit", "--allow-empty", "-m", "x")
			if err != nil {
				t.Fatalf("commit: %v", err)
			}
			r.ran(t, r.hookMarker, "a commit hook")
		})
	}
}

func TestRunGitRunsNoFsmonitorProgram(t *testing.T) {
	r := newHookRepo(t, false)
	if _, _, err := runGit(context.Background(), r.root, 1<<16, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	r.ran(t, r.monitorMarker, "the repository's fsmonitor program")
}
