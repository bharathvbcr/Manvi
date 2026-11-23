//go:build windows

package artifacts

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/safefile"
)

// A rename over a file a reader has open fails on Windows; the store waits for
// the reader instead of reporting the collision as a failed write.
func TestAnUpdateWaitsForAReaderThatHasTheFileOpen(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("a.md", "one", Metadata{}); err != nil {
		t.Fatal(err)
	}
	f, err := safefile.OpenNoFollow(nil, filepath.Join(dir, "a.md"), os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = f.Close()
	}()
	if _, err := store.Update("a.md", "two", nil); err != nil {
		t.Fatalf("an update that collided with a reader failed: %v", err)
	}
	if got, err := store.Get("a.md"); err != nil || got.Content != "two" {
		t.Fatalf("after the update: %+v, %v", got, err)
	}
}
