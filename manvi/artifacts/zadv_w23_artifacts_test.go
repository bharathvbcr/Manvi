//go:build unix

package artifacts

// Adversarial tests for the w23 containment change (lane w23-containment-adv).
// A test whose name ends in Defect documents a real defect and is expected to
// FAIL on the w23-containment tip.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/internal/filelock"
	"github.com/bharathvbcr/Manvi/manvi/internal/treefile"
)

func zadvTree(t *testing.T) (anchor, outside string) {
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

func zadvEmpty(t *testing.T, dir string) {
	t.Helper()
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Fatalf("%s is not empty: %v", dir, es)
	}
}

func zadvWithin(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s blocked for 10s", what)
	}
}

// A link at the first, a middle and the last component of the store path: every
// operation refuses and nothing lands outside.
func TestZadvStoreRootLinkAtEveryComponent(t *testing.T) {
	for _, at := range []string{"a", "a/b", "a/b/store"} {
		t.Run(strings.ReplaceAll(at, "/", "_"), func(t *testing.T) {
			anchor, outside := zadvTree(t)
			if err := os.MkdirAll(filepath.Join(anchor, "a", "b", "store"), 0o750); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(anchor, filepath.FromSlash(at))
			if err := os.RemoveAll(victim); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, victim); err != nil {
				t.Skip(err)
			}
			s, err := NewStoreUnder(anchor, "a/b/store")
			if err != nil {
				return // refused at construction is fine
			}
			if _, err := s.Create("x", "c", Metadata{}); err == nil {
				t.Error("Create wrote through a linked component")
			}
			if _, err := s.Update("x", "c", nil); err == nil {
				t.Error("Update through a linked component answered")
			}
			if _, err := s.Get("x"); err == nil {
				t.Error("Get through a linked component answered")
			}
			if arts, err := s.List(); err == nil && arts == nil {
				t.Error("List reported an empty store for a linked component (hides the refusal)")
			}
			zadvEmpty(t, outside)
		})
	}
}

// The store directory is flipped between real and a link out of the tree while
// Creates run: each cycle parks the directory under a fresh name, renames a
// pre-made link onto the store name, and later removes the link and brings the
// directory back (a Create that ran in the gap re-made the store, in which case
// the park is simply left). Nothing may ever land in the outside directory, and
// the test requires that the attack fired: some Creates must have been refused
// as a link and some must have succeeded.
func TestZadvStoreRootSwapLoop(t *testing.T) {
	anchor, outside := zadvTree(t)
	store := filepath.Join(anchor, "store")
	if err := os.Mkdir(store, 0o750); err != nil {
		t.Fatal(err)
	}
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var cycles int
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			parked, lnk := fmt.Sprintf("%s.parked%d", store, i), fmt.Sprintf("%s.lnk%d", store, i)
			if os.Symlink(outside, lnk) != nil {
				return
			}
			if os.Rename(store, parked) != nil {
				_ = os.Remove(lnk)
				continue
			}
			if os.Rename(lnk, store) != nil {
				_ = os.Remove(lnk)
				_ = os.Rename(parked, store)
				continue
			}
			cycles++
			time.Sleep(200 * time.Microsecond)
			_ = os.Remove(store)
			_ = os.Rename(parked, store)
		}
	}()
	var refused, created int
	deadline := time.Now().Add(3 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		_, err := s.Create(fmt.Sprintf("n%d", i), "c", Metadata{})
		switch {
		case err == nil:
			created++
		case errors.Is(err, treefile.ErrRefused):
			refused++
		}
		_, _ = s.Get(fmt.Sprintf("n%d", i/2))
		_, _ = s.List()
	}
	close(stop)
	wg.Wait()
	zadvEmpty(t, outside)
	if cycles == 0 || refused == 0 || created == 0 {
		t.Fatalf("the attack did not fire: swap cycles=%d refused=%d created=%d", cycles, refused, created)
	}
	t.Logf("swap cycles=%d, Create refused=%d created=%d", cycles, refused, created)
}

// A hard link in the store to a file outside: read refuses, and update and
// create-over never write through it.
func TestZadvHardLinkToAnOutsideFile(t *testing.T) {
	anchor, outside := zadvTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("doc", "v1", Metadata{}); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := filepath.Join(anchor, "store", "doc")
	if err := os.Remove(content); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, content); err != nil {
		t.Skip(err)
	}
	if _, err := s.Get("doc"); err == nil {
		t.Error("Get read a hard-linked outside file")
	}
	arts, err := s.List()
	var partial *PartialListError
	if !errors.As(err, &partial) {
		t.Fatalf("List err = %v (arts %d), want *PartialListError naming doc", err, len(arts))
	}
	// Update replaces the name (it never writes through the inode), so the
	// outside file keeps its bytes.
	if _, err := s.Update("doc", "v2", nil); err != nil {
		t.Logf("Update refused: %v", err)
	}
	if got, _ := os.ReadFile(secret); string(got) != "precious" {
		t.Fatalf("Update wrote through the hard link: %q", got)
	}
	// Sidecar hard-linked too.
	meta := filepath.Join(anchor, "store", "doc.meta.json")
	_ = os.Remove(meta)
	if err := os.Link(secret, meta); err != nil {
		t.Skip(err)
	}
	if _, err := s.Update("doc", "v3", nil); err == nil {
		t.Error("Update accepted a hard-linked sidecar")
	}
	if got, _ := os.ReadFile(secret); string(got) != "precious" {
		t.Fatalf("outside file changed: %q", got)
	}
}

// Mode-000 entries (file, sidecar, directory): List survives, returns the good
// artifact, and reports each bad one.
func TestZadvListSurvivesUnreadableEntries(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 files")
	}
	anchor, _ := zadvTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"good", "nofile", "nometa", "sub/inner"} {
		if _, err := s.Create(n, "c", Metadata{}); err != nil {
			t.Fatal(err)
		}
	}
	store := filepath.Join(anchor, "store")
	for _, p := range []string{"nofile", "nometa.meta.json"} {
		if err := os.Chmod(filepath.Join(store, p), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(store, "sub"), 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(filepath.Join(store, "sub"), 0o750) }()
	arts, err := s.List()
	var partial *PartialListError
	if !errors.As(err, &partial) {
		t.Fatalf("err = %v, want a *PartialListError", err)
	}
	var names []string
	for _, a := range arts {
		names = append(names, a.Name)
	}
	if len(names) != 1 || names[0] != "good" {
		t.Fatalf("names = %v, want [good]", names)
	}
	skipped := map[string]bool{}
	for _, e := range partial.Skipped {
		skipped[e.Name] = true
	}
	for _, want := range []string{"nofile", "nometa", "sub"} {
		if !skipped[want] {
			t.Errorf("List did not report %q as skipped: %v", want, partial.Skipped)
		}
	}
}

// Hostile names: newline, unicode, NUL, "..", absolute, over-long. Each either
// round-trips through Create and Get, or is refused; nothing lands outside, no
// stray file is left behind, and Create's answer agrees with Get's.
func TestZadvHostileNames(t *testing.T) {
	anchor, outside := zadvTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"new\nline", "ünï-cödé", "tab\tname", "nul\x00byte", "..", "../x", "a/../../x", "/abs/x", "a/./b", "sub/", strings.Repeat("n", 100), strings.Repeat("n", 255), strings.Repeat("n", 256), strings.Repeat("d", 300) + "/f"}
	for _, n := range names {
		art, cerr := s.Create(n, "body", Metadata{})
		if cerr != nil {
			continue
		}
		got, gerr := s.Get(n)
		if gerr != nil || got.Content != "body" {
			t.Errorf("Create(%q) succeeded (%s) but Get = %v, %v", n, art.Name, got, gerr)
		}
	}
	zadvEmpty(t, outside)
	if es, _ := os.ReadDir(anchor); len(es) != 1 {
		t.Errorf("anchor holds more than the store: %v", es)
	}
}

// Name lengths where the content's stage or lock file name fits but the
// sidecar's does not (or the reverse): Create must be all or nothing, and an
// artifact Create reports must be updatable.
func TestZadvNameLengthBoundary(t *testing.T) {
	anchor, _ := zadvTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	for n := 215; n <= 255; n++ {
		name := strings.Repeat("n", n)
		_, cerr := s.Create(name, "body", Metadata{})
		entries, _ := os.ReadDir(filepath.Join(anchor, "store"))
		if cerr != nil {
			for _, e := range entries {
				if e.Name() == name {
					t.Fatalf("len %d: Create failed (%v) but left the content behind", n, cerr)
				}
			}
			continue
		}
		if _, err := s.Get(name); err != nil {
			t.Fatalf("len %d: Create succeeded, Get: %v", n, err)
		}
		if _, err := s.Update(name, "body2", nil); err != nil {
			t.Fatalf("len %d: Create succeeded, Update: %v", n, err)
		}
		if _, err := s.List(); err != nil {
			t.Fatalf("len %d: List: %v", n, err)
		}
	}
}

// Case folding. A successful Create must be readable under the name it
// reported, and a name that differs only in case must not alias another
// artifact's content.
func TestZadvCaseFoldedNamesAreNotAliased(t *testing.T) {
	anchor, _ := zadvTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("Foo", "upper", Metadata{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("foo", "lower", Metadata{}); err == nil {
		if got, gerr := s.Get("foo"); gerr != nil || got.Content != "lower" {
			t.Errorf("Create(foo) succeeded but Get(foo) = %v, %v", got, gerr)
		}
	}
	if got, err := s.Get("Foo"); err != nil || got.Content != "upper" {
		t.Errorf("Get(Foo) = %v, %v: the first artifact was disturbed", got, err)
	}
	if got, err := s.Get("foo"); err == nil && got.Content == "upper" {
		t.Errorf("Get(foo) returned Foo's content under a second identity")
	}
	if _, err := s.Update("foo", "x", nil); err == nil {
		if got, _ := s.Get("Foo"); got != nil && got.Content == "x" {
			t.Errorf("Update(foo) rewrote Foo")
		}
	}
}

// The same through a directory component: Create("Dir/a") then Create("dir/b").
// On a case-insensitive file system the second lands in Dir/. Create reported
// success, so Get must find it.
func TestZadvCaseFoldedDirectoryDefect(t *testing.T) {
	t.Skip("case-fold aliasing: separate card")
	anchor, _ := zadvTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("Dir/a", "a", Metadata{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("dir/b", "b", Metadata{}); err != nil {
		return // refused: the invariant holds
	}
	if got, err := s.Get("dir/b"); err != nil || got.Content != "b" {
		t.Errorf("Create(dir/b) succeeded but Get(dir/b) = %v, %v (the artifact is stored under Dir/b)", got, err)
	}
	arts, _ := s.List()
	var names []string
	for _, a := range arts {
		names = append(names, a.Name)
	}
	t.Logf("List: %v", names)
}

// FIFO, socket, huge file, /dev/zero link at the content, the sidecar and the
// lock file: nothing blocks, nothing is read unbounded.
func TestZadvHostileEntriesAtEveryReadSite(t *testing.T) {
	anchor, _ := zadvTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(anchor, "store")
	mk := func(name string) {
		if _, err := s.Create(name, "body", Metadata{}); err != nil {
			t.Fatal(err)
		}
	}
	replace := func(path string, how string) {
		_ = os.Remove(path)
		switch how {
		case "fifo":
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		case "zero":
			if err := os.Symlink("/dev/zero", path); err != nil {
				t.Skip(err)
			}
		case "huge":
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(1 << 40); err != nil {
				t.Skip(err)
			}
			_ = f.Close()
		case "dir":
			if err := os.Mkdir(path, 0o750); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, how := range []string{"fifo", "zero", "huge", "dir"} {
		for _, site := range []string{"content", "sidecar", "lock"} {
			name := how + "_" + site
			if how == "huge" && site != "sidecar" {
				// A sparse regular file is a valid lock file, and Update replaces
				// a content file without reading it: neither is an attack.
				continue
			}
			mk(name)
			path := filepath.Join(store, name)
			switch site {
			case "sidecar":
				path += metaSuffix
			case "lock":
				path = filepath.Join(store, "."+name+"."+lockNonce+".tmp")
			}
			replace(path, how)
			zadvWithin(t, name, func() {
				if _, err := s.Get(name); err == nil && site != "lock" {
					t.Errorf("%s: Get succeeded", name)
				}
				if _, err := s.Update(name, "next", nil); err == nil && site != "lock" {
					t.Errorf("%s: Update succeeded", name)
				}
				if site == "lock" {
					if _, err := s.Update(name, "next", nil); err == nil {
						t.Errorf("%s: Update took a lock that is a %s", name, how)
					}
				}
				_, _ = s.List()
			})
		}
	}
	short, err := os.MkdirTemp("/tmp", "zadv")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = os.RemoveAll(short) }()
	st2, err := NewStoreUnder(short, "store")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st2.Create("sock", "body", Metadata{}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(short, "store", "sock")
	_ = os.Remove(p)
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = l.Close() }()
	zadvWithin(t, "socket content", func() {
		if _, err := st2.Get("sock"); err == nil {
			t.Error("Get read a socket")
		}
		_, _ = st2.List()
	})
}

// --- processes ---

const zadvEnv = "ZADV_W23_ARTIFACTS_HELPER"

func TestZadvHelperProcess(t *testing.T) {
	if os.Getenv(zadvEnv) == "" {
		t.Skip("helper process only")
	}
	s, err := NewStoreUnder(os.Getenv("ZADV_ANCHOR"), "store")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	n, _ := strconv.Atoi(os.Getenv("ZADV_N"))
	lockWait = 60 * time.Second
	for i := 0; i < n; i++ {
		switch os.Getenv(zadvEnv) {
		case "update":
			if _, err := s.Update("doc", fmt.Sprintf("%d-%d", os.Getpid(), i), nil); err != nil {
				fmt.Fprintln(os.Stderr, "update:", err)
				os.Exit(3)
			}
		case "cas":
			for {
				got, err := s.Get("doc")
				if err != nil {
					fmt.Fprintln(os.Stderr, "get:", err)
					os.Exit(4)
				}
				_, err = s.UpdateIfRevision("doc", fmt.Sprintf("%d-%d", os.Getpid(), i), nil, got.Revision)
				if err == nil {
					break
				}
				if !errors.Is(err, ErrRevisionConflict) {
					fmt.Fprintln(os.Stderr, "cas:", err)
					os.Exit(5)
				}
			}
		}
	}
	os.Exit(0)
}

// Real processes updating one artifact: no lost revision, no torn file.
func TestZadvUpdatesAcrossProcessesLoseNothing(t *testing.T) {
	for _, mode := range []string{"update", "cas"} {
		t.Run(mode, func(t *testing.T) {
			anchor, _ := zadvTree(t)
			s, err := NewStoreUnder(anchor, "store")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Create("doc", "v0", Metadata{}); err != nil {
				t.Fatal(err)
			}
			const procs, each = 6, 15
			var cmds []*exec.Cmd
			var outs []*strings.Builder
			for i := 0; i < procs; i++ {
				c := exec.Command(os.Args[0], "-test.run=^TestZadvHelperProcess$")
				c.Env = append(os.Environ(), zadvEnv+"="+mode, "ZADV_ANCHOR="+anchor, "ZADV_N="+strconv.Itoa(each))
				var sb strings.Builder
				c.Stdout, c.Stderr = &sb, &sb
				if err := c.Start(); err != nil {
					t.Fatal(err)
				}
				cmds, outs = append(cmds, c), append(outs, &sb)
			}
			for i, c := range cmds {
				if err := c.Wait(); err != nil {
					t.Errorf("child %d: %v\n%s", i, err, outs[i])
				}
			}
			got, err := s.Get("doc")
			if err != nil {
				t.Fatal(err)
			}
			if want := 1 + procs*each; got.Revision != want {
				t.Fatalf("revision = %d, want %d: updates were lost", got.Revision, want)
			}
		})
	}
}

// Readers race writers that replace the content and the sidecar by rename. A
// reader must see the old or the new file, never a refusal or an error for a
// file that exists throughout.
func TestZadvGetRacingUpdateNeverFailsDefect(t *testing.T) {
	anchor, _ := zadvTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("doc", "v0", Metadata{}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var writers, readers sync.WaitGroup
	errs := make(chan error, 64)
	for w := 0; w < 3; w++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := 0; i < 150; i++ {
				if _, err := s.Update("doc", fmt.Sprintf("v%d", i), nil); err != nil {
					errs <- fmt.Errorf("Update: %w", err)
					return
				}
			}
		}()
	}
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			reader, _ := NewStoreUnder(anchor, "store")
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := reader.Get("doc"); err != nil {
					errs <- fmt.Errorf("Get: %w", err)
					return
				}
				if _, err := reader.List(); err != nil {
					errs <- fmt.Errorf("List: %w", err)
					return
				}
			}
		}()
	}
	writers.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// An entry lock file that is 31 s old but HELD by a live holder is never taken
// over: its age says nothing about whether it is held.
func TestZadvAnAgedButHeldEntryLockIsNeverTakenOver(t *testing.T) {
	anchor, _ := zadvTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("doc", "v0", Metadata{}); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(anchor, "store", ".doc."+lockNonce+".tmp")
	unlock, err := filelock.Lock(t.Context(), lock)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-31 * time.Second)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	prev := lockWait
	lockWait = 300 * time.Millisecond
	_, uerr := s.Update("doc", "v1", nil)
	lockWait = prev
	if !errors.Is(uerr, ErrLocked) {
		t.Fatalf("Update with a held 31 s old lock = %v, want ErrLocked", uerr)
	}
	unlock()
	if _, err := s.Update("doc", "v1", nil); err != nil {
		t.Fatalf("Update after release: %v", err)
	}
}
