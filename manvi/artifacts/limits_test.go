package artifacts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A created and updated artifact has exactly one name (Create unlinks its
// temporary, Update renames over), so the hard-link refusal never fires on the
// store's own output.
func TestAnOrdinaryCreatedAndUpdatedArtifactStillReads(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Create("lessons/a.md", "one", Metadata{Summary: "s"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get("lessons/a.md")
	if err != nil || got.Content != "one" || got.Revision != 1 {
		t.Fatalf("after Create: %+v, %v", got, err)
	}
	if _, err := store.Update("lessons/a.md", "two", nil); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get("lessons/a.md")
	if err != nil || got.Content != "two" || got.Revision != 2 || got.Name != "lessons/a.md" {
		t.Fatalf("after Update: %+v, %v", got, err)
	}
	arts, err := store.List()
	if err != nil || len(arts) != 1 || arts[0].Revision != 2 || arts[0].Content != "" {
		t.Fatalf("List = %+v, %v", arts, err)
	}
}

func TestCreateAndUpdateRefuseContentOverTheLimit(t *testing.T) {
	store, dir := newTestStore(t)
	big := strings.Repeat("x", maxArtifactBytes+1)
	if _, err := store.Create("big.md", big, Metadata{}); err == nil {
		t.Fatal("Create accepted content over the limit")
	}
	if _, err := os.Lstat(filepath.Join(dir, "big.md")); err == nil {
		t.Fatal("an oversized Create left content behind")
	}
	if _, err := store.Create("ok.md", strings.Repeat("x", maxArtifactBytes), Metadata{}); err != nil {
		t.Fatalf("content exactly at the limit was refused: %v", err)
	}
	if got, err := store.Get("ok.md"); err != nil || len(got.Content) != maxArtifactBytes {
		t.Fatalf("an at-limit artifact does not read back: %v", err)
	}
	if _, err := store.Update("ok.md", big, nil); err == nil {
		t.Fatal("Update accepted content over the limit")
	}
}

func TestListReportsEverySkippedEntryWithItsReason(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("good.md", "ok", Metadata{Summary: "good"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile(t), filepath.Join(dir, "evil.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.md.meta.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	arts, err := store.List()
	if len(arts) != 1 || arts[0].Name != "good.md" || arts[0].Metadata.Summary != "good" {
		t.Fatalf("readable artifacts = %+v", arts)
	}
	var partial *PartialListError
	if !errors.As(err, &partial) || len(partial.Skipped) != 2 {
		t.Fatalf("err = %v; want a PartialListError with two entries", err)
	}
	if partial.Skipped[0].Name != "bad.md" || partial.Skipped[1].Name != "evil.md" {
		t.Fatalf("skipped = %+v", partial.Skipped)
	}
	for _, e := range partial.Skipped {
		if e.Reason == "" {
			t.Errorf("%s has no reason", e.Name)
		}
	}
}
