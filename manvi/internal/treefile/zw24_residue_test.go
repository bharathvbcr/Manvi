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
	err = Publish(root, "entry", []byte("payload"), PublishOptions{SyncDir: breakStage})
	if err == nil {
		t.Fatal("a stage that could not be removed was not reported at all")
	}
	if !errors.Is(err, ErrPublishedUnsynced) {
		t.Fatalf("the write took effect but reads as a failure: %v", err)
	}
	if got, rerr := os.ReadFile(filepath.Join(anchor, "entry")); rerr != nil || string(got) != "payload" {
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

// A store that publishes by link has two links to the file for a moment. A read
// that lands there is not looking at a file outside the tree; one that still sees
// two afterwards is.
func TestSingleLinkWaitsForAPublishInProgressAndStillRefusesAPlantedLink(t *testing.T) {
	anchor, _ := tree(t)
	name, stage := filepath.Join(anchor, "entry"), filepath.Join(anchor, ".entry.0123456789abcdef.tmp")
	if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(name, stage); err != nil {
		t.Skipf("hard links are not available: %v", err)
	}
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	// The publish ends at the moment the reader first waits: the hook removes the
	// stage there, so the test does not depend on how fast this host schedules.
	saved := settle
	defer func() { settle = saved }()
	waits := 0
	settle = func(time.Duration) {
		waits++
		_ = os.Remove(stage)
	}
	if got, err := ReadFile(root, "entry", ReadOptions{Limit: 10, SingleLink: true}); err != nil || string(got) != "x" {
		t.Fatalf("a read racing the end of a publish was refused: %q, %v", got, err)
	}

	planted := filepath.Join(anchor, "planted")
	if err := os.Link(name, planted); err != nil {
		t.Fatal(err)
	}
	waits = 0
	settle = func(time.Duration) { waits++ }
	_, err = ReadFile(root, "entry", ReadOptions{Limit: 10, SingleLink: true})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("a hard link that stays was not refused: %v", err)
	}
	if waits != linkSettleAttempts-1 {
		t.Fatalf("a planted link was waited on %d times, want the bound %d", waits, linkSettleAttempts-1)
	}
}
