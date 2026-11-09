//go:build unix

package serve

import (
	"os"
	"path/filepath"
	"testing"
)

// A .git entry that cannot be read is no boundary: the refusal's test fails closed
// and the two checkouts overlap.
func TestUnreadableGitEntryIsNotABoundary(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	root := resolvedDir(t)
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(sub, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o750) }) //nolint:gosec // a directory needs its search bit back so TempDir can remove it
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if hasGitBoundary(rootInfo, root, sub) {
		t.Error("a .git entry that could not be read was taken as a boundary")
	}
}
