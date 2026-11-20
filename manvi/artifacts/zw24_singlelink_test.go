package artifacts

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/internal/treefile"
)

// Create publishes by link and then removes its stage, so for a moment the new
// artifact has two hard links, and a Get that lands there used to be refused as
// a file that may lie outside the tree. A reader of an artifact that is only ever
// created through the store is never told that.
func TestAGetRacingACreateIsNotRefusedAsAHardLink(t *testing.T) {
	anchor, _ := aliasTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	const n = 150
	var wg sync.WaitGroup
	created := make(chan string, n)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(created)
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("race-%03d.md", i)
			if _, err := s.Create(name, "body", Metadata{}); err != nil {
				t.Errorf("Create(%s): %v", name, err)
				return
			}
			created <- name
		}
	}()
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				name := fmt.Sprintf("race-%03d.md", i%n)
				_, err := s.Get(name)
				if err != nil && !errors.Is(err, ErrNotFound) && (errors.Is(err, treefile.ErrRefused) || strings.Contains(err.Error(), "hard links")) {
					t.Errorf("Get(%s) = %v", name, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	for range created {
	}
}

// And a hard link that is not a publish in progress is still refused: the
// defence against an entry that is a second name for a file outside the tree.
func TestAnArtifactThatIsAHardLinkToAFileOutsideTheStoreIsStillRefused(t *testing.T) {
	anchor, outside := aliasTree(t)
	s, err := NewStoreUnder(anchor, "store")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("kept.md", "body", Metadata{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(anchor+"/store/kept.md", outside+"/second-name"); err != nil {
		t.Skipf("hard links are not available: %v", err)
	}
	if _, err := s.Get("kept.md"); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("a content file with a second name outside the store was read: %v", err)
	}
}

// Update replaces the content and sidecar by rename. A Get racing it must never be
// refused: the reader's name check retries a replace it lands in.
func TestAGetRacingAnUpdateIsNeverRefused(t *testing.T) {
	anchor, _ := aliasTree(t)
	s := mustStoreUnder(t, anchor, "store")
	if _, err := s.Create("doc.md", "v0", Metadata{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 2000; i++ {
			if _, err := s.Update("doc.md", fmt.Sprintf("v%d", i), nil); err != nil {
				t.Errorf("Update %d: %v", i, err)
				return
			}
		}
	}()
	other := mustStoreUnder(t, anchor, "store")
	for i := 0; i < 2000; i++ {
		if _, err := other.Get("doc.md"); err != nil {
			t.Errorf("Get %d was refused while an Update was replacing the files: %v", i, err)
			break
		}
	}
	wg.Wait()
}
