//go:build unix

package catalog

// Adversarial tests for the w23 containment change (lane w23-containment-adv).
// A test whose name ends in Defect documents a real defect and is expected to
// FAIL on the w23-containment tip.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

func zadvPut(t *testing.T, s Store, id, rev string) Entry {
	t.Helper()
	e, err := s.Put(catalogFixtureID(t, id, rev))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// zadvVariant is the same identity and revision with a different body.
func zadvVariant(t *testing.T, id, rev string) *workflow.Program {
	t.Helper()
	var c workflow.Capability
	if err := json.Unmarshal(catalogFixtureID(t, id, rev).Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	c.Application = "variant"
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p, err := workflow.Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
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

// Identity B is junk in every way an entry can be (link, FIFO, huge pointer,
// pointer that is a FIFO, a mode-000 directory): Get for A answers, List returns
// A's rows and names every bad identity.
func TestZadvGetAndListBesideJunkIdentities(t *testing.T) {
	s := Store{Root: t.TempDir()}
	a := zadvPut(t, s, "a", "1")
	if err := s.Approve("a", "1", a.SHA256); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.Root, "linked")); err != nil {
		t.Skip(err)
	}
	for _, n := range []string{"fifodir", "hugeptr", "fifoptr", "locked", "zerolink"} {
		if err := os.Mkdir(filepath.Join(s.Root, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(s.Root, "fifodir", "1.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(s.Root, "fifoptr", "current.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(s.Root, "hugeptr", "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 40); err != nil {
		t.Skip(err)
	}
	_ = f.Close()
	if err := os.Symlink("/dev/zero", filepath.Join(s.Root, "zerolink", "1.json")); err != nil {
		t.Skip(err)
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(filepath.Join(s.Root, "locked"), 0); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(filepath.Join(s.Root, "locked"), 0o700) }()
	}
	zadvWithin(t, "Get and List", func() {
		got, err := s.Get("a", "1")
		if err != nil || got.Status != StatusApproved {
			t.Errorf("Get(a, 1) = %+v, %v", got, err)
		}
		rows, err := s.List()
		var partial *PartialListError
		if !errors.As(err, &partial) {
			t.Errorf("List err = %v, want *PartialListError", err)
			return
		}
		if len(rows) != 1 || rows[0].ID != "a" {
			t.Errorf("rows = %+v", rows)
		}
		skipped := map[string]bool{}
		for _, e := range partial.Skipped {
			skipped[e.ID] = true
		}
		for _, want := range []string{"linked", "fifoptr", "hugeptr", "zerolink"} {
			if !skipped[want] {
				t.Errorf("List did not name %q: %v", want, err)
			}
		}
	})
	if es, _ := os.ReadDir(outside); len(es) != 0 {
		t.Fatalf("outside was touched: %v", es)
	}
}

// Hostile files inside the identity being read: a FIFO or huge file as the
// revision or the pointer. Get, Approve and Revoke return an error and do not
// block.
func TestZadvHostileFilesInsideTheIdentityBeingUsed(t *testing.T) {
	for _, site := range []string{"revision", "pointer"} {
		for _, how := range []string{"fifo", "huge", "zero"} {
			t.Run(site+"_"+how, func(t *testing.T) {
				s := Store{Root: t.TempDir()}
				e := zadvPut(t, s, "a", "1")
				if err := s.Approve("a", "1", e.SHA256); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(s.Root, "a", "1.json")
				if site == "pointer" {
					path = filepath.Join(s.Root, "a", "current.json")
				}
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
				default:
					f, err := os.Create(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.Truncate(1 << 40); err != nil {
						t.Skip(err)
					}
					_ = f.Close()
				}
				zadvWithin(t, "operations", func() {
					if _, err := s.Get("a", "1"); err == nil {
						t.Error("Get succeeded")
					}
					if err := s.Approve("a", "1", e.SHA256); err == nil && site == "revision" {
						t.Error("Approve succeeded")
					}
					if err := s.Revoke("a", "1", e.SHA256); err == nil {
						t.Error("Revoke succeeded")
					}
					_, _ = s.List()
				})
			})
		}
	}
}

// Every bad revision of an identity is reported, and no good one is dropped.
func TestZadvListReportsEveryBadRevisionAndKeepsTheGoodOnes(t *testing.T) {
	s := Store{Root: t.TempDir()}
	zadvPut(t, s, "a", "1")
	zadvPut(t, s, "a", "2")
	dir := filepath.Join(s.Root, "a")
	for _, n := range []string{"bad1.json", "bad2.json", "bad3.json"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "adir.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List()
	var partial *PartialListError
	if !errors.As(err, &partial) {
		t.Fatalf("err = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want both good revisions", rows)
	}
	if len(partial.Skipped) != 5 {
		t.Fatalf("skipped = %v, want 5 entries", partial.Skipped)
	}
}

// A pointer that names revision 1 under the wrong digest: Get(a, 2), a good
// draft, still answers; List must then not lose it silently. It is reported as
// the identity being skipped, which is the stricter of the two answers, so the
// two disagree: Get serves a row List refuses.
func TestZadvListAndGetAgreeWhenThePointerIsWrongDefect(t *testing.T) {
	s := Store{Root: t.TempDir()}
	e := zadvPut(t, s, "a", "1")
	zadvPut(t, s, "a", "2")
	if err := s.Approve("a", "1", e.SHA256); err != nil {
		t.Fatal(err)
	}
	rewriteCurrent(t, filepath.Join(s.Root, "a", "current.json"), func(m map[string]json.RawMessage) {
		m["sha256"] = json.RawMessage(`"deadbeef"`)
	})
	got, gerr := s.Get("a", "2")
	rows, _ := s.List()
	listed := false
	for _, r := range rows {
		if r.ID == "a" && r.Revision == "2" {
			listed = true
		}
	}
	if gerr == nil && !listed {
		t.Errorf("Get(a, 2) = %+v answers, but List omits it", got)
	}
}

// The approved revision file is swapped for another valid program after the
// approval was recorded: it must never read as approved.
func TestZadvSwappedRevisionIsNeverApproved(t *testing.T) {
	s := Store{Root: t.TempDir()}
	e := zadvPut(t, s, "a", "1")
	if err := s.Approve("a", "1", e.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, "a", "1.json"), zadvVariant(t, "a", "1").Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get("a", "1"); err == nil {
		t.Fatalf("Get answered %+v for a swapped revision", got)
	}
	rows, _ := s.List()
	for _, r := range rows {
		if r.Status == StatusApproved {
			t.Fatalf("List reports a swapped revision as approved: %+v", r)
		}
	}
	if err := s.Approve("a", "1", e.SHA256); err == nil {
		t.Fatal("Approve accepted the old digest for a swapped file")
	}
}

// Approve, Promote and Revoke race Get from many goroutines, each with its own
// Store value: Get never sees a torn pointer, and no writer is refused.
// Intermittent at the tip: one failure ("refused ... dangling symbolic link")
// in about ten runs; see the treefile race test for the deterministic form.
func TestZadvConcurrentApprovalsAgainstGet(t *testing.T) {
	root := t.TempDir()
	e := zadvPut(t, Store{Root: root}, "a", "1")
	var wg sync.WaitGroup
	errs := make(chan error, 256)
	stop := make(chan struct{})
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s := Store{Root: root}
			for i := 0; i < 40; i++ {
				var err error
				switch (w + i) % 3 {
				case 0:
					err = s.Approve("a", "1", e.SHA256)
				case 1:
					err = s.Promote("a", "1", e.SHA256)
				default:
					err = s.Revoke("a", "1", e.SHA256)
					if err != nil && strings.Contains(err.Error(), "cannot revoke without a catalog pointer") {
						err = nil
					}
				}
				if err != nil {
					errs <- fmt.Errorf("writer %d step %d: %w", w, i, err)
					return
				}
			}
		}(w)
	}
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			s := Store{Root: root}
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := s.Get("a", "1")
				if err != nil {
					errs <- fmt.Errorf("Get: %w", err)
					return
				}
				if got.Status != StatusApproved && got.Status != StatusRevoked && got.Status != StatusDraft {
					errs <- fmt.Errorf("Get status %q", got.Status)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A revocation that returned must stay in force: after Revoke succeeds with no
// later Approve, Get says revoked, for every interleaving of stability and
// approval writers that finished before it.
func TestZadvRevocationIsNeverOverwrittenByAnEarlierWriter(t *testing.T) {
	root := t.TempDir()
	e := zadvPut(t, Store{Root: root}, "a", "1")
	for round := 0; round < 25; round++ {
		var wg sync.WaitGroup
		for w := 0; w < 6; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = Store{Root: root}.Approve("a", "1", e.SHA256)
			}()
		}
		wg.Wait()
		if err := (Store{Root: root}).Revoke("a", "1", e.SHA256); err != nil {
			t.Fatal(err)
		}
		got, err := (Store{Root: root}).Get("a", "1")
		if err != nil || got.Status != StatusRevoked {
			t.Fatalf("round %d: after Revoke, Get = %+v, %v", round, got, err)
		}
	}
}

// ErrPublishedUnsynced is returned only once the change is visible, and a
// caller that retries on it does not double-apply.
func TestZadvPublishedUnsyncedIsVisibleAndRetrySafe(t *testing.T) {
	s := Store{Root: t.TempDir()}
	old := syncDir
	defer func() { syncDir = old }()
	syncDir = func(*os.Root) error { return errors.New("injected sync failure") }

	p := catalogFixtureID(t, "a", "1")
	e, err := s.Put(p)
	if !errors.Is(err, ErrPublishedUnsynced) || e.SHA256 != p.Digest() {
		t.Fatalf("Put = %+v, %v", e, err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "a", "1.json")); err != nil {
		t.Fatalf("Put reported unsynced but the revision is not on disk: %v", err)
	}
	if _, err := s.Put(p); err != nil && !errors.Is(err, ErrPublishedUnsynced) {
		t.Fatalf("a retried Put failed: %v", err)
	}
	for i := 0; i < 2; i++ {
		err = s.Approve("a", "1", p.Digest())
		if !errors.Is(err, ErrPublishedUnsynced) {
			t.Fatalf("Approve #%d = %v", i, err)
		}
		if got, gerr := s.Get("a", "1"); gerr != nil || got.Status != StatusApproved {
			t.Fatalf("after unsynced Approve: Get = %+v, %v", got, gerr)
		}
	}
	err = s.Revoke("a", "1", p.Digest())
	if !errors.Is(err, ErrPublishedUnsynced) {
		t.Fatalf("Revoke = %v", err)
	}
	if got, gerr := s.Get("a", "1"); gerr != nil || got.Status != StatusRevoked {
		t.Fatalf("after unsynced Revoke: Get = %+v, %v", got, gerr)
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root, "a"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("stage left behind: %s", e.Name())
		}
	}
}

// The catalog root itself: a link at its last component, a file, a FIFO.
func TestZadvCatalogRootHostile(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip(err)
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(base, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{link, file, fifo} {
		s := Store{Root: root}
		zadvWithin(t, root, func() {
			if _, err := s.Put(catalogFixtureID(t, "a", "1")); err == nil {
				t.Errorf("%s: Put succeeded", root)
			}
			if _, err := s.Get("a", "1"); err == nil || strings.Contains(err.Error(), "unknown catalog revision") {
				t.Errorf("%s: Get = %v, want a refusal rather than absence", root, err)
			}
			if rows, err := s.List(); err == nil && rows != nil && len(rows) == 0 && root != "" {
				t.Errorf("%s: List answered an empty catalog", root)
			}
		})
	}
	zadvEmptyDir(t, outside)
}

func zadvEmptyDir(t *testing.T, dir string) {
	t.Helper()
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Fatalf("%s: %v", dir, es)
	}
}

// A hard link from the pointer or a revision to an outside file: no write ever
// goes through it.
func TestZadvHardLinksNeverWrittenThrough(t *testing.T) {
	s := Store{Root: t.TempDir()}
	e := zadvPut(t, s, "a", "1")
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("{\"keep\":true}"), 0o600); err != nil {
		t.Fatal(err)
	}
	pointer := filepath.Join(s.Root, "a", "current.json")
	if err := os.Link(outside, pointer); err != nil {
		t.Skip(err)
	}
	_ = s.Approve("a", "1", e.SHA256)
	_ = s.Revoke("a", "1", e.SHA256)
	if got, _ := os.ReadFile(outside); string(got) != "{\"keep\":true}" {
		t.Fatalf("a write went through the hard-linked pointer: %q", got)
	}
}

// Case folding (APFS default): "Foo" and "foo" are one directory, so
// operations on one identity must not change what the other reads as. foo uses
// another revision number so that it is not refused as a collision with Foo/1.
func TestZadvCaseFoldedIdentitiesDoNotAliasDefect(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "CaseProbe"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "caseprobe")); err != nil {
		t.Skip("case-sensitive file system")
	}
	s := Store{Root: filepath.Join(root, "cat")}
	upper := zadvPut(t, s, "Foo", "1")
	if err := s.Approve("Foo", "1", upper.SHA256); err != nil {
		t.Fatal(err)
	}
	lower, perr := s.Put(catalogFixtureID(t, "foo", "2"))
	aerr := error(nil)
	if perr == nil {
		aerr = s.Approve("foo", "2", lower.SHA256)
	}
	t.Logf("Put(foo, 2) = %v; Approve(foo, 2) = %v", perr, aerr)
	pointer, _ := os.ReadFile(filepath.Join(s.Root, "Foo", "current.json"))
	got, err := s.Get("Foo", "1")
	if err != nil || got.Status != StatusApproved || got.SHA256 != upper.SHA256 {
		t.Errorf("after operations on foo, Get(Foo, 1) = %+v, %v: another identity changed it; Foo/current.json is now %s", got, err, pointer)
	}
	if rows, lerr := s.List(); lerr != nil {
		t.Errorf("List after a case-folded identity: %v (rows %d)", lerr, len(rows))
	}
}
