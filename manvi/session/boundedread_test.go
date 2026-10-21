package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func savedSession(t *testing.T) (*Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	s := NewStore(dir)
	log := NewLog()
	if _, err := log.Append(TurnStart, nil); err != nil {
		t.Fatal(err)
	}
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(id, log); err != nil {
		t.Fatal(err)
	}
	_, path, err := s.current(id)
	if err != nil || path == "" {
		t.Fatalf("current: %q %v", path, err)
	}
	return s, id, path
}

func TestLoadRefusesASymlinkedSessionFile(t *testing.T) {
	s, id, path := savedSession(t)
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := s.Load(id); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a symlinked session loaded: %v", err)
	}
}

func TestLoadRefusesAnOversizeSessionFileWithoutReadingIt(t *testing.T) {
	s, id, path := savedSession(t)
	if err := os.Truncate(path, maxLoadBytes+1); err != nil { // sparse: no real disk is spent
		t.Fatal(err)
	}
	if _, _, err := s.Load(id); err == nil || !strings.Contains(err.Error(), "load bound") {
		t.Fatalf("an oversize session loaded: %v", err)
	}
}
