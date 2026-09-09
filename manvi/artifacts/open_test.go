package artifacts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKernelNoFollowPreservesExistingTargetContents(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("preserve this existing content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeContained(link, []byte("replacement"), 0600); err == nil {
		t.Fatal("link write accepted")
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "preserve this existing content" {
		t.Fatalf("target modified: %q %v", raw, err)
	}
}
func TestVerifiedRegularHandleTruncatesOldSuffix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := writeContained(path, []byte("long original content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeContained(path, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "new" {
		t.Fatalf("regular update failed: %q %v", raw, err)
	}
}
