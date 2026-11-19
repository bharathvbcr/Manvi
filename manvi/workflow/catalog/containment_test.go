package catalog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func approved(t *testing.T, s Store, id, revision string) Entry {
	t.Helper()
	e, err := s.Put(catalogFixtureID(t, id, revision))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(id, revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestGetIsNotAffectedByAnotherRevisionOfItsOwnIdentity(t *testing.T) {
	s := Store{Root: t.TempDir()}
	e := approved(t, s, "b", "1")
	if err := os.WriteFile(filepath.Join(s.Root, "b", "2.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("b", "1")
	if err != nil || got.Status != StatusApproved || got.SHA256 != e.SHA256 {
		t.Fatalf("Get(b, 1) = %+v, %v", got, err)
	}
	if _, err := s.Get("b", "2"); err == nil {
		t.Fatal("Get answered for a revision that does not compile")
	}
	if _, err := s.Get("b", "9"); err == nil || !strings.Contains(err.Error(), "unknown catalog revision") {
		t.Fatalf("Get of an absent revision: %v", err)
	}
	if _, err := s.Get("nope", "1"); err == nil || !strings.Contains(err.Error(), "unknown catalog revision") {
		t.Fatalf("Get of an absent identity: %v", err)
	}
}

// A revision is approved only when id, revision and digest all match the
// pointer. A pointer that names this revision under another digest is an error,
// never a draft and never approved.
func TestGetNeverAnswersApprovedForAPointerThatDoesNotMatch(t *testing.T) {
	s := Store{Root: t.TempDir()}
	approved(t, s, "b", "1")
	pointer := filepath.Join(s.Root, "b", "current.json")
	rewriteCurrent(t, pointer, func(m map[string]json.RawMessage) {
		m["sha256"] = json.RawMessage(`"deadbeef"`)
	})
	if got, err := s.Get("b", "1"); err == nil {
		t.Fatalf("Get answered %+v for a pointer with another digest", got)
	}
	if err := os.WriteFile(pointer, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get("b", "1"); err == nil {
		t.Fatalf("Get answered %+v for a pointer that does not parse", got)
	}
}

func TestGetRefusesALinkedIdentityAndTheCatalogRoot(t *testing.T) {
	outside := t.TempDir()
	s := Store{Root: t.TempDir()}
	if err := os.Symlink(outside, filepath.Join(s.Root, "b")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if _, err := s.Get("b", "1"); err == nil || strings.Contains(err.Error(), "unknown catalog revision") {
		t.Fatalf("a linked identity was treated as absent: %v", err)
	}
	link := filepath.Join(t.TempDir(), "catalog")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	linked := Store{Root: link}
	if _, err := linked.Put(catalogFixtureID(t, "b", "1")); err == nil {
		t.Fatal("Put wrote through a catalog root that is a link")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("the link's target was written to: %v", entries)
	}
}

func TestListReturnsTheReadableRowsBesideTheEntriesItSkipped(t *testing.T) {
	s := Store{Root: t.TempDir()}
	approved(t, s, "b", "1")
	if err := os.Mkdir(filepath.Join(s.Root, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, "a", "junk.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(s.Root, "c")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}

	rows, err := s.List()
	var partial *PartialListError
	if !errors.As(err, &partial) {
		t.Fatalf("err = %v, want a *PartialListError", err)
	}
	if len(rows) != 1 || rows[0].ID != "b" || rows[0].Status != StatusApproved {
		t.Fatalf("rows = %+v, want only b/1, approved", rows)
	}
	names := map[string]bool{}
	for _, skipped := range partial.Skipped {
		names[skipped.ID+"/"+skipped.Name] = true
	}
	if !names["a/junk.json"] || !names["c/"] || len(partial.Skipped) != 2 {
		t.Fatalf("skipped = %v, want a/junk.json and c", partial.Skipped)
	}
}

// An identity whose pointer cannot be trusted is skipped whole: listing its
// revisions as drafts would guess at a status that may be approved or revoked.
func TestListSkipsAnIdentityWhosePointerIsBroken(t *testing.T) {
	s := Store{Root: t.TempDir()}
	approved(t, s, "b", "1")
	approved(t, s, "d", "1")
	if err := os.WriteFile(filepath.Join(s.Root, "d", "current.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List()
	var partial *PartialListError
	if !errors.As(err, &partial) || len(rows) != 1 || rows[0].ID != "b" {
		t.Fatalf("rows = %+v, err = %v", rows, err)
	}
}

// A directory-sync failure after the name landed is the shared sentinel, not a
// plain failure, for every publish the catalog makes: the change is on disk.
func TestAnUnconfirmedDirectorySyncIsReportedAsPublishedNotFailed(t *testing.T) {
	s := Store{Root: t.TempDir()}
	e, err := s.Put(catalogFixtureID(t, "b", "1"))
	if err != nil {
		t.Fatal(err)
	}
	prev := syncDir
	syncDir = func(*os.Root) error { return errors.New("disk gone") }
	t.Cleanup(func() { syncDir = prev })

	if err := s.Approve("b", "1", e.SHA256); !errors.Is(err, ErrPublishedUnsynced) {
		t.Fatalf("Approve = %v, want ErrPublishedUnsynced", err)
	}
	got, err := s.Get("b", "1")
	if err != nil || got.Status != StatusApproved {
		t.Fatalf("the approval did not take effect: %+v, %v", got, err)
	}
	if err := s.SetStability("b", "1", e.SHA256, Stability{Runs: 3}); !errors.Is(err, ErrPublishedUnsynced) {
		t.Fatalf("SetStability = %v, want ErrPublishedUnsynced", err)
	}
	if err := s.Revoke("b", "1", e.SHA256); !errors.Is(err, ErrPublishedUnsynced) {
		t.Fatalf("Revoke = %v, want ErrPublishedUnsynced", err)
	}
	put, err := s.Put(catalogFixtureID(t, "b", "2"))
	if !errors.Is(err, ErrPublishedUnsynced) || put.Revision != "2" || put.SHA256 == "" {
		t.Fatalf("Put = %+v, %v, want the entry beside ErrPublishedUnsynced", put, err)
	}
	if got, err := s.Get("b", "2"); err != nil || got.SHA256 != put.SHA256 {
		t.Fatalf("the published revision is not readable: %+v, %v", got, err)
	}
	if got, err := s.Get("b", "1"); err != nil || got.Status != StatusRevoked {
		t.Fatalf("the revocation did not take effect: %+v, %v", got, err)
	}
}
