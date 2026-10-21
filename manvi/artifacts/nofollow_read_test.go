package artifacts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const outsideSecret = "SECRET-OUTSIDE-THE-STORE"

func outsideFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(p, []byte(outsideSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func swapForSymlink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func TestGetAndListRefuseContentSwappedForSymlink(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("plan.md", "real", Metadata{Summary: "s"}); err != nil {
		t.Fatal(err)
	}
	swapForSymlink(t, filepath.Join(dir, "plan.md"), outsideFile(t))

	got, err := store.Get("plan.md")
	if err == nil || (got != nil && strings.Contains(got.Content, outsideSecret)) {
		t.Fatalf("Get followed the link: %+v, %v", got, err)
	}
	if !strings.Contains(err.Error(), "plan.md") {
		t.Errorf("error does not name the artifact: %v", err)
	}
	arts, err := store.List()
	if err == nil {
		t.Fatalf("List accepted the link: %+v", arts)
	}
	for _, a := range arts {
		if strings.Contains(a.Content, outsideSecret) {
			t.Fatal("List returned the target's bytes")
		}
	}
}

func TestGetAndListRefuseSidecarSwappedForSymlink(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("plan.md", "real", Metadata{Summary: "s"}); err != nil {
		t.Fatal(err)
	}
	forged := filepath.Join(t.TempDir(), "forged.json")
	if err := os.WriteFile(forged, []byte(`{"revision":99,"metadata":{"summary":"forged"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	swapForSymlink(t, filepath.Join(dir, "plan.md.meta.json"), forged)

	if got, err := store.Get("plan.md"); err == nil {
		t.Fatalf("Get accepted a symlinked sidecar: %+v", got)
	}
	if arts, err := store.List(); err == nil {
		t.Fatalf("List accepted a symlinked sidecar: %+v", arts)
	}
	if _, err := store.Update("plan.md", "y", nil); err == nil {
		t.Fatal("Update continued history from a symlinked sidecar")
	}
}

func TestGetRefusesBadSidecarInsteadOfRevisionZero(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("plan.md", "real", Metadata{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.md.meta.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get("plan.md"); err == nil {
		t.Fatalf("Get returned %+v over a corrupt sidecar", got)
	}
	if arts, err := store.List(); err == nil {
		t.Fatalf("List returned %+v over a corrupt sidecar", arts)
	}
}

func TestGetRefusesIntermediateDirectorySwappedForSymlink(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("lessons/gitpulse/x.md", "real", Metadata{}); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "gitpulse"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"x.md", "x.md.meta.json"} {
		if err := os.WriteFile(filepath.Join(out, "gitpulse", n), []byte(outsideSecret), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(filepath.Join(dir, "lessons")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, filepath.Join(dir, "lessons")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got, err := store.Get("lessons/gitpulse/x.md"); err == nil {
		t.Fatalf("Get walked through a symlinked directory: %+v", got)
	}
	if arts, err := store.List(); err == nil {
		t.Fatalf("List walked through a symlinked directory: %+v", arts)
	}
}

func TestListTreatsMixedCaseSidecarAsMetadataNotContent(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("a.md", "x", Metadata{}); err != nil {
		t.Fatal(err)
	}
	// A different stem, so the file is its own entry on a case-insensitive
	// filesystem too instead of overwriting a.md's sidecar.
	if err := os.WriteFile(filepath.Join(dir, "b.md.META.JSON"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	arts, err := store.List()
	if err != nil || len(arts) != 1 || arts[0].Name != "a.md" {
		t.Fatalf("List = %+v, %v", arts, err)
	}
}
