package artifacts

import (
	"os"
	"path/filepath"
	"testing"
)

// On a case-insensitive filesystem "x.META.JSON" is the sidecar of "x", so the
// reserved namespace is matched without regard to case, in every component.
func TestSidecarNamespaceIsCaseBlindAndCoversEveryComponent(t *testing.T) {
	store, dir := newTestStore(t)
	for _, name := range []string{
		"plan.md.META.JSON", "plan.md.Meta.Json", "d.meta.json/inner", "a/B.META.JSON/c",
		".plan.md.0123456789abcdef.tmp",
	} {
		if _, err := store.Create(name, "squatter", Metadata{}); err == nil {
			t.Errorf("Create(%q) was accepted", name)
		}
	}
	if _, err := store.Create("plan.md", "real", Metadata{Summary: "real"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get("plan.md")
	if err != nil || got.Revision != 1 || got.Metadata.Summary != "real" {
		t.Fatalf("plan.md: %+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d.meta.json")); err == nil {
		t.Fatal("a directory in the sidecar namespace was created")
	}
}

// A crash between creating a temporary sibling and publishing it leaves the
// sibling behind; it is not an artifact.
func TestAFileLeftByACrashedWriteIsNotListed(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("a.md", "x", Metadata{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".a.md.0123456789abcdef.tmp"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	arts, err := store.List()
	if err != nil || len(arts) != 1 || arts[0].Name != "a.md" {
		t.Fatalf("List = %v, %v", arts, err)
	}
}

// A sidecar whose content is gone does not license Update to recreate the
// content: Update modifies, it never creates.
func TestUpdateDoesNotRecreateContentOverAnOrphanSidecar(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("a.md", "x", Metadata{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "a.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("a.md", "y", nil); err == nil {
		t.Fatal("Update recreated an artifact whose content was missing")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.md")); err == nil {
		t.Fatal("content was written")
	}
}
