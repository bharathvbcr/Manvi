package catalog

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/internal/treefile"
)

// Two writers (two Store values, as two processes) race two spellings of one
// identity. Exactly one may win: the loser's Put must not report a success that a
// Get of its own identity then does not find, nor rewrite the winner's entry.
func TestZadvW24ConcurrentPutsOfFoldedIdentitiesHaveOneWinner(t *testing.T) {
	both, unreadable := 0, 0
	const rounds = 150
	for round := 0; round < rounds; round++ {
		root := filepath.Join(t.TempDir(), "cat")
		a, b := Store{Root: root}, Store{Root: root}
		var wg sync.WaitGroup
		var ea, eb error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, ea = a.Put(catalogFixtureID(t, "Foo", "A")) }()
		go func() { defer wg.Done(); <-start; _, eb = b.Put(catalogFixtureID(t, "foo", "B")) }()
		close(start)
		wg.Wait()
		if ea == nil && eb == nil {
			both++
			_, ga := a.Get("Foo", "A")
			_, gb := a.Get("foo", "B")
			if ga != nil || gb != nil {
				unreadable++
			}
		}
	}
	if both > 0 {
		t.Errorf("%d of %d rounds: Foo/A and foo/B were both put (one directory, two spellings); %d leave a put identity that Get does not find", both, rounds, unreadable)
	}
}

// Same race for revisions inside one identity: the leaf is published by link, so
// the file system itself arbitrates.
func TestZadvW24ConcurrentPutsOfFoldedRevisionsHaveOneWinner(t *testing.T) {
	for round := 0; round < 100; round++ {
		root := filepath.Join(t.TempDir(), "cat")
		a, b := Store{Root: root}, Store{Root: root}
		if _, err := a.Put(catalogFixtureID(t, "Id", "seed")); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var ea, eb error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, ea = a.Put(catalogFixtureID(t, "Id", "Rev")) }()
		go func() { defer wg.Done(); <-start; _, eb = b.Put(catalogFixtureID(t, "Id", "rev")) }()
		close(start)
		wg.Wait()
		if ea == nil && eb == nil {
			// Different programs under one name: the second Put replaced or shared the first.
			ga, _ := a.Get("Id", "Rev")
			gb, _ := a.Get("Id", "rev")
			t.Fatalf("round %d: both revisions were put; Get(Rev) = %+v, Get(rev) = %+v", round, ga, gb)
		}
	}
}

// Reserved pointer name under every spelling, including as an identity.
func TestZadvW24PointerNameSpellingsAreRefusedAsRevisions(t *testing.T) {
	s := Store{Root: filepath.Join(t.TempDir(), "cat")}
	for _, rev := range []string{"current", "CURRENT", "Current", "cUrReNt"} {
		if _, err := s.Put(catalogFixtureID(t, "Id", rev)); err == nil {
			t.Errorf("revision %q accepted", rev)
		}
	}
	// An identity called current is a directory, not the pointer, and stays allowed.
	if _, err := s.Put(catalogFixtureID(t, "current", "r1")); err != nil {
		t.Logf("identity named current: %v", err)
	}
	if _, err := s.Put(catalogFixtureID(t, "CURRENT", "r1")); err != nil && !errors.Is(err, treefile.ErrAliased) {
		t.Logf("CURRENT after current: %v", err)
	}
}
