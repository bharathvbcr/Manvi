package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

// Get asks for one identity and one revision. A neighbour that cannot be
// compiled is not part of that question, and used to answer it anyway.
func TestGetAnswersForItsIdentityWhileAnotherHoldsJunk(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if _, err := s.Put(catalogFixtureID(t, "b", "1")); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(s.Root, "a")
	if err := os.Mkdir(junk, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(junk, "junk.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("b", "1")
	if err != nil {
		t.Fatalf("Get(b, 1) failed because of identity a: %v", err)
	}
	if got.ID != "b" || got.Revision != "1" {
		t.Fatalf("Get returned %+v", got)
	}
}
