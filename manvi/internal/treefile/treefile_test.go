package treefile

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/internal/filelock"
)

func tree(t *testing.T) (anchor, outside string) {
	t.Helper()
	base := t.TempDir()
	anchor = filepath.Join(base, "anchor")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{anchor, outside} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return anchor, outside
}

func link(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
}

func isEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("%s is not empty: %v %v", dir, entries, err)
	}
}

func TestOpenDirTrustsTheAnchorAndChecksWhatIsBelowIt(t *testing.T) {
	anchor, outside := tree(t)
	alias := filepath.Join(t.TempDir(), "alias")
	link(t, anchor, alias)
	if err := os.Mkdir(filepath.Join(anchor, "state"), 0o750); err != nil {
		t.Fatal(err)
	}
	// The anchor is the caller's to vouch for, so a link there is followed.
	root, err := OpenDir(alias, "state", 0)
	if err != nil {
		t.Fatalf("a linked anchor was refused: %v", err)
	}
	_ = root.Close()

	link(t, outside, filepath.Join(anchor, "linked"))
	for _, rel := range []string{"linked", "linked/deeper", filepath.Join("state", "..", "linked")} {
		if root, err := OpenDir(anchor, rel, 0o750); err == nil {
			_ = root.Close()
			t.Errorf("OpenDir(%q) passed through a symbolic link", rel)
		}
	}
	isEmpty(t, outside)
}

func TestDescendRefusesALinkInTheMiddleAndCreatesNothingBeyondIt(t *testing.T) {
	anchor, outside := tree(t)
	link(t, outside, filepath.Join(anchor, "mid"))
	_, err := OpenDir(anchor, "mid/a/b", 0o750)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("got %v, want a refusal", err)
	}
	isEmpty(t, outside)
}

func TestDescendCreatesBelowTheRootAndRefusesFilesAndEscapes(t *testing.T) {
	anchor, _ := tree(t)
	root, err := OpenDir(anchor, "a/b/c", 0o750)
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	if info, err := os.Stat(filepath.Join(anchor, "a", "b", "c")); err != nil || !info.IsDir() {
		t.Fatalf("directories were not created: %v", err)
	}
	if err := os.WriteFile(filepath.Join(anchor, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"file", "file/x", "..", "../x", "/etc"} {
		if root, err := OpenDir(anchor, rel, 0o750); err == nil {
			_ = root.Close()
			t.Errorf("OpenDir(%q) succeeded", rel)
		}
	}
	if _, err := OpenDir(anchor, "missing", 0); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing directory without create: %v", err)
	}
	dot, err := OpenDir(anchor, ".", 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = dot.Close()
}

func TestChildRefusesAnythingButASegment(t *testing.T) {
	anchor, _ := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"", ".", "..", "a/b", "a\\b"} {
		if _, err := Child(root, name, 0o750); !errors.Is(err, ErrRefused) {
			t.Errorf("Child(%q) = %v, want a refusal", name, err)
		}
	}
}

func TestReadFileBoundsAndRefusals(t *testing.T) {
	anchor, outside := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(anchor, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	write("ok.md", "hello")
	write("big.md", strings.Repeat("x", 100))
	write("naïve\nname.md", "odd name")
	link(t, secret, filepath.Join(anchor, "out.md"))
	link(t, "ok.md", filepath.Join(anchor, "in.md"))
	link(t, filepath.Join(outside, "absent"), filepath.Join(anchor, "dangling.md"))
	if err := os.Mkdir(filepath.Join(anchor, "dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(anchor, "hard.md")); err != nil {
		t.Skipf("hard links are not available: %v", err)
	}
	o := ReadOptions{Limit: 50}

	if got, err := ReadFile(root, "ok.md", o); err != nil || string(got) != "hello" {
		t.Fatalf("ok.md: %q %v", got, err)
	}
	if got, err := ReadFile(root, "naïve\nname.md", o); err != nil || string(got) != "odd name" {
		t.Fatalf("odd name: %q %v", got, err)
	}
	if _, err := ReadFile(root, "absent.md", o); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("absent: %v", err)
	}
	if _, err := ReadFile(root, "big.md", o); !errors.Is(err, ErrTooLarge) || !errors.Is(err, ErrRefused) {
		t.Errorf("oversized: %v", err)
	}
	for _, name := range []string{"out.md", "in.md", "dir"} {
		got, err := ReadFile(root, name, o)
		if !errors.Is(err, ErrRefused) || len(got) != 0 {
			t.Errorf("%s: %q %v, want a refusal", name, got, err)
		}
	}
	if _, err := ReadFile(root, "dangling.md", o); !errors.Is(err, ErrRefused) || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dangling link: %v, want a refusal that is not an absence", err)
	}
	if got, err := ReadFile(root, "hard.md", o); err != nil || string(got) != "SECRET" {
		t.Errorf("a hard link is read when the caller did not ask for single-link: %q %v", got, err)
	}
	if got, err := ReadFile(root, "hard.md", ReadOptions{Limit: 50, SingleLink: true}); !errors.Is(err, ErrRefused) || len(got) != 0 {
		t.Errorf("hard link with SingleLink: %q %v", got, err)
	}
	if _, err := ReadFile(root, "ok.md", ReadOptions{}); err == nil {
		t.Error("a read with no limit was accepted")
	}
	if err := Check(root, "ok.md", o); err != nil {
		t.Errorf("Check(ok.md): %v", err)
	}
	if err := Check(root, "out.md", o); !errors.Is(err, ErrRefused) {
		t.Errorf("Check(out.md): %v", err)
	}
}

func TestPublishCreatesReplacesAndLeavesNoStage(t *testing.T) {
	anchor, _ := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := Publish(root, "a.json", []byte("one"), PublishOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Publish(root, "a.json", []byte("two"), PublishOptions{}); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("an exclusive publish over an existing name: %v", err)
	}
	if err := Publish(root, "a.json", []byte("three"), PublishOptions{Replace: true}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(anchor, "a.json"))
	if err != nil || string(raw) != "three" {
		t.Fatalf("content %q %v", raw, err)
	}
	entries, _ := os.ReadDir(anchor)
	if len(entries) != 1 {
		t.Fatalf("a stage was left behind: %v", entries)
	}
}

func TestPublishNeverWritesThroughALinkOrOverANonFile(t *testing.T) {
	anchor, outside := tree(t)
	target := filepath.Join(outside, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link(t, target, filepath.Join(anchor, "l"))
	if err := os.Mkdir(filepath.Join(anchor, "d"), 0o750); err != nil {
		t.Fatal(err)
	}
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"l", "d", "x/y"} {
		if err := Publish(root, name, []byte("planted"), PublishOptions{Replace: true}); err == nil {
			t.Errorf("Publish(%q) succeeded", name)
		}
	}
	if raw, _ := os.ReadFile(target); string(raw) != "keep" {
		t.Fatalf("the link's target was written: %q", raw)
	}
}

func TestPublishedButUnsyncedIsASentinelWithTheNameInPlace(t *testing.T) {
	anchor, _ := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	fail := PublishOptions{SyncDir: func(*os.Root) error { return errors.New("disk gone") }}
	for _, replace := range []bool{false, true} {
		fail.Replace = replace
		name := map[bool]string{false: "link", true: "rename"}[replace]
		err := Publish(root, name, []byte("x"), fail)
		if !errors.Is(err, ErrPublishedUnsynced) || !strings.Contains(err.Error(), "disk gone") {
			t.Fatalf("%s: %v", name, err)
		}
		if _, statErr := os.Stat(filepath.Join(anchor, name)); statErr != nil {
			t.Errorf("%s: reported unsynced but not published: %v", name, statErr)
		}
	}
	entries, _ := os.ReadDir(anchor)
	if len(entries) != 2 {
		t.Errorf("a stage was left behind: %v", entries)
	}
}

func TestAFailedFileSyncPublishesNothing(t *testing.T) {
	anchor, _ := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	err = Publish(root, "a", []byte("x"), PublishOptions{SyncFile: func(*os.File) error { return errors.New("fsync failed") }})
	if err == nil || errors.Is(err, ErrPublishedUnsynced) {
		t.Fatalf("got %v, want a plain failure", err)
	}
	isEmpty(t, anchor)
}

func TestExactPermIsAppliedDespiteTheUmask(t *testing.T) {
	anchor, _ := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := Publish(root, "m", []byte("x"), PublishOptions{Perm: 0o664, ExactPerm: true}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(anchor, "m"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o664 {
		t.Fatalf("mode %v, want 0664", got)
	}
}

func TestLockExcludesAnotherHolderWhateverTheFilesAge(t *testing.T) {
	anchor, _ := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	unlock, err := Lock(t.Context(), root, ".l")
	if err != nil {
		if errors.Is(err, filelock.ErrUnsupported) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(anchor, ".l"), old, old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if second, err := Lock(ctx, root, ".l"); err == nil {
		second()
		t.Fatal("an old lock file was taken from its holder")
	}
	unlock()
	again, err := Lock(t.Context(), root, ".l")
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	again()
	if _, err := os.Lstat(filepath.Join(anchor, ".l")); err != nil {
		t.Errorf("the lock file was removed: %v", err)
	}
}

func TestLockRefusesALinkAtItsName(t *testing.T) {
	anchor, outside := tree(t)
	link(t, filepath.Join(outside, "victim"), filepath.Join(anchor, ".l"))
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if unlock, err := Lock(t.Context(), root, ".l"); err == nil {
		unlock()
		t.Fatal("a lock was taken through a link")
	}
	isEmpty(t, outside)
}

// Several writers meet at a lock file's first use. An O_CREATE open racing
// another creator of the same name can fail with a spurious ENOENT, so first use
// is exercised under contention, on a fresh name each round.
func TestLockFirstUseUnderContentionNeverFails(t *testing.T) {
	anchor, _ := tree(t)
	for round := 0; round < 40; round++ {
		name := "." + strings.Repeat("l", round+1) + ".lock"
		var wg sync.WaitGroup
		for p := 0; p < 8; p++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				root, err := OpenDir(anchor, ".", 0)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = root.Close() }()
				unlock, err := Lock(t.Context(), root, name)
				if err != nil {
					t.Errorf("round %d: %v", round, err)
					return
				}
				unlock()
			}()
		}
		wg.Wait()
	}
}
