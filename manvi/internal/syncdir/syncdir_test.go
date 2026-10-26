package syncdir

import (
	"os"
	"path/filepath"
	"testing"
)

// The three entry points flush a real directory without error, and a file
// published into it stays there.
func TestFlushesARealDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Dir(dir); err != nil {
		t.Fatalf("Dir: %v", err)
	}

	d, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if err := File(d); err != nil {
		t.Fatalf("File: %v", err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := Root(root); err != nil {
		t.Fatalf("Root: %v", err)
	}
}

// A directory that is not there is an error where a flush is possible: "nothing
// to flush" must not read as "flushed". Where the platform cannot flush at all
// the call is a documented no-op and nothing is opened.
func TestMissingDirectoryIsNotMistakenForFlushed(t *testing.T) {
	err := Dir(filepath.Join(t.TempDir(), "absent"))
	if supported && err == nil {
		t.Fatal("Dir on a missing directory returned nil")
	}
	if !supported && err != nil {
		t.Fatalf("Dir on a platform that cannot flush must be a no-op, got %v", err)
	}
}

// A closed handle surfaces the flush's failure where there is a flush, which is
// what lets a caller see a failed directory sync at all.
func TestFileReportsAFailedFlush(t *testing.T) {
	d, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	err = File(d)
	if supported && err == nil {
		t.Fatal("File on a closed handle returned nil")
	}
	if !supported && err != nil {
		t.Fatalf("File on a platform that cannot flush must be a no-op, got %v", err)
	}
}
