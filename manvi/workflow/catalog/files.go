package catalog

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

func (s Store) open(create bool) (*os.Root, error) {
	if strings.TrimSpace(s.Root) == "" {
		return nil, errors.New("catalog root required")
	}
	if create {
		if err := os.MkdirAll(s.Root, 0700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(s.Root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("catalog root must be a real directory")
	}
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, errors.New("catalog root changed during open")
	}
	return root, nil
}
func openChild(root *os.Root, name string, create bool) (*os.Root, error) {
	if !segment.MatchString(name) {
		return nil, errors.New("invalid catalog directory")
	}
	if create {
		if err := root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("catalog identity must be a real directory")
	}
	child, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := child.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		child.Close()
		return nil, errors.New("catalog directory changed during open")
	}
	return child, nil
}
func readRegular(root *os.Root, name string) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > workflow.MaxArtifactBytes {
		return nil, errors.New("catalog artifact must be a bounded regular file")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("catalog file changed during open")
	}
	raw, err := io.ReadAll(io.LimitReader(f, workflow.MaxArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > workflow.MaxArtifactBytes {
		return nil, errors.New("catalog artifact exceeds size limit")
	}
	return raw, nil
}
func directoryEntries(root *os.Root) ([]os.DirEntry, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(4097)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > 4096 {
		return nil, errors.New("catalog directory exceeds 4096 entries")
	}
	return entries, nil
}

// Publish only after the full bytes have reached a synchronized staging file.
// Link provides atomic non-replacement for immutable revisions on every OS.
func publish(root *os.Root, name string, raw []byte, replace bool) error {
	staged := ".staged-" + rand.Text()
	f, err := root.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(staged)
	n, writeErr := f.Write(raw)
	if writeErr == nil && n != len(raw) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	if replace {
		err = root.Rename(staged, name)
	} else {
		err = root.Link(staged, name)
	}
	if err != nil {
		return err
	}
	// Syncing the staged file put the bytes on the disk; syncing the directory
	// is what puts the name there. Without it a power loss can leave the
	// revision (or the current pointer) published as an unreferenced inode.
	if err := syncDir(root); err != nil {
		return fmt.Errorf("catalog: syncing directory after publishing %s: %w", name, err)
	}
	return nil
}

// syncDir flushes a catalog directory's own entries. It is a variable so a
// test can prove publish calls it and fails when it fails; a crash cannot be
// reproduced in a test.
var syncDir = func(root *os.Root) error {
	if runtime.GOOS == "windows" {
		// A directory handle cannot be flushed there (see llm/budget/lock_windows.go).
		return nil
	}
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
