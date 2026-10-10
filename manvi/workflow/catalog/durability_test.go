package catalog

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// A name that is not durable is a revision that can vanish on power loss.
// Every publish, replacing or not, must sync the directory after the name
// lands and report a sync failure instead of swallowing it.
func TestPublishSyncsTheDirectoryAfterTheNameLands(t *testing.T) {
	var synced int
	prev := syncDir
	syncDir = func(root *os.Root) error {
		synced++
		if _, err := root.Stat("name"); err != nil {
			t.Errorf("directory synced before the name was published: %v", err)
		}
		return nil
	}
	t.Cleanup(func() { syncDir = prev })
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := publish(root, "name", []byte("a"), false); err != nil {
		t.Fatal(err)
	}
	if err := publish(root, "name", []byte("b"), true); err != nil {
		t.Fatal(err)
	}
	if synced != 2 {
		t.Fatalf("directory synced %d times for 2 publishes", synced)
	}
}

func TestPublishReportsADirectorySyncFailure(t *testing.T) {
	prev := syncDir
	syncDir = func(*os.Root) error { return errors.New("disk gone") }
	t.Cleanup(func() { syncDir = prev })
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := publish(root, "name", []byte("a"), true); err == nil || !strings.Contains(err.Error(), "disk gone") {
		t.Fatalf("sync failure not reported: %v", err)
	}
}

// The real directory sync works on a directory handle.
func TestSyncDirOnARealRoot(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := syncDir(root); err != nil {
		t.Fatal(err)
	}
}
