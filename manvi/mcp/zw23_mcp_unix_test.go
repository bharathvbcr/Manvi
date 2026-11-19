//go:build unix

package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A manifest or config that is a link to a device must not be read.
//
// os.ReadFile sat behind an os.Stat size check, and a device reports no size.
// The FIFO is the form of it that can be run against the unfixed code without
// filling memory: its read blocks forever instead.
func TestAConfigThatIsAFIFOFailsDiscoveryWithinASecond(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	if err := os.Symlink(fifo, filepath.Join(root, "mcp.json")); err != nil {
		t.Fatal(err)
	}
	assertDiscoveryRefusesQuickly(t, root)
}

func TestAConfigThatIsALinkToDevZeroFailsDiscoveryWithinASecond(t *testing.T) {
	isolateTrust(t)
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("no /dev/zero")
	}
	root := t.TempDir()
	if err := os.Symlink("/dev/zero", filepath.Join(root, "mcp.json")); err != nil {
		t.Fatal(err)
	}
	assertDiscoveryRefusesQuickly(t, root)
}

func TestAManifestThatIsALinkIsReportedNotRead(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	secret := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(secret, []byte(`{"name":"escaped","runtime":{"command":"echo"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "plugins", "a")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "plugin.json")); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.ServerNames(); len(got) != 0 {
		t.Fatalf("a manifest reached through a link was registered: %v", got)
	}
	if got := m.Skipped(); len(got) != 1 {
		t.Fatalf("Skipped() = %+v, want the refused link named", got)
	}
}

func assertDiscoveryRefusesQuickly(t *testing.T, root string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- NewManager(root).AutoDiscover(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("discovery accepted a config that is not a regular file")
		}
	case <-time.After(time.Second):
		t.Fatal("discovery was still reading a config that is not a regular file after a second")
	}
}

// Discovery says what it could not search.
func TestAnUnreadablePluginDirectoryIsInSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 directory")
	}
	isolateTrust(t)
	root := t.TempDir()
	dir := filepath.Join(root, "plugins", "a")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) }) //nolint:gosec // a directory must be searchable again for TempDir's own removal

	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Skipped() {
		if strings.HasSuffix(s.Path, filepath.Join("plugins", "a")) {
			return
		}
	}
	t.Fatalf("Skipped() = %+v, want the unreadable directory named", m.Skipped())
}

func TestADepthPrunedSubtreeIsInSkipped(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	deep := filepath.Join(root, "plugins")
	for range maxManifestDepth + 2 {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(deep, 0o750); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Skipped() {
		if strings.Contains(s.Reason, "deeper than") {
			return
		}
	}
	t.Fatalf("Skipped() = %+v, want the pruned subtree named", m.Skipped())
}

// A server that exits and leaves a descendant holding its stdout is not alive,
// and Close removes the descendant.
//
// The leader here is sh, which backgrounds a sleep that inherits the pipes and
// exits. Before the fix nothing ever waited on the leader: end-of-stream never
// came, Alive stayed true for as long as the sleeper lived, and Close, finding
// the leader gone, never killed the group.
func TestADescendantHoldingTheStdoutDoesNotKeepAServerAlive(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "sleeper.pid")
	c, err := NewClient(ServerConfig{Name: "bg", Command: "sh",
		Args: []string{"-c", "sleep 600 & echo $! > " + pidFile + "; exit 0"}})
	if err != nil {
		t.Fatal(err)
	}
	var sleeper int
	t.Cleanup(func() {
		_ = c.Close()
		// Whatever the assertions found, the sleeper must not outlive the test.
		if sleeper > 0 {
			_ = syscall.Kill(sleeper, syscall.SIGKILL)
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for sleeper == 0 && time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil { //nolint:gosec // a path under t.TempDir()
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				sleeper = pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sleeper == 0 {
		t.Fatal("the server never started its sleeper")
	}

	select {
	case <-c.Done():
	case <-time.After(waitDelay + 3*time.Second):
		t.Fatal("a server that exited was still reported alive after waitDelay, held open by its descendant")
	}
	if c.Alive() {
		t.Fatal("Alive() is true for a server that has exited")
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	gone := false
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if err := syscall.Kill(sleeper, 0); errors.Is(err, syscall.ESRCH) {
			gone = true
			break
		}
	}
	if !gone {
		t.Fatal("the descendant outlived Close")
	}
}
