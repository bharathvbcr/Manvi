package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/internal/treefile"
)

// Identities and revisions that fold equal to an existing, differently spelled
// one are refused at Put with a named error, on any file system, and what is
// stored keeps its spelling and stays readable.
func TestPutRefusesAnIdentityOrRevisionThatAliasesAnExistingOne(t *testing.T) {
	s := Store{Root: filepath.Join(t.TempDir(), "cat")}
	upper, err := s.Put(catalogFixtureID(t, "Foo", "A"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve("Foo", "A", upper.SHA256); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string][2]string{
		"identity":          {"foo", "2"},
		"identity upper":    {"FOO", "A"},
		"revision":          {"Foo", "a"},
		"identity and rev.": {"fOO", "a"},
	} {
		if _, err := s.Put(catalogFixtureID(t, p[0], p[1])); !errors.Is(err, treefile.ErrAliased) {
			t.Errorf("%s: Put(%s, %s) = %v, want ErrAliased", name, p[0], p[1], err)
		}
	}
	got, err := s.Get("Foo", "A")
	if err != nil || got.Status != StatusApproved || got.SHA256 != upper.SHA256 {
		t.Fatalf("the existing entry changed: %+v, %v", got, err)
	}
	if rows, err := s.List(); err != nil || len(rows) != 1 {
		t.Fatalf("List = %v, %v", rows, err)
	}
	entries, _ := os.ReadDir(s.Root)
	if len(entries) != 1 || entries[0].Name() != "Foo" {
		t.Fatalf("an identity was renamed or created beside Foo: %v", entries)
	}
	// Putting the same revision again under its own spelling is still the
	// idempotent write it was.
	if _, err := s.Put(catalogFixtureID(t, "Foo", "A")); err != nil {
		t.Fatalf("the stored spelling was refused: %v", err)
	}
	// And a revision nothing resembles is added.
	if _, err := s.Put(catalogFixtureID(t, "Foo", "B")); err != nil {
		t.Fatalf("a new revision was refused: %v", err)
	}
}

// The pointer is current.json. On APFS a revision called CURRENT is the same file
// name, and promoting the identity would then rename the pointer over the revision.
func TestPutRefusesARevisionThatFoldsToThePointerFile(t *testing.T) {
	s := Store{Root: filepath.Join(t.TempDir(), "cat")}
	for _, rev := range []string{"current", "CURRENT", "Current", "cuRRent"} {
		if _, err := s.Put(catalogFixtureID(t, "Foo", rev)); err == nil {
			t.Errorf("Put(Foo, %q) succeeded", rev)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(s.Root, "Foo")); len(entries) != 0 {
		t.Errorf("a file was written for a reserved revision: %v", entries)
	}
}
