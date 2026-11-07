package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// hookRepo is a repository whose hooks and fsmonitor program each write their
// own marker file, so a test can tell which of them git ran.
func hookRepo(t *testing.T, tracked bool) (root, hookMarker, monitorMarker string) {
	t.Helper()
	root = gitRepo(t)
	markers := t.TempDir() // outside the work tree, so the markers never show as changes
	hookMarker, monitorMarker = filepath.Join(markers, "hook"), filepath.Join(markers, "monitor")

	hooks := filepath.Join(root, ".git", "hooks")
	if tracked {
		hooks = filepath.Join(root, ".husky")
		runGit(t, root, "config", "core.hooksPath", ".husky")
	}
	if err := os.MkdirAll(hooks, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-commit", "prepare-commit-msg", "post-commit", "post-index-change"} {
		script := "#!/bin/sh\ntouch '" + hookMarker + "'\n"
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(script), 0o755); err != nil { //nolint:gosec // a hook has to be executable
			t.Fatal(err)
		}
	}
	monitor := filepath.Join(markers, "monitor.sh")
	if err := os.WriteFile(monitor, []byte("#!/bin/sh\ntouch '"+monitorMarker+"'\n"), 0o755); err != nil { //nolint:gosec // an fsmonitor program has to be executable
		t.Fatal(err)
	}
	runGit(t, root, "config", "core.fsmonitor", monitor)
	return root, hookMarker, monitorMarker
}

// The project root is the operator's repository, but a tracked hook directory
// in it is writable by the agent. bootstrap's git helper runs on the host, so
// whatever subcommand it is given must not find a hook.
func TestGitHelperFindsNoRepositoryHook(t *testing.T) {
	for name, tracked := range map[string]bool{"tracked core.hooksPath dir": true, "dot-git hooks": false} {
		t.Run(name, func(t *testing.T) {
			root, hookMarker, _ := hookRepo(t, tracked)
			if _, err := git(context.Background(), root,
				"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false",
				"commit", "--allow-empty", "-m", "x"); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if _, err := os.Stat(hookMarker); err == nil {
				t.Error("a repository hook ran from the bootstrap git helper")
			}
		})
	}
}

func TestGitHelperRunsNoFsmonitorProgram(t *testing.T) {
	root, _, monitorMarker := hookRepo(t, false)
	if _, err := git(context.Background(), root, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(monitorMarker); err == nil {
		t.Error("the repository's fsmonitor program ran from the bootstrap git helper")
	}
}
