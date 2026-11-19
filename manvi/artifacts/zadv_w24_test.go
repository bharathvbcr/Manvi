package artifacts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/internal/treefile"
)

// Whatever the file system reads as an existing artifact must be refused by
// Create, or created under exactly the stored spelling: Create(B) must never
// succeed and then leave Get(A) returning B's content.
func TestZadvW24CreateNeverLandsInAnotherArtifactsSlot(t *testing.T) {
	pairs := [][2]string{
		{"k", "\u212a"}, {"s", "\u017f"}, {"i", "\u0130"}, {"i", "\u0131"},
		{"stra\u00dfe", "STRASSE"}, {"\u03c3", "\u03c2"},
		{"caf\u00e9", "cafe\u0301"}, {"cafe\u0301", "caf\u00e9"},
		{"\u1100\u1161", "\uac00"}, {"\uff41", "a"},
		{"Dir/x", "dir/y"}, {"Dir/x", "DIR/x"}, {"d\u00e9/x", "de\u0301/y"},
		{"A", "a"}, {"a/B", "a/b"},
	}
	for _, p := range pairs {
		anchor, _ := aliasTree(t)
		s, err := NewStoreUnder(anchor, "store")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Create(p[0], "first", Metadata{}); err != nil {
			t.Logf("setup %+q: %v", p[0], err)
			continue
		}
		_, err = s.Create(p[1], "second", Metadata{})
		got, gerr := s.Get(p[0])
		if gerr != nil || got.Content != "first" {
			t.Errorf("stored %+q, then Create(%+q) = %v: Get(%+q) = %v, %v", p[0], p[1], err, p[0], got, gerr)
		}
		if err == nil {
			// Created: then it must be a different entry that Get reads back as itself.
			g2, e2 := s.Get(p[1])
			if e2 != nil || g2.Content != "second" {
				t.Errorf("Create(%+q) succeeded but Get(%+q) = %v, %v", p[1], p[1], g2, e2)
			}
		}
	}
}

// Sidecar namespace under folding: every spelling the file system reads as
// ".meta.json" is reserved, in any component.
func TestZadvW24SidecarSpellingsAreReserved(t *testing.T) {
	anchor, _ := aliasTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"x.META.JSON", "x.meta.j\u017fon", "x.Meta.Json", "dir.meta.json/y", "a/b.meta.j\u017fon/c",
		".meta.json", ".META.JSON", "x.meta.json ", " x.meta.json", "x.meta.json/", "x.meta.json/.",
		"x.m\u0435ta.json", // Cyrillic e: not a fold, a different name
	} {
		_, err := s.Create(name, "c", Metadata{})
		if name == "x.m\u0435ta.json" {
			if err != nil {
				t.Logf("Cyrillic lookalike refused: %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("Create(%q) succeeded in the sidecar namespace", name)
		}
	}
}

// Two stores over one directory (as two processes would be) race two spellings:
// exactly one wins, and the winner's content is what both spellings read.
func TestZadvW24ConcurrentCreatesOfFoldedLeafNamesHaveOneWinner(t *testing.T) {
	for round := 0; round < 150; round++ {
		anchor, _ := aliasTree(t)
		s1, _ := NewStoreUnder(anchor, "store")
		s2, _ := NewStoreUnder(anchor, "store")
		var wg sync.WaitGroup
		var e1, e2 error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, e1 = s1.Create("Foo", "one", Metadata{}) }()
		go func() { defer wg.Done(); <-start; _, e2 = s2.Create("foo", "two", Metadata{}) }()
		close(start)
		wg.Wait()
		if (e1 == nil) == (e2 == nil) {
			t.Fatalf("round %d: Foo err=%v, foo err=%v: want exactly one winner", round, e1, e2)
		}
		want := "one"
		if e1 != nil {
			want = "two"
		}
		winner, loser := "Foo", "foo"
		if e1 != nil {
			winner, loser = "foo", "Foo"
		}
		if got, err := s1.Get(winner); err != nil || got.Content != want {
			t.Fatalf("round %d: Get(%q) = %v, %v; want content %q", round, winner, got, err, want)
		}
		if got, err := s1.Get(loser); err == nil && got.Content != want {
			t.Fatalf("round %d: Get(%q) returned %q, not the winner's %q", round, loser, got.Content, want)
		}
	}
}

// The same race through a DIRECTORY component. The check is not atomic with the
// directory's creation (documented), so this one may let both in: the cost is
// two artifacts in one directory under two spellings of it, not lost data.
func TestZadvW24ConcurrentCreatesOfFoldedDirectoryNamesHaveOneWinner(t *testing.T) {
	both, unreadable := 0, 0
	const rounds = 200
	for round := 0; round < rounds; round++ {
		anchor, _ := aliasTree(t)
		s1, _ := NewStoreUnder(anchor, "store")
		s2, _ := NewStoreUnder(anchor, "store")
		var wg sync.WaitGroup
		var e1, e2 error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, e1 = s1.Create("Dir/a", "one", Metadata{}) }()
		go func() { defer wg.Done(); <-start; _, e2 = s2.Create("dir/b", "two", Metadata{}) }()
		close(start)
		wg.Wait()
		if e1 == nil && e2 == nil {
			both++
			_, ga := s1.Get("Dir/a")
			_, gb := s1.Get("dir/b")
			if ga != nil || gb != nil {
				unreadable++
			}
		}
	}
	if both > 0 {
		t.Errorf("%d of %d rounds: two spellings of one directory were both created (Dir/a and dir/b share a directory); %d of those leave a created name that Get does not find", both, rounds, unreadable)
	}
}

// Create refuses a name through a file occupying a directory component, under an
// aliased spelling, without panicking or writing.
func TestZadvW24AFileWhereADirectoryComponentWouldBeIsRefusedCleanly(t *testing.T) {
	anchor, _ := aliasTree(t)
	s, _ := NewStoreUnder(anchor, "store")
	if _, err := s.Create("Leaf", "x", Metadata{}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"leaf/child", "Leaf/child", "LEAF/c/d"} {
		if _, err := s.Create(n, "y", Metadata{}); err == nil {
			t.Errorf("Create(%q) through a file succeeded", n)
		}
	}
}

// One Create reads the directories on its path: bound the cost with 10k siblings.
func TestZadvW24CreateCostInATenThousandEntryStore(t *testing.T) {
	anchor, _ := aliasTree(t)
	s, _ := NewStoreUnder(anchor, "store")
	dir := filepath.Join(anchor, "store")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("a%05d", i)))
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	start := time.Now()
	for i := 0; i < 10; i++ {
		if _, err := s.Create(fmt.Sprintf("new%d", i), "c", Metadata{}); err != nil {
			t.Fatal(err)
		}
	}
	per := time.Since(start) / 10
	t.Logf("Create in a 10k-entry store: %v each", per)
	if per > 250*time.Millisecond {
		t.Errorf("Create costs %v in a 10k-entry store", per)
	}
	if _, err := s.Create("A00007", "c", Metadata{}); !errors.Is(err, treefile.ErrAliased) {
		t.Errorf("an alias in a big store was not found: %v", err)
	}
}

// An existing pair stored before the fix is reachable by its stored spelling:
// artifacts that exist under spellings the check would now refuse are still
// readable, listed and updatable. (On a case-insensitive file system only one
// spelling can exist, which is what is created here.)
func TestZadvW24ExistingEntriesStayReadableAndUpdatable(t *testing.T) {
	anchor, _ := aliasTree(t)
	s, _ := NewStoreUnder(anchor, "store")
	if _, err := s.Create("Mixed/Case", "v1", Metadata{}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get("Mixed/Case"); err != nil || got.Content != "v1" {
		t.Fatalf("Get: %v, %v", got, err)
	}
	if _, err := s.Update("Mixed/Case", "v2", nil); err != nil {
		t.Fatalf("Update by stored spelling: %v", err)
	}
	if _, err := s.Create("Mixed/Case", "v3", Metadata{}); err == nil || errors.Is(err, treefile.ErrAliased) {
		t.Errorf("re-creating the exact stored name must say it exists, not that it aliases: %v", err)
	}
}
