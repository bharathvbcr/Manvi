//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO (mode 0755) named like the agent passes exec.LookPath's mode test. The
// resolver's launch-shape read must not block on it, and must not return it.
func TestAdvW16ReleaseFIFONamedLikeTheAgentDoesNotHangOrResolve(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			managedTrustWorkspace(t)
			trusted := t.TempDir()
			fifo := filepath.Join(trusted, a.name)
			if err := syscall.Mkfifo(fifo, 0o755); err != nil {
				t.Skip(err)
			}
			t.Setenv("PATH", trusted)
			done := make(chan string, 1)
			go func() { done <- toolBinary(a.env, a.name) }()
			select {
			case got := <-done:
				if got != unresolvedTool(a.name) {
					t.Fatalf("toolBinary(%s) = %q for a FIFO, want %q", a.name, got, unresolvedTool(a.name))
				}
			case <-time.After(10 * time.Second):
				t.Fatal("toolBinary blocked on a FIFO named like the agent")
			}
		})
	}
}

// A FIFO as an install-dir version entry.
func TestAdvW16ReleaseFIFOVersionEntryDoesNotHang(t *testing.T) {
	_, home := managedTrustWorkspace(t)
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(versions, "9.9.9"), 0o755); err != nil {
		t.Skip(err)
	}
	want := filepath.Join(versions, "2.1.1")
	writeExecFile(t, want, fakeMachO())
	t.Setenv("PATH", t.TempDir())
	done := make(chan string, 1)
	go func() { done <- toolBinary("MANVI_CLAUDE_BINARY", "claude") }()
	select {
	case got := <-done:
		if got != resolvedPath(t, want) {
			t.Fatalf("toolBinary(claude) = %q, want %q past the FIFO", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("toolBinary blocked on a FIFO version entry")
	}
}
