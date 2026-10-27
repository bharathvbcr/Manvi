package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pinned reader shares its pinning with the writer, so a link at the leaf was
// reported as a refusal to write through it. A refused read says it is a read.
func TestReadPinnedNamesAReadWhenALinkRefusesIt(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("AGENTS.md", filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	_, err = ReadPinned(root, "CLAUDE.md", 1024)
	if err == nil {
		t.Fatal("a link was read through")
	}
	if !strings.Contains(err.Error(), "refusing to read through") || strings.Contains(err.Error(), "write") {
		t.Fatalf("ReadPinned = %v, want a refusal that names a read", err)
	}
	if err := WritePinned(root, "CLAUDE.md", []byte("x"), 0o600); err == nil || !strings.Contains(err.Error(), "refusing to write through") {
		t.Fatalf("WritePinned = %v, want a refusal that still names a write", err)
	}
}
