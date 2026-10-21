package devcouncil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/artifacts"
)

// A partial listing is a qualified success that names what it left out; any
// other listing error is still a failure. Store-free of the task database.
func TestListArtifactsNamesSkippedEntriesAndStillFailsOnRealErrors(t *testing.T) {
	dir := t.TempDir()
	store, err := artifacts.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("good.md", "ok", artifacts.Metadata{Summary: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(dir, "evil.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	arts, listErr := store.List()
	skipped, err := skippedArtifactEntries(listErr)
	if err != nil {
		t.Fatalf("a partial listing was treated as a failure: %v", err)
	}
	if len(arts) != 1 || len(skipped) != 1 || skipped[0]["name"] != "evil.md" || skipped[0]["reason"] == "" {
		t.Fatalf("arts=%+v skipped=%+v", arts, skipped)
	}

	boom := fmt.Errorf("walking the store: %w", errors.New("disk on fire"))
	if got, err := skippedArtifactEntries(boom); err == nil || got != nil {
		t.Fatalf("a real error was swallowed: %v, %v", got, err)
	}
	if got, err := skippedArtifactEntries(nil); err != nil || got != nil {
		t.Fatalf("nil = %v, %v", got, err)
	}
}
