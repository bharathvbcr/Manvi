package artifacts

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store, dir
}

// An artifact's metadata lives beside it as "<name>.meta.json". A name that
// ends in that suffix would be the sidecar of another artifact, so the store
// refuses it rather than letting the two namespaces overlap.
func TestANameInTheSidecarNamespaceIsRefused(t *testing.T) {
	store, dir := newTestStore(t)
	art, err := store.Create("plan.md", "# plan", Metadata{Summary: "real"})
	if err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(dir, "plan.md.meta.json")
	before, _ := os.ReadFile(sidecar)

	for _, name := range []string{"plan.md.meta.json", "sub/plan.md.meta.json", "other.meta.json"} {
		if _, err := store.Create(name, "forged", Metadata{Summary: "forged"}); err == nil {
			t.Errorf("Create(%q) was accepted", name)
		}
		if _, err := store.Update(name, "forged", &Metadata{Summary: "forged"}); err == nil {
			t.Errorf("Update(%q) was accepted", name)
		}
		if _, err := store.Get(name); err == nil {
			t.Errorf("Get(%q) was accepted", name)
		}
	}
	if after, _ := os.ReadFile(sidecar); string(after) != string(before) {
		t.Fatalf("plan.md's metadata was rewritten:\n%s", after)
	}
	got, err := store.Get("plan.md")
	if err != nil || got.Revision != art.Revision || got.Metadata.Summary != "real" {
		t.Fatalf("plan.md changed: %+v, %v", got, err)
	}
}

// Creating the artifact first and the sidecar-shaped name second, and the other
// way round, both end the same way.
func TestSidecarCollisionIsRefusedInEitherCreateOrder(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("x.meta.json", "squatter", Metadata{}); err == nil {
		t.Fatal("a sidecar-shaped name was created")
	}
	if _, err := store.Create("x", "content", Metadata{Summary: "s"}); err != nil {
		t.Fatalf("a legitimate artifact was blocked: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "x")); string(data) != "content" {
		t.Fatalf("content = %q", data)
	}
	got, err := store.Get("x")
	if err != nil || got.Revision != 1 || got.Metadata.Summary != "s" {
		t.Fatalf("x = %+v, %v", got, err)
	}
}

func TestListReturnsEveryArtifactTheStoreCreated(t *testing.T) {
	store, _ := newTestStore(t)
	want := []string{"a.md", "b.meta.md", "meta.json", "sub/c.md", "sub/deep/d.txt"}
	for _, name := range want {
		if _, err := store.Create(name, "c", Metadata{}); err != nil {
			t.Fatalf("Create(%q): %v", name, err)
		}
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range list {
		got = append(got, a.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
}

func TestUpdateRefusesToResetHistoryOverBadMetadata(t *testing.T) {
	for name, damage := range map[string]func(sidecar string) error{
		"unreadable JSON": func(p string) error { return os.WriteFile(p, []byte("{not json"), 0o600) },
		"empty object":    func(p string) error { return os.WriteFile(p, []byte("{}"), 0o600) },
		"missing":         os.Remove,
	} {
		t.Run(name, func(t *testing.T) {
			store, dir := newTestStore(t)
			if _, err := store.Create("plan.md", "v1", Metadata{Summary: "s"}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				if _, err := store.Update("plan.md", "vN", nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := damage(filepath.Join(dir, "plan.md.meta.json")); err != nil {
				t.Fatal(err)
			}
			if art, err := store.Update("plan.md", "overwrite", nil); err == nil {
				t.Fatalf("Update succeeded over damaged metadata and returned revision %d", art.Revision)
			}
			if data, _ := os.ReadFile(filepath.Join(dir, "plan.md")); string(data) != "vN" {
				t.Fatalf("content was rewritten despite the error: %q", data)
			}
		})
	}
}

func TestUpdateDoesNotCreateAnArtifact(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Update("ghost.md", "boo", &Metadata{Summary: "x"}); err == nil {
		t.Fatal("Update created an artifact that never existed")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("Update left %d entries behind", len(entries))
	}
}

// A write that fails keeps the revision that was there.
func TestAFailedWriteKeepsThePreviousRevision(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("plan.md", "v1", Metadata{Summary: "s"}); err != nil {
		t.Fatal(err)
	}
	old := syncFile
	syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
	t.Cleanup(func() { syncFile = old })

	if _, err := store.Update("plan.md", "v2", nil); err == nil {
		t.Fatal("a failed fsync was reported as success")
	}
	syncFile = old
	got, err := store.Get("plan.md")
	if err != nil || got.Content != "v1" || got.Revision != 1 {
		t.Fatalf("the previous revision was lost: %+v, %v", got, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "plan.md" && e.Name() != "plan.md.meta.json" {
			t.Errorf("a failed write left %q behind", e.Name())
		}
	}

	syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
	if _, err := store.Create("new.md", "x", Metadata{}); err == nil {
		t.Fatal("a failed fsync on Create was reported as success")
	}
	syncFile = old
	if _, err := os.Stat(filepath.Join(dir, "new.md")); !os.IsNotExist(err) {
		t.Fatalf("a Create that failed left its content behind (stat error: %v)", err)
	}
}

// Two stores over one directory — two processes — racing to create the same
// name: exactly one wins, and what is on disk is the winner's.
func TestCreateIsExclusiveAcrossStores(t *testing.T) {
	dir := t.TempDir()
	for round := 0; round < 40; round++ {
		name := "race" + strings.Repeat("x", round) + ".md"
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make([]error, 4)
		for i := range results {
			store, err := NewStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, results[i] = store.Create(name, strings.Repeat(string(rune('a'+i)), 4096), Metadata{})
			}()
		}
		close(start)
		wg.Wait()
		wins := 0
		for _, err := range results {
			if err == nil {
				wins++
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d creators of %q succeeded, want 1", round, wins, name)
		}
	}
}
