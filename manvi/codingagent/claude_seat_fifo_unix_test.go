//go:build unix

package codingagent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// .claude-account and .git live in the agent-writable checkout. A FIFO planted
// under either name must be refused or ignored, never opened: opening one for
// reading blocks until a writer appears, which would hang the launch forever.
func TestClaudeSeatFilesThatAreNotRegularFilesDoNotBlockTheLaunch(t *testing.T) {
	cases := []struct {
		name      string
		plant     func(home, repo string) string
		wantError bool
	}{
		{"marker", func(_, repo string) string { return filepath.Join(repo, ".claude-account") }, true},
		{"worktree .git", func(_, repo string) string { return filepath.Join(repo, ".git") }, false},
		{"accounts file", func(home, _ string) string { return filepath.Join(home, ".claude-accounts") }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			repo := filepath.Join(home, "Code", "Hostile")
			if err := os.MkdirAll(repo, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(tc.plant(home, repo), 0o600); err != nil {
				t.Skipf("mkfifo unavailable: %v", err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := claudeSeatOverrides(repo)
				done <- err
			}()
			select {
			case err := <-done:
				if tc.wantError && err == nil {
					t.Fatal("a FIFO was accepted as seat configuration")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("claudeSeatOverrides blocked reading a FIFO")
			}
		})
	}
}
