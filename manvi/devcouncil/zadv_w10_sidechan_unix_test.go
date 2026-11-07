//go:build unix

package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Non-regular files in the tree (symlink loop, FIFO named like a credential,
// FIFO with an innocent name) must neither hang the tool nor change the reply.
func TestAdvGrepSymlinkLoopAndFIFOAreHarmlessAndPatternIndependent(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, ".env", "TOKEN=abc123\n")
	writeRepoFile(t, f.root, "src/a.go", "package src // visible\n")
	if err := os.Symlink("loop2", filepath.Join(f.root, "loop1")); err != nil {
		t.Skip(err)
	}
	if err := os.Symlink("loop1", filepath.Join(f.root, "loop2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(f.root, "src", "self")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pipe", "cfg/.env.fifo", "src/innocent.fifo"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(f.root, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(f.root, name), 0o600); err != nil {
			t.Skipf("mkfifo unavailable: %v", err)
		}
	}
	run := func(p string) string {
		done := make(chan string, 1)
		go func() { done <- advGrepRaw(t, f, p, map[string]any{"include_ignored": true}) }()
		select {
		case s := <-done:
			return s
		case <-time.After(60 * time.Second):
			t.Fatalf("grep hung on a FIFO or symlink loop (pattern %q)", p)
			return ""
		}
	}
	a, b := run(`^TOKEN=[a-m]|visible`), run(`^TOKEN=[n-z]|visible`)
	if a != b {
		t.Errorf("reply differs by pattern with loops/FIFOs present:\n  %s\n  %s", a, b)
	}
	if !strings.Contains(a, "src/a.go") || strings.Contains(a, "abc123") {
		t.Errorf("unexpected reply: %s", a)
	}
}
