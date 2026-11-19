//go:build unix

package bootstrap

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO at .gitignore used to hang os.ReadFile forever, before any command the
// operator typed could run.
func TestAFifoAtGitignoreDoesNotBlockScaffolding(t *testing.T) {
	root := gitRepo(t)
	if err := syscall.Mkfifo(filepath.Join(root, ".gitignore"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan Report, 1)
	go func() { done <- Ensure(root, options()) }()
	select {
	case report := <-done:
		if len(report.Failures) == 0 || len(report.AddedRules) != 0 {
			t.Fatalf("report = %+v, want a refusal", report)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("scaffolding blocked on a FIFO at .gitignore")
	}
}
