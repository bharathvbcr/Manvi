package artifacts

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/internal/treefile"
)

// aliasTree is a store anchor and a directory beside it, made without anything
// the unix-only helpers of the other test files provide.
func aliasTree(t *testing.T) (anchor, outside string) {
	t.Helper()
	base := t.TempDir()
	anchor, outside = filepath.Join(base, "anchor"), filepath.Join(base, "outside")
	for _, d := range []string{anchor, outside} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return anchor, outside
}

// A name that folds equal to an artifact or directory already in the store, under
// a different spelling, is refused with a named error; nothing is renamed, and
// the existing artifact is still read by the spelling it was stored under.
func TestCreateRefusesNamesThatAliasAnExistingEntry(t *testing.T) {
	anchor, _ := aliasTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("Dir/a", "first", Metadata{}); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"dir/b", "DIR/b", "Dir/A", "dir/a", "dIr/a"} {
		_, err := s.Create(alias, "second", Metadata{})
		if !errors.Is(err, treefile.ErrAliased) {
			t.Errorf("Create(%q) = %v, want ErrAliased", alias, err)
		}
	}
	got, err := s.Get("Dir/a")
	if err != nil || got.Content != "first" {
		t.Fatalf("the existing artifact is no longer readable by its own spelling: %v, %v", got, err)
	}
	entries, _ := os.ReadDir(filepath.Join(anchor, "store"))
	if len(entries) != 1 || entries[0].Name() != "Dir" {
		t.Fatalf("an entry was renamed or created beside Dir: %v", entries)
	}
	// A new name inside the stored spelling is fine.
	if _, err := s.Create("Dir/c", "third", Metadata{}); err != nil {
		t.Fatalf("a name nothing resembles was refused: %v", err)
	}
}

// Simple case folding, not lower-casing: the long s is an s, and the Kelvin sign
// is a k, to the file system and now to the store.
func TestCreateRefusesUnicodeCaseFoldedSpellings(t *testing.T) {
	anchor, _ := aliasTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]string{{"status.md", "ſtatus.md"}, {"kilo.md", "Kilo.md"}} {
		if _, err := s.Create(p[0], "x", Metadata{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Create(p[1], "y", Metadata{}); !errors.Is(err, treefile.ErrAliased) {
			t.Errorf("Create(%q) beside %q = %v, want ErrAliased", p[1], p[0], err)
		}
	}
	// The dotless i does not fold to i: both are creatable, as on APFS.
	if _, err := s.Create("istanbul.md", "x", Metadata{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("ıstanbul.md", "y", Metadata{}); err != nil {
		t.Errorf("the dotless i was folded with i: %v", err)
	}
}

// NFC and NFD spell one name on APFS. The store has no decomposition tables, so
// where the file system treats them as one entry the second is refused, and where
// it keeps them apart both are stored and both read back.
func TestCreateAndNormalisationForms(t *testing.T) {
	const nfc, nfd = "café.md", "café.md"
	anchor, _ := aliasTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(nfc, "composed", Metadata{}); err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Stat(filepath.Join(anchor, "store", nfd))
	sameName := statErr == nil
	_, err = s.Create(nfd, "decomposed", Metadata{})
	if sameName {
		if !errors.Is(err, treefile.ErrAliased) {
			t.Fatalf("the file system holds %q and %q as one entry and Create(%q) = %v", nfc, nfd, nfd, err)
		}
	} else if err != nil {
		t.Fatalf("the file system keeps %q and %q apart and Create(%q) = %v", nfc, nfd, nfd, err)
	}
	if got, err := s.Get(nfc); err != nil || got.Content != "composed" {
		t.Fatalf("the stored spelling is no longer readable: %v, %v", got, err)
	}
	t.Logf("NFC and NFD are one name on this file system: %v", sameName)
}

// The metadata namespace is decided under the same folding as the names: on APFS
// "x.meta.jſon" is the sidecar of x.
func TestTheSidecarNamespaceIsFoldedLikeTheNames(t *testing.T) {
	for _, name := range []string{"x.meta.json", "x.META.JSON", "x.meta.jſon", "x.meta.JſON"} {
		if !isSidecarName(name) {
			t.Errorf("%q is not in the sidecar namespace", name)
		}
	}
	for _, name := range []string{"x.json", "x.meta", "meta.jsonx"} {
		if isSidecarName(name) {
			t.Errorf("%q is in the sidecar namespace", name)
		}
	}
}
