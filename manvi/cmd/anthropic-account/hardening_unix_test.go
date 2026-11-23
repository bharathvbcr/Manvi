//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// unblockPipe releases any opener blocked on a named pipe by holding it open
// for read and write.
func unblockPipe(p string) {
	// #nosec G304 -- p is a pipe this test created under t.TempDir().
	if f, openErr := os.OpenFile(p, syscall.O_RDWR|syscall.O_NONBLOCK, 0); openErr == nil {
		_ = f.Close()
	}
}

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a named pipe here: %v", err)
	}
}

func TestAMarkerThatIsASymlinkToAPipeDoesNotBlockTheLauncher(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "proj")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(t.TempDir(), "pipe")
	mkfifo(t, pipe)
	marker := filepath.Join(repo, markerName)
	if err := os.Symlink(pipe, marker); err != nil {
		t.Fatal(err)
	}
	_, name, err := findMarkerWithin(t, repo, home, pipe)
	if err == nil {
		t.Fatalf("a symlinked marker was honoured (account %q); it must be refused", name)
	}
	if !strings.Contains(err.Error(), marker) {
		t.Errorf("the refusal does not name the marker: %v", err)
	}
}

func TestAMarkerThatIsAPipeDoesNotBlockTheLauncher(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "proj")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(repo, markerName)
	mkfifo(t, marker)
	_, _, err := findMarkerWithin(t, repo, home, marker)
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("a pipe marker: err = %v, want a refusal saying it is not a regular file", err)
	}
}

func TestAMarkerThatIsASymlinkToARegularFileIsRefused(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "proj")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	write(t, target, "alt\n")
	if err := os.Symlink(target, filepath.Join(repo, markerName)); err != nil {
		t.Fatal(err)
	}
	_, _, err := findMarkerWithin(t, repo, home)
	if err == nil {
		t.Fatal("a marker reached through a symlink was honoured; the marker chooses the billed organisation and must be read where it sits")
	}
	if !errors.Is(err, syscall.ELOOP) && !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("err = %v, want the refusal to say the marker is a symbolic link", err)
	}
}

// TestTheWalkStopsAtHomeEvenWhenHomeIsReachedThroughASymlink: the walk used to
// compare two textual paths, so a working directory spelled through a symlink
// to home never equalled home and the walk read the marker in home itself,
// which the registry's default is supposed to own.
func TestTheWalkStopsAtHomeEvenWhenHomeIsReachedThroughASymlink(t *testing.T) {
	realHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(realHome, markerName), "alt\n")
	if err := os.MkdirAll(filepath.Join(realHome, "Code"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "homelink")
	if err := os.Symlink(realHome, link); err != nil {
		t.Fatal(err)
	}
	path, name, err := findMarkerWithin(t, filepath.Join(link, "Code"), realHome)
	if err != nil || path != "" || name != "" {
		t.Fatalf("a marker in home was honoured through a symlink: path=%q name=%q err=%v", path, name, err)
	}
}
