package devcouncil

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A file that is not there is absence, and callers that treat absence as "no
// such thing here" (the harness spec reader, the skills listing) can only tell
// it from a refusal if the error says so. The pinned reader's two spellings of
// absence, a missing leaf and a missing directory above it, both wrap
// fs.ErrNotExist.
func TestPinnedReadReportsAbsenceAsNotExist(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "present"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, rel := range map[string]string{
		"missing leaf":      "present/absent.json",
		"missing directory": "absent/dir/file.json",
	} {
		if _, err := ReadPinned(root, rel, 1024); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: ReadPinned error = %v, want one wrapping fs.ErrNotExist", name, err)
		}
		if _, err := RepoReaderAt(context.Background(), root)(rel, 1024); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: the gated repo reader reported %v, want one wrapping fs.ErrNotExist", name, err)
		}
	}
}

func TestWritePinnedCreatesReplacesAndRefusesALink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePinned(root, "NOTES.md", []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "NOTES.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WritePinned(root, "NOTES.md", []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "NOTES.md"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("an existing file's permission bits were not kept: %v", info.Mode().Perm())
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "LINK.md")); err != nil {
		t.Fatal(err)
	}
	if err := WritePinned(root, "LINK.md", []byte("through the link"), 0o644); err == nil {
		t.Error("a write through a symbolic link was accepted")
	}
	// #nosec G304 -- a file under this test's own t.TempDir.
	if got, _ := os.ReadFile(outside); string(got) != "untouched" {
		t.Errorf("the link's target was written: %q", got)
	}
}
