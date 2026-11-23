package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCaptureFileIsOwnerOnlyEvenWhenItAlreadyExists(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX permission bits")
	}
	path := filepath.Join(t.TempDir(), "gemini-wire.log")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openCapture(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != 0 {
		t.Fatalf("capture mode %v size %d, want 0600 and truncated", info.Mode().Perm(), info.Size())
	}
	fresh, err := openCapture(filepath.Join(t.TempDir(), "new.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	if info, err := fresh.Stat(); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("new capture mode %v, %v", info.Mode().Perm(), err)
	}
}
