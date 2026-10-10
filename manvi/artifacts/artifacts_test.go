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

// TestNewStoreCreatesNothingUntilAWrite: the store is built for every tool
// surface, including 'manvi tools', which only lists what an agent could call.
// Creating the directory at construction made that listing — and every other
// command that builds the surface without writing an artifact — leave a
// .devcouncil/artifacts/ behind in a checkout nobody asked it to change.
func TestNewStoreCreatesNothingUntilAWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "artifacts")
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(dir)); !os.IsNotExist(err) {
		t.Fatalf("NewStore created %s before anything was written (stat error: %v)", filepath.Dir(dir), err)
	}

	// Reading an empty store is an answer, not an error, and still writes nothing.
	arts, err := store.List()
	if err != nil || len(arts) != 0 {
		t.Fatalf("List on a store never written = %v, %v; want empty and no error", arts, err)
	}
	if _, err := store.Get("plan.md"); err == nil {
		t.Fatal("Get found an artifact in a store never written")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("reading the store created %s (stat error: %v)", dir, err)
	}

	// The first write makes the directory.
	if _, err := store.Create("plan.md", "# Plan", Metadata{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Create("notes/today.md", "x", Metadata{}); err != nil {
		t.Fatalf("Create into a new subdirectory: %v", err)
	}
	if arts, err := store.List(); err != nil || len(arts) != 2 {
		t.Fatalf("List after two writes = %d artifacts, %v; want 2", len(arts), err)
	}
}

// TestNewStoreRefusesAFile: deferring the directory must not defer the one
// misconfiguration construction could always see.
func TestNewStoreRefusesAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifacts")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path); err == nil {
		t.Fatal("NewStore accepted a regular file as its directory")
	}
}
