//go:build unix

package treefile

// Adversarial tests for the w23 containment change (lane w23-containment-adv).
// Files are prefixed zadv_w23_. A test whose name ends in Defect documents a
// real defect and is expected to FAIL on the w23-containment tip.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/safefile"
)

func zadvDirs(t *testing.T) (anchor, outside string) {
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

func zadvWithin(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s blocked for more than %s", what, d)
	}
}

// A link at the first, a middle and the last component of a descent is refused
// and nothing is created through it.
func TestZadvDescendRefusesALinkAtEveryComponent(t *testing.T) {
	for _, at := range []string{"a", "a/b", "a/b/c"} {
		t.Run(strings.ReplaceAll(at, "/", "_"), func(t *testing.T) {
			anchor, outside := zadvDirs(t)
			if err := os.MkdirAll(filepath.Join(anchor, "a", "b", "c"), 0o750); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(anchor, filepath.FromSlash(at))
			if err := os.RemoveAll(victim); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, victim); err != nil {
				t.Skip(err)
			}
			root, err := OpenDir(anchor, "a/b/c", 0o750)
			if err == nil {
				_ = root.Close()
				t.Fatal("a descent through a link succeeded")
			}
			if !errors.Is(err, ErrRefused) && !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Fatalf("something was created through the link: %v", entries)
			}
		})
	}
}

// The entry d is flipped between a real directory and a link out of the tree as
// fast as possible (dir away, pre-made link in, link away, dir back: each step
// is an atomic rename onto a name that is free) while Child runs in a loop.
// Every handle Child returns must be the real directory: a probe written through
// it never reaches outside. The test also requires that the attack fired, so it
// cannot pass vacuously: some Child calls must have been refused as a link and
// some must have succeeded.
func TestZadvChildSurvivesATOCTOUSwapLoop(t *testing.T) {
	anchor, outside := zadvDirs(t)
	d := filepath.Join(anchor, "d")
	if err := os.Mkdir(d, 0o750); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var cycles atomic.Int32
	wg.Add(1)
	go func() {
		defer wg.Done()
		parked, lnk := filepath.Join(anchor, "parked"), filepath.Join(anchor, "lnk")
		for {
			select {
			case <-stop:
				return
			default:
			}
			if os.Symlink(outside, lnk) != nil {
				return
			}
			if os.Rename(d, parked) != nil {
				return
			}
			if os.Rename(lnk, d) != nil {
				return
			}
			cycles.Add(1)
			runtime.Gosched()
			if os.Rename(d, lnk) != nil {
				return
			}
			if os.Rename(parked, d) != nil {
				return
			}
			_ = os.Remove(lnk)
		}
	}()
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	var refused, opened int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		child, err := Child(root, "d", 0)
		if err != nil {
			if errors.Is(err, ErrRefused) {
				refused++
			}
			continue
		}
		opened++
		f, err := child.OpenFile("probe", os.O_WRONLY|os.O_CREATE, 0o600)
		if err == nil {
			_ = f.Close()
		}
		_ = child.Close()
	}
	close(stop)
	wg.Wait()
	if es, _ := os.ReadDir(outside); len(es) != 0 {
		t.Fatalf("a handle Child returned wrote outside the tree: %v", es)
	}
	if cycles.Load() == 0 || refused == 0 || opened == 0 {
		t.Fatalf("the attack did not fire: swap cycles=%d refused=%d opened=%d", cycles.Load(), refused, opened)
	}
	t.Logf("swap cycles=%d, Child refused=%d opened=%d", cycles.Load(), refused, opened)
}

// A handle follows its directory when the directory is renamed; a later
// Publish through it lands in the renamed directory and never re-creates the old
// path.
func TestZadvRenamingTheDirectoryWhileOpenDoesNotRedirectWrites(t *testing.T) {
	anchor, _ := zadvDirs(t)
	if err := os.Mkdir(filepath.Join(anchor, "store"), 0o750); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenDir(anchor, "store", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if err := os.Rename(filepath.Join(anchor, "store"), filepath.Join(anchor, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := Publish(dir, "x", []byte("1"), PublishOptions{}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := os.Stat(filepath.Join(anchor, "store")); err == nil {
		t.Fatal("the old path was re-created")
	}
	if _, err := os.Stat(filepath.Join(anchor, "moved", "x")); err != nil {
		t.Fatalf("the write did not follow the handle: %v", err)
	}
}

// FIFO, socket, a link to /dev/zero, a sparse file over the limit, a hard link:
// none may block, read without bound, or be accepted.
func TestZadvReadFileHostileEntries(t *testing.T) {
	anchor, outside := zadvDirs(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	opts := ReadOptions{Limit: 1 << 16, SingleLink: true}

	if err := syscall.Mkfifo(filepath.Join(anchor, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	zadvWithin(t, 3*time.Second, "ReadFile of a FIFO", func() {
		if _, err := ReadFile(root, "fifo", opts); !errors.Is(err, ErrRefused) {
			t.Errorf("FIFO: err = %v, want ErrRefused", err)
		}
	})

	short, err := os.MkdirTemp("/tmp", "zadv")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = os.RemoveAll(short) }()
	l, err := net.Listen("unix", filepath.Join(short, "s"))
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = l.Close() }()
	sockRoot, err := Open(short)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sockRoot.Close() }()
	zadvWithin(t, 3*time.Second, "ReadFile of a socket", func() {
		if _, err := ReadFile(sockRoot, "s", opts); err == nil {
			t.Error("a socket was read")
		}
	})

	if err := os.Symlink("/dev/zero", filepath.Join(anchor, "zero")); err != nil {
		t.Skip(err)
	}
	zadvWithin(t, 3*time.Second, "ReadFile of a link to /dev/zero", func() {
		if _, err := ReadFile(root, "zero", opts); !errors.Is(err, ErrRefused) {
			t.Errorf("/dev/zero link: err = %v, want ErrRefused", err)
		}
	})

	big, err := os.Create(filepath.Join(anchor, "big"))
	if err != nil {
		t.Fatal(err)
	}
	if err := big.Truncate(1 << 40); err != nil {
		t.Skip(err)
	}
	_ = big.Close()
	zadvWithin(t, 3*time.Second, "ReadFile of a sparse 1 TiB file", func() {
		if _, err := ReadFile(root, "big", opts); !errors.Is(err, ErrTooLarge) {
			t.Errorf("sparse file: err = %v, want ErrTooLarge", err)
		}
	})

	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(outside, "secret"), filepath.Join(anchor, "hard")); err != nil {
		t.Skip(err)
	}
	if _, err := ReadFile(root, "hard", opts); !errors.Is(err, ErrRefused) {
		t.Errorf("hard link with SingleLink: err = %v, want ErrRefused", err)
	}
}

// A name that is not one plain segment, or is too long for a stage file, is an
// error and leaves nothing behind or outside.
func TestZadvPublishHostileNames(t *testing.T) {
	anchor, outside := zadvDirs(t)
	dir, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	long := strings.Repeat("n", 250)
	for _, name := range []string{"", ".", "..", "a/b", "../x", "/abs", "a\\b", "nul\x00byte", long, strings.Repeat("n", 300)} {
		err := Publish(dir, name, []byte("x"), PublishOptions{})
		if err == nil {
			t.Errorf("Publish(%q) succeeded", name)
			continue
		}
	}
	for _, name := range []string{"new\nline", "ünï-cödé", "tab\there", strings.Repeat("n", 100)} {
		if err := Publish(dir, name, []byte("x"), PublishOptions{}); err != nil {
			t.Errorf("Publish(%q): %v", name, err)
		}
	}
	entries, _ := os.ReadDir(anchor)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a stage file was left behind: %q", e.Name())
		}
	}
	isEmpty := func(d string) {
		if es, _ := os.ReadDir(d); len(es) != 0 {
			t.Errorf("%s: %v", d, es)
		}
	}
	isEmpty(outside)
}

// A planted link or hard link at the final name is replaced, never written
// through: the file it pointed at keeps its bytes, whether Publish replaces or
// links.
func TestZadvPublishNeverWritesThroughALinkOrHardLink(t *testing.T) {
	for _, replace := range []bool{true, false} {
		t.Run(fmt.Sprintf("replace=%v", replace), func(t *testing.T) {
			anchor, outside := zadvDirs(t)
			target := filepath.Join(outside, "target")
			if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(anchor, "sym")); err != nil {
				t.Skip(err)
			}
			if err := os.Link(target, filepath.Join(anchor, "hard")); err != nil {
				t.Skip(err)
			}
			dir, err := Open(anchor)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dir.Close() }()
			for _, name := range []string{"sym", "hard"} {
				_ = Publish(dir, name, []byte("PWNED"), PublishOptions{Replace: replace})
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != "precious" {
				t.Fatalf("the outside file was written through: %q %v", got, err)
			}
		})
	}
}

// Case collisions: on a case-insensitive file system (APFS default) Foo and foo
// are one entry. Whatever the file system does, a non-Replace publish must not
// clobber the first, and the first must be readable intact.
func TestZadvPublishCaseFoldingNeverClobbers(t *testing.T) {
	anchor, _ := zadvDirs(t)
	dir, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	pairs := [][2]string{{"Foo", "foo"}, {"s", "\u017f"}, {"K", "\u212a"}, {"\u00e9", "e\u0301"}}
	for _, p := range pairs {
		if err := Publish(dir, p[0], []byte("first"), PublishOptions{}); err != nil {
			t.Fatalf("Publish(%q): %v", p[0], err)
		}
		err := Publish(dir, p[1], []byte("second"), PublishOptions{})
		got, rerr := ReadFile(dir, p[0], ReadOptions{Limit: 100})
		if rerr != nil {
			t.Fatalf("read %q: %v", p[0], rerr)
		}
		if err == nil && string(got) != "first" {
			// The two names are distinct files only if the file system says so.
			if _, lerr := dir.Lstat(p[1]); lerr == nil && string(got) == "second" {
				t.Errorf("Publish(%q) replaced %q without Replace", p[1], p[0])
			}
		}
	}
}

// --- Lock ---

// Many goroutines meet at the first use of one lock name, again and again, each
// round in a fresh directory: the pair (create, open) must never fail and never
// admit two holders.
func TestZadvLockGoroutineHammer(t *testing.T) {
	anchor, _ := zadvDirs(t)
	for round := 0; round < 60; round++ {
		name := fmt.Sprintf("d%d", round)
		if err := os.Mkdir(filepath.Join(anchor, name), 0o750); err != nil {
			t.Fatal(err)
		}
		dir, err := OpenDir(anchor, name, 0)
		if err != nil {
			t.Fatal(err)
		}
		var holders, violations atomic.Int32
		var wg sync.WaitGroup
		errs := make(chan error, 32)
		for g := 0; g < 24; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 3; i++ {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					unlock, err := Lock(ctx, dir, "lk")
					cancel()
					if err != nil {
						errs <- err
						return
					}
					if holders.Add(1) != 1 {
						violations.Add(1)
					}
					time.Sleep(200 * time.Microsecond)
					holders.Add(-1)
					unlock()
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: Lock failed: %v", round, err)
		}
		if violations.Load() != 0 {
			t.Fatalf("round %d: two holders at once %d times", round, violations.Load())
		}
		_ = dir.Close()
	}
}

// Root cause probe for the spurious ENOENT the implementer worked around: count
// how often a bare O_CREATE open through safefile.OpenNoFollow fails with ENOENT
// when 8 goroutines create the same new name. It is informational and must not
// fail the suite; the count is logged.
func TestZadvSpuriousENOENTRootCauseProbe(t *testing.T) {
	anchor, _ := zadvDirs(t)
	var enoent, other, ok atomic.Int32
	for round := 0; round < 300; round++ {
		name := fmt.Sprintf("f%d", round)
		dir, err := Open(anchor)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				f, err := safefile.OpenNoFollow(dir, name, os.O_RDWR|os.O_CREATE, 0o600)
				switch {
				case err == nil:
					ok.Add(1)
					_ = f.Close()
				case errors.Is(err, fs.ErrNotExist):
					enoent.Add(1)
				default:
					other.Add(1)
				}
			}()
		}
		wg.Wait()
		_ = dir.Close()
	}
	t.Logf("bare O_CREATE|O_RDWR via safefile.OpenNoFollow, 300 rounds x 8: ok=%d enoent=%d other=%d", ok.Load(), enoent.Load(), other.Load())

	// The same race with os.Root.OpenFile alone (no safefile identity check) and
	// with plain os.OpenFile, to place the ENOENT.
	var rootENOENT, plainENOENT atomic.Int32
	for round := 0; round < 300; round++ {
		name := fmt.Sprintf("g%d", round)
		dir, err := Open(anchor)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if f, err := dir.OpenFile(name, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600); err == nil {
					_ = f.Close()
				} else if errors.Is(err, fs.ErrNotExist) {
					rootENOENT.Add(1)
				}
				if f, err := os.OpenFile(filepath.Join(anchor, "p"+name), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600); err == nil {
					_ = f.Close()
				} else if errors.Is(err, fs.ErrNotExist) {
					plainENOENT.Add(1)
				}
			}()
		}
		wg.Wait()
		_ = dir.Close()
	}
	t.Logf("os.Root.OpenFile alone: enoent=%d; plain os.OpenFile: enoent=%d", rootENOENT.Load(), plainENOENT.Load())
}

// Many goroutines create the same new directory path through Descend at once
// (the first write into a fresh store): none may fail.
func TestZadvDescendCreateContention(t *testing.T) {
	anchor, _ := zadvDirs(t)
	for round := 0; round < 100; round++ {
		rel := fmt.Sprintf("r%d/a/b", round)
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d, err := OpenDir(anchor, rel, 0o750)
				if err != nil {
					errs <- err
					return
				}
				_ = d.Close()
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: concurrent create of %s failed: %v", round, rel, err)
		}
	}
}

// ReadFile racing Publish(Replace) of the same name: a reader sees the old or
// the new bytes, never a refusal or an error for a file that exists throughout.
// SingleLink is what the artifact store asks for; the catalog does not.
func TestZadvReadFileRacingReplaceNeverFailsDefect(t *testing.T) {
	for _, single := range []bool{true, false} {
		t.Run(fmt.Sprintf("single-link=%v", single), func(t *testing.T) {
			anchor, _ := zadvDirs(t)
			dir, err := Open(anchor)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dir.Close() }()
			if err := Publish(dir, "f", []byte("0"), PublishOptions{Replace: true}); err != nil {
				t.Fatal(err)
			}
			stop := make(chan struct{})
			var wg, rd sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 800; i++ {
					if err := Publish(dir, "f", []byte(strconv.Itoa(i)), PublishOptions{Replace: true}); err != nil {
						t.Errorf("Publish: %v", err)
						return
					}
				}
			}()
			var failures, reads atomic.Int32
			var first atomic.Value
			for r := 0; r < 4; r++ {
				rd.Add(1)
				go func() {
					defer rd.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						reads.Add(1)
						if _, err := ReadFile(dir, "f", ReadOptions{Limit: 100, SingleLink: single}); err != nil {
							failures.Add(1)
							first.CompareAndSwap(nil, err.Error())
						}
					}
				}()
			}
			wg.Wait()
			close(stop)
			rd.Wait()
			if failures.Load() != 0 {
				t.Fatalf("%d of %d reads of a file that existed throughout failed; first: %v", failures.Load(), reads.Load(), first.Load())
			}
		})
	}
}

// A lock file that is a directory, a link or a FIFO is an error and never a
// hang.
func TestZadvLockHostileLockFiles(t *testing.T) {
	anchor, outside := zadvDirs(t)
	dir, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if err := os.Mkdir(filepath.Join(anchor, "isdir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(anchor, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "gone"), filepath.Join(anchor, "dangling")); err != nil {
		t.Skip(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "real"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "real"), filepath.Join(anchor, "linked")); err != nil {
		t.Skip(err)
	}
	for _, name := range []string{"isdir", "fifo", "dangling", "linked"} {
		zadvWithin(t, 5*time.Second, "Lock on "+name, func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			unlock, err := Lock(ctx, dir, name)
			if err == nil {
				unlock()
				t.Errorf("Lock(%s) succeeded", name)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(outside, "gone")); err == nil {
		t.Error("a lock was created through a dangling link")
	}
}

// A third party deletes (or atomically replaces) the lock file while it is held.
// The holder still holds; a second Lock must not succeed.
func TestZadvLockDeletedWhileHeldDefect(t *testing.T) {
	for _, how := range []string{"remove", "rename-over"} {
		t.Run(how, func(t *testing.T) {
			anchor, _ := zadvDirs(t)
			dir, err := Open(anchor)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dir.Close() }()
			first, err := Lock(context.Background(), dir, "lk")
			if err != nil {
				t.Fatal(err)
			}
			defer first()
			switch how {
			case "remove":
				if err := os.Remove(filepath.Join(anchor, "lk")); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(filepath.Join(anchor, "new"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(anchor, "new"), filepath.Join(anchor, "lk")); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			second, err := Lock(ctx, dir, "lk")
			if err == nil {
				second()
				t.Fatal("a second holder took the lock while the first still held it (lock file replaced under it)")
			}
		})
	}
}

// --- real processes ---

const zadvHelperEnv = "ZADV_W23_TREEFILE_HELPER"

// TestZadvHelperProcess is the child of the multi-process tests. It is inert
// unless the helper env var is set.
func TestZadvHelperProcess(t *testing.T) {
	mode := os.Getenv(zadvHelperEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	dirPath, log := os.Getenv("ZADV_DIR"), os.Getenv("ZADV_LOG")
	dir, err := Open(dirPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(2)
	}
	switch mode {
	case "contend":
		n, _ := strconv.Atoi(os.Getenv("ZADV_N"))
		for i := 0; i < n; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			unlock, err := Lock(ctx, dir, "lk")
			cancel()
			if err != nil {
				fmt.Fprintln(os.Stderr, "lock:", err)
				os.Exit(3)
			}
			lf, err := os.OpenFile(log, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
			if err != nil {
				os.Exit(4)
			}
			fmt.Fprintf(lf, "in %d\n", os.Getpid())
			time.Sleep(time.Millisecond)
			fmt.Fprintf(lf, "out %d\n", os.Getpid())
			_ = lf.Close()
			unlock()
		}
	case "hold":
		unlock, err := Lock(context.Background(), dir, "lk")
		if err != nil {
			fmt.Fprintln(os.Stderr, "lock:", err)
			os.Exit(3)
		}
		_ = unlock
		if err := os.WriteFile(log, []byte("held"), 0o600); err != nil {
			os.Exit(4)
		}
		time.Sleep(10 * time.Minute)
	}
	os.Exit(0)
}

func zadvChild(t *testing.T, mode, dir, log string, extra ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestZadvHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), zadvHelperEnv+"="+mode, "ZADV_DIR="+dir, "ZADV_LOG="+log)
	cmd.Env = append(cmd.Env, extra...)
	return cmd
}

// Eight real processes contend for one lock file that does not exist yet; their
// critical sections, recorded in one append-only log, must never interleave.
func TestZadvLockAcrossProcesses(t *testing.T) {
	anchor, _ := zadvDirs(t)
	log := filepath.Join(anchor, "log")
	var cmds []*exec.Cmd
	var outs []*strings.Builder
	for i := 0; i < 8; i++ {
		c := zadvChild(t, "contend", anchor, log, "ZADV_N=25")
		var sb strings.Builder
		c.Stdout, c.Stderr = &sb, &sb
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, c)
		outs = append(outs, &sb)
	}
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Errorf("child %d: %v\n%s", i, err, outs[i].String())
		}
	}
	f, err := os.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var open string
	var entries int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		kind, pid, _ := strings.Cut(sc.Text(), " ")
		switch kind {
		case "in":
			if open != "" {
				t.Fatalf("pid %s entered while %s was inside", pid, open)
			}
			open = pid
			entries++
		case "out":
			if open != pid {
				t.Fatalf("pid %s left but %q was inside", pid, open)
			}
			open = ""
		}
	}
	if entries != 8*25 {
		t.Fatalf("%d critical sections ran, want %d", entries, 8*25)
	}
}

// A holder killed with SIGKILL releases the lock: the next Lock gets it fast,
// and while the holder is alive nobody else gets it.
func TestZadvLockReleasedWhenHolderIsKilled(t *testing.T) {
	anchor, _ := zadvDirs(t)
	log := filepath.Join(anchor, "held")
	c := zadvChild(t, "hold", anchor, log)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pid := c.Process.Pid
	defer func() { _ = c.Process.Kill(); _, _ = c.Process.Wait() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(log); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the holder never reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
	dir, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	short, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	if unlock, err := Lock(short, dir, "lk"); err == nil {
		unlock()
		cancel()
		t.Fatalf("lock taken while pid %d holds it", pid)
	}
	cancel()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_, _ = c.Process.Wait()
	ctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	unlock, err := Lock(ctx, dir, "lk")
	if err != nil {
		t.Fatalf("the lock was not released by the kernel when its holder died: %v", err)
	}
	unlock()
}
