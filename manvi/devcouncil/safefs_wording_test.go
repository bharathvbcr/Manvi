package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A refusal at pin time says which operation it refused. The leaf-symlink
// refusal used to say "write" for a read too, because every operation pins
// through the same function, and a person told they were refused a write on a
// file they only wanted read had nothing to act on.
func TestPinTimeSymlinkRefusalNamesTheOperation(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real.md"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.md", filepath.Join(root, "link.md")); err != nil {
		t.Skipf("symbolic links are not available here: %v", err)
	}

	_, err = ReadPinned(root, "link.md", 1024)
	if err == nil {
		t.Fatal("a read through a link was allowed")
	}
	for _, want := range []string{"refusing to read", `"link.md"`, "symbolic link", "Replace the link with the file itself"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("read refusal lacks %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "write") {
		t.Errorf("a refused read speaks of writing: %v", err)
	}

	err = WritePinned(root, "link.md", []byte("y"), 0o600)
	if err == nil || !strings.Contains(err.Error(), "refusing to write through") {
		t.Fatalf("the write refusal changed: %v", err)
	}
	err = RemovePinned(root, "link.md")
	if err == nil || !strings.Contains(err.Error(), "refusing to remove through") {
		t.Fatalf("a refused removal does not say it is one: %v", err)
	}
}
