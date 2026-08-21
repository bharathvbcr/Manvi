package artifacts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactStoreCRUD(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(tmpDir)
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}

	meta := Metadata{
		Summary:         "Initial implementation plan for feature X",
		UserFacing:      true,
		RequestFeedback: true,
	}

	// 1. Create
	art, err := store.Create("implementation_plan.md", "# Plan\nStep 1: Do X", meta)
	if err != nil {
		t.Fatalf("creating artifact: %v", err)
	}
	if art.Revision != 1 || art.Metadata.Summary != meta.Summary {
		t.Errorf("unexpected artifact after create: %+v", art)
	}

	// 2. Duplicate Create should fail
	if _, err := store.Create("implementation_plan.md", "duplicate", meta); err == nil {
		t.Errorf("expected duplicate create to fail")
	}

	// 3. Update
	updatedMeta := Metadata{
		Summary:         "Revised plan with user feedback incorporated",
		UserFacing:      true,
		RequestFeedback: false,
	}
	upd, err := store.Update("implementation_plan.md", "# Plan\nStep 1: Do X\nStep 2: Do Y", &updatedMeta)
	if err != nil {
		t.Fatalf("updating artifact: %v", err)
	}
	if upd.Revision != 2 || upd.Metadata.Summary != updatedMeta.Summary {
		t.Errorf("unexpected artifact after update: %+v", upd)
	}

	// 4. Get
	got, err := store.Get("implementation_plan.md")
	if err != nil {
		t.Fatalf("getting artifact: %v", err)
	}
	if got.Content != "# Plan\nStep 1: Do X\nStep 2: Do Y" {
		t.Errorf("unexpected content: %q", got.Content)
	}

	// 5. List
	_, _ = store.Create("walkthrough.md", "# Walkthrough", Metadata{Summary: "Walkthrough doc"})
	list, err := store.List()
	if err != nil {
		t.Fatalf("listing artifacts: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("expected 2 artifacts, got %d", len(list))
	}
}

func TestArtifactStoreTraversalRejection(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	meta := Metadata{Summary: "test"}
	if _, err := store.Create("../escape.md", "content", meta); err == nil {
		t.Errorf("expected relative traversal to be rejected")
	}

	absPath := filepath.Join(os.TempDir(), "abs.md")
	if _, err := store.Create(absPath, "content", meta); err == nil {
		t.Errorf("expected absolute path to be rejected")
	}
}
