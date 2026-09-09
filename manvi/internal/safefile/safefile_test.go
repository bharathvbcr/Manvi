package safefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedIdentitySurvivesPathReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := PinIdentity(info)
	if !ok {
		t.Fatal("identity unavailable")
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	other, ok := PinIdentity(current)
	if !ok || id.Equal(other) {
		t.Fatal("pin followed replacement pathname")
	}
	original, err := os.Stat(path + ".old")
	if err != nil {
		t.Fatal(err)
	}
	oldID, ok := PinIdentity(original)
	if !ok || !id.Equal(oldID) {
		t.Fatal("rename changed pinned identity")
	}
}

func TestPathSnapshotMatchesAlreadyHeldDirectory(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	held, err := root.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	named, err := Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	heldID, heldOK := PinIdentity(held)
	namedID, namedOK := PinIdentity(named)
	if !heldOK || !namedOK || !heldID.Equal(namedID) {
		t.Fatal("path identity cannot be pinned while the directory is already held")
	}
}

func TestNoFollowRootAndDirectOpensPreserveLinkTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, rooted := range []bool{false, true} {
		var parent *os.Root
		name := filepath.Join(dir, "link")
		if rooted {
			parent, name = root, "link"
		}
		if f, err := OpenNoFollow(parent, name, os.O_WRONLY|os.O_CREATE, 0600); err == nil {
			f.Close()
			t.Fatalf("accepted symlink, rooted=%v", rooted)
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "original" {
		t.Fatalf("target changed: %q %v", contents, err)
	}
}

func TestLinkCountUsesOpenHandleAndRejectsEarlyTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenNoFollow(nil, path, os.O_WRONLY|os.O_TRUNC, 0600); err == nil {
		f.Close()
		t.Fatal("allowed unvalidated truncation")
	}
	f, err := OpenNoFollow(nil, path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Link(path, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	links, err := LinkCount(f)
	if err != nil || links != 2 {
		t.Fatalf("link count %d: %v", links, err)
	}
}
