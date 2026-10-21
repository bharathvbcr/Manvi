package artifacts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestUpdateIfRevisionRefusesAStaleRevision(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Create("a.md", "one", Metadata{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateIfRevision("a.md", "two", nil, 1); err != nil {
		t.Fatal(err)
	}
	_, err := store.UpdateIfRevision("a.md", "stale", nil, 1)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("a write over a revision the caller did not read: %v", err)
	}
	got, _ := store.Get("a.md")
	if got.Content != "two" || got.Revision != 2 {
		t.Fatalf("the refused write changed the artifact: %+v", got)
	}
	if _, err := store.UpdateIfRevision("a.md", "x", nil, 0); err == nil {
		t.Error("expected revision 0 was accepted")
	}
}

// Separate Stores over one directory share no mutex, which is what two
// processes look like to the store: only the lock file and the revision compare
// stand between them.
func TestCompareAndSwapAcrossStoresLosesNoUpdate(t *testing.T) {
	seed, dir := newTestStore(t)
	if _, err := seed.Create("a.md", "0", Metadata{}); err != nil {
		t.Fatal(err)
	}
	const writers, each = 6, 5
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		s, err := NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; {
				cur, err := s.Get("a.md")
				if err != nil {
					t.Errorf("get: %v", err)
					return
				}
				_, err = s.UpdateIfRevision("a.md", fmt.Sprintf("w%d-%d", w, i), nil, cur.Revision)
				switch {
				case err == nil:
					i++
				case errors.Is(err, ErrRevisionConflict):
				default:
					t.Errorf("update: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	got, err := seed.Get("a.md")
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 1+writers*each {
		t.Fatalf("revision %d, want %d: updates were lost or doubled", got.Revision, 1+writers*each)
	}
}

func TestAStaleLockFromADeadWriterIsTakenOverAndALiveOneIsNotListed(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("a.md", "x", Metadata{}); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "."+"a.md"+"."+lockNonce+".tmp")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStaleAfter)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("a.md", "y", nil); err != nil {
		t.Fatalf("a crashed writer's lock blocked every later update: %v", err)
	}
	if _, err := os.Lstat(lock); err == nil {
		t.Error("the lock was left behind")
	}
	// Held fresh, it is neither an artifact nor a name a caller can create.
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	arts, err := store.List()
	if err != nil || len(arts) != 1 {
		t.Fatalf("a lock file was listed: %v %v", arts, err)
	}
	if _, err := store.Create("."+"a.md"+"."+lockNonce+".tmp", "x", Metadata{}); err == nil {
		t.Error("the lock file's name is creatable as an artifact")
	}
}
