//go:build unix

package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The process-group and signal half of the MCP suite. Setpgid and kill(2) have
// no Windows spelling, which is why client.go reaches them through the
// platform files rather than directly; a test that names them directly is
// unix-only, and in a file of its own so the rest of the package's tests
// compile and run on Windows.

// --- Defect: no process group, so grandchildren outlived Close ---

// Closing a server must reach what that server started. Killing only the direct
// child left a daemon it spawned running with the pipes it inherited, and
// because the child shared this process's group there was no group to kill that
// would not also have killed the harness.
func TestClosingAServerKillsWhatItSpawned(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	c, err := NewClient(stubConfig(t, "grandchild", "grandchild", pidFile))
	if err != nil {
		t.Fatal(err)
	}

	if c.cmd.SysProcAttr == nil || !c.cmd.SysProcAttr.Setpgid {
		t.Error("the server was not given its own process group, so a group kill would reach this harness")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	pid := 0
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		data, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err == nil && pid > 0 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pid <= 0 {
		t.Skip("the stub could not start a grandchild here")
	}
	// Never leave one behind, whatever the verdict.
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	if err := syscall.Kill(pid, 0); err != nil {
		t.Skipf("the grandchild was already gone before Close: %v", err)
	}

	_ = c.Close()

	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if err := syscall.Kill(pid, 0); err != nil {
			return // gone, as it must be
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d, spawned by the MCP server, outlived Close", pid)
}
