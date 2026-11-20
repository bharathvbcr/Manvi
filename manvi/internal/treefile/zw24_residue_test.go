package treefile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A publish by link leaves its stage behind until the end. If the name has landed
// and the stage then cannot be removed, the write took effect: it must not read
// as a plain failure, or a caller retries a write that is already there.
func TestAFailedStageRemovalAfterTheLinkLandedStillReportsAPublishedWrite(t *testing.T) {
	anchor, _ := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	// Between the link and the removal, the stage is swapped for a directory that
	// has something in it, which Remove refuses on every platform.
	breakStage := func(dir *os.Root) error {
		d, err := dir.Open(".")
		if err != nil {
			return err
		}
		names, err := d.Readdirnames(-1)
		_ = d.Close()
		if err != nil {
			return err
		}
		for _, n := range names {
			if strings.HasPrefix(n, ".entry.") && strings.HasSuffix(n, ".tmp") {
				if err := dir.Remove(n); err != nil {
					return err
				}
				if err := dir.Mkdir(n, 0o750); err != nil {
					return err
				}
				f, err := dir.Create(filepath.Join(n, "held"))
				if err != nil {
					return err
				}
				return f.Close()
			}
		}
		return errors.New("no stage to break")
	}
	// The link fallback, which leaves the stage behind to be removed; where the OS
	// has the atomic rename there is no stage to fail to remove.
	saved := noReplaceRename
	defer func() { noReplaceRename = saved }()
	noReplaceRename = func(*os.Root, string, string) error { return errNoAtomicRename }
	err = Publish(root, "entry", []byte("payload"), PublishOptions{SyncDir: breakStage})
	if err == nil {
		t.Fatal("a stage that could not be removed was not reported at all")
	}
	if !errors.Is(err, ErrPublishedUnsynced) {
		t.Fatalf("the write took effect but reads as a failure: %v", err)
	}
	got, rerr := os.ReadFile(filepath.Join(anchor, "entry")) // #nosec G304 -- a path under this test's own t.TempDir
	if rerr != nil || string(got) != "payload" {
		t.Fatalf("the name did not land: %q, %v", got, rerr)
	}
}

// A regular file that keeps being replaced while it is opened is a race. It was
// reported as a dangling symbolic link, which it is not.
func TestARegularFileThatKeepsBeingReplacedIsNotCalledADanglingLink(t *testing.T) {
	err := unstableOpenError("f")
	if strings.Contains(err.Error(), "dangling") || errors.Is(err, ErrRefused) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a race is reported as a refusal or an absence: %v", err)
	}
	if !errors.Is(err, ErrUnstable) {
		t.Fatalf("the race has no name: %v", err)
	}

	// And on a real tree: whatever a reader of a file that is only ever a
	// regular file is told, it is never that the name is a link.
	anchor, _ := tree(t)
	path := filepath.Join(anchor, "f")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, rerr := Open(anchor)
	if rerr != nil {
		t.Fatal(rerr)
	}
	defer func() { _ = root.Close() }()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			tmp := filepath.Join(anchor, "tmp")
			if os.WriteFile(tmp, []byte("y"), 0o600) == nil {
				_ = os.Rename(tmp, path)
			}
		}
	}()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := ReadFile(root, "f", ReadOptions{Limit: 10}); err != nil &&
			(errors.Is(err, ErrRefused) || strings.Contains(err.Error(), "dangling")) {
			close(stop)
			<-done
			t.Fatalf("a regular file that is only ever replaced is reported as a link: %v", err)
		}
	}
	close(stop)
	<-done
}

// A permission failure and an exhausted descriptor table are the host's, not the
// entry's, and are not refusals.
func TestOpenFailuresThatAreTheHostsAreNotRefusals(t *testing.T) {
	for name, cause := range map[string]error{
		"EACCES": &fs.PathError{Op: "openat", Path: "f", Err: syscall.EACCES},
		"EMFILE": &fs.PathError{Op: "openat", Path: "f", Err: syscall.EMFILE},
		"ENFILE": &fs.PathError{Op: "openat", Path: "f", Err: syscall.ENFILE},
	} {
		err := openFailure("f", cause)
		if errors.Is(err, ErrRefused) {
			t.Errorf("%s was classed as a refusal: %v", name, err)
		}
		if !errors.Is(err, cause) {
			t.Errorf("%s lost its cause: %v", name, err)
		}
	}
	if err := openFailure("f", &fs.PathError{Op: "openat", Path: "f", Err: syscall.ELOOP}); !errors.Is(err, ErrRefused) {
		t.Errorf("a link met on the way is a refusal, got %v", err)
	}
}

func TestAnUnreadableFileIsNotRefusedAsHostile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	anchor, _ := tree(t)
	path := filepath.Join(anchor, "secret")
	if err := os.WriteFile(path, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	_, err = ReadFile(root, "secret", ReadOptions{Limit: 10})
	if err == nil {
		t.Skip("this platform reads a mode 000 file")
	}
	if errors.Is(err, ErrRefused) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("a permission failure is reported as %v", err)
	}
}

// Where the OS has an atomic no-replace rename a published entry never has a
// second name, however slow the directory sync: there is no window to read it in.
func TestAPublishNeverLeavesTheEntryWithASecondLink(t *testing.T) {
	anchor, _ := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := noReplaceRename(root, "absent", "alsoabsent"); errors.Is(err, errNoAtomicRename) {
		t.Skip("this platform has no atomic no-replace rename")
	}
	var seen error
	err = Publish(root, "entry", []byte("payload"), PublishOptions{SyncDir: func(*os.Root) error {
		time.Sleep(50 * time.Millisecond)
		_, seen = ReadFile(root, "entry", ReadOptions{Limit: 1 << 10, SingleLink: true})
		return nil
	}})
	if err != nil || seen != nil {
		t.Fatalf("publish %v; a read during the slow sync: %v", err, seen)
	}
	if err := Publish(root, "entry", []byte("again"), PublishOptions{}); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("publishing over an existing name = %v, want fs.ErrExist", err)
	}
}

// The link fallback has a second name until the stage is removed. A reader accepts
// that second link only as this store's own stage sibling, however long the sync
// takes, and refuses a hard link to a file outside the tree, with or without a
// stage-shaped name beside it.
func TestTheLinkFallbackIsReadableOnlyThroughItsOwnStage(t *testing.T) {
	anchor, outside := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	saved := noReplaceRename
	defer func() { noReplaceRename = saved }()
	noReplaceRename = func(*os.Root, string, string) error { return errNoAtomicRename }

	var during error
	err = Publish(root, "entry", []byte("payload"), PublishOptions{SyncDir: func(*os.Root) error {
		time.Sleep(60 * time.Millisecond) // a slow fsync, far over any fixed window
		_, during = ReadFile(root, "entry", ReadOptions{Limit: 1 << 10, SingleLink: true})
		return nil
	}})
	if err != nil || during != nil {
		t.Fatalf("publish %v; a read through its own stage: %v", err, during)
	}

	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(anchor, "planted")
	if err := os.Link(secret, planted); err != nil {
		t.Skipf("hard links are not available: %v", err)
	}
	if _, err := ReadFile(root, "planted", ReadOptions{Limit: 1 << 10, SingleLink: true}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("a hard link to a file outside the tree was read: %v", err)
	}
	// The same link with a stage-shaped name beside it is three links, not a publish.
	if err := os.Link(secret, filepath.Join(anchor, ".planted.0123456789abcdef.tmp")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(root, "planted", ReadOptions{Limit: 1 << 10, SingleLink: true}); !errors.Is(err, ErrRefused) {
		t.Fatalf("a planted stage-shaped name excused a link to a file outside the tree: %v", err)
	}
	// And a two-link file whose second name is not a stage sibling is refused.
	if err := os.WriteFile(filepath.Join(anchor, "two"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(anchor, "two"), filepath.Join(anchor, "other-name")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(root, "two", ReadOptions{Limit: 1 << 10, SingleLink: true}); !errors.Is(err, ErrRefused) {
		t.Fatalf("a second link that is not a stage sibling was accepted: %v", err)
	}
}
