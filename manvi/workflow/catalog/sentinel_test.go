package catalog

import (
	"errors"
	"os"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/artifacts"
)

// The three stores that publish atomically (artifacts, the catalog, bootstrap)
// report "published, durability unconfirmed" as one value, so a caller that
// handles it for one handles it for all. An Approve that took effect must match
// it rather than read as a plain failure.
func TestApproveUnderASyncFailureMatchesTheArtifactsSentinel(t *testing.T) {
	s := Store{Root: t.TempDir()}
	e, err := s.Put(catalogFixtureID(t, "b", "1"))
	if err != nil {
		t.Fatal(err)
	}
	prev := syncDir
	syncDir = func(*os.Root) error { return errors.New("disk gone") }
	t.Cleanup(func() { syncDir = prev })

	if err := s.Approve("b", "1", e.SHA256); !errors.Is(err, artifacts.ErrPublishedUnsynced) {
		t.Fatalf("Approve = %v, want a match for artifacts.ErrPublishedUnsynced", err)
	}
	got, err := s.Get("b", "1")
	if err != nil || got.Status != StatusApproved {
		t.Fatalf("the approval that was reported unsynced did not take effect: %+v, %v", got, err)
	}
}
