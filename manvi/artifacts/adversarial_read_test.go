package artifacts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Adversarial review of the no-follow read (ft-89c224376bf199fab8d5f87ee7eb8045).

// Attack 1: a writer flips the content path between a regular file and a link
// to outside while Get and List loop. Outside bytes must never be returned.
func TestAdversarialTOCTOUContentFlip(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("plan.md", "real", Metadata{Summary: "s"}); err != nil {
		t.Fatal(err)
	}
	outside := outsideFile(t)
	content := filepath.Join(dir, "plan.md")
	sidecar := filepath.Join(dir, "plan.md.meta.json")
	realMeta, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		n := 0
		for !stop.Load() {
			n++
			lnk := filepath.Join(dir, fmt.Sprintf(".swap-link-%d", n%2))
			reg := filepath.Join(dir, fmt.Sprintf(".swap-reg-%d", n%2))
			_ = os.Remove(lnk)
			_ = os.Remove(reg)
			if os.Symlink(outside, lnk) == nil {
				_ = os.Rename(lnk, content)
			}
			if os.WriteFile(reg, []byte("real"), 0o644) == nil {
				_ = os.Rename(reg, content)
			}
			// sidecar flip too
			if os.Symlink(outside, lnk) == nil {
				_ = os.Rename(lnk, sidecar)
			}
			if os.WriteFile(reg, realMeta, 0o644) == nil {
				_ = os.Rename(reg, sidecar)
			}
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a, err := store.Get("plan.md"); err == nil && strings.Contains(a.Content, outsideSecret) {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("Get returned outside bytes")
		}
		if arts, err := store.List(); err == nil {
			for _, a := range arts {
				if strings.Contains(a.Content, outsideSecret) {
					stop.Store(true)
					wg.Wait()
					t.Fatalf("List returned outside bytes")
				}
			}
		}
	}
	stop.Store(true)
	wg.Wait()
}

// Attack 1b: the intermediate directory flips between real and a link.
func TestAdversarialTOCTOUIntermediateDirFlip(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("lessons/x.md", "real", Metadata{}); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	for _, n := range []string{"x.md", "x.md.meta.json"} {
		if err := os.WriteFile(filepath.Join(out, n), []byte(outsideSecret), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	live := filepath.Join(dir, "lessons")
	hold := filepath.Join(dir, "lessons-held")
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			if os.Rename(live, hold) != nil {
				continue
			}
			_ = os.Symlink(out, live)
			time.Sleep(200 * time.Microsecond)
			_ = os.Remove(live)
			_ = os.Rename(hold, live)
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	bad := ""
	for time.Now().Before(deadline) && bad == "" {
		if a, err := store.Get("lessons/x.md"); err == nil && strings.Contains(a.Content, outsideSecret) {
			bad = "Get"
		}
		if arts, err := store.List(); err == nil {
			for _, a := range arts {
				if strings.Contains(a.Content, outsideSecret) {
					bad = "List"
				}
			}
		}
	}
	stop.Store(true)
	wg.Wait()
	if bad != "" {
		t.Fatalf("%s returned outside bytes through a flipped directory", bad)
	}
}

// Attack 2: a hard link to a file outside the store, swapped in after publish.
// O_NOFOLLOW does not see it; the DevCouncil untracked-diff reader refuses
// nlink != 1 (devcouncil/diff.go via safefile.LinkCount).
func TestAdversarialHardLinkedContentIsRefused(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("plan.md", "real", Metadata{}); err != nil {
		t.Fatal(err)
	}
	outside := outsideFile(t)
	content := filepath.Join(dir, "plan.md")
	if err := os.Remove(content); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, content); err != nil {
		t.Skipf("hard links unavailable across these directories: %v", err)
	}
	if got, err := store.Get("plan.md"); err == nil {
		t.Fatalf("Get returned a hard-linked outside file: %q", got.Content)
	}
	if arts, err := store.List(); err == nil {
		t.Fatalf("List returned a hard-linked outside file: %+v", arts)
	}
}

func TestAdversarialHardLinkedSidecarIsRefused(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("plan.md", "real", Metadata{}); err != nil {
		t.Fatal(err)
	}
	forged := filepath.Join(t.TempDir(), "forged.json")
	if err := os.WriteFile(forged, []byte(`{"revision":99,"metadata":{"summary":"forged"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(dir, "plan.md.meta.json")
	_ = os.Remove(meta)
	if err := os.Link(forged, meta); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if got, err := store.Get("plan.md"); err == nil {
		t.Fatalf("Get trusted a hard-linked sidecar: revision %d", got.Revision)
	}
}

// Attack 3: one unreadable entry must not brick List for every other artifact.
// A symlink can arrive without any store call: a repository can commit one
// under .devcouncil/artifacts/ (git tracks links), a checkout materialises it,
// and devcouncil_list_artifacts then fails for good.
func TestAdversarialListSurvivesOneBadEntry(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("good.md", "ok", Metadata{Summary: "good"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile(t), filepath.Join(dir, "evil.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	arts, _ := store.List()
	found := false
	for _, a := range arts {
		found = found || a.Name == "good.md"
		if strings.Contains(a.Content, outsideSecret) {
			t.Fatal("List returned outside bytes")
		}
	}
	if !found {
		t.Fatalf("one bad entry hid every good artifact from List: %+v", arts)
	}
}

// Attack 3b: a dangling link must be reported, never treated as absence. The
// readContained comment says a present name is a refusal, but the error wraps
// fs.ErrNotExist with %w, so List's "removed while the walk was running" skip
// and Get's "not found" and readArtifact's "no sidecar" all swallow it.
func TestAdversarialDanglingContentLinkIsNotAbsence(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("plan.md", "real", Metadata{}); err != nil {
		t.Fatal(err)
	}
	content := filepath.Join(dir, "plan.md")
	_ = os.Remove(content)
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), content); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := store.Get("plan.md"); err == nil || strings.Contains(err.Error(), "not found") {
		t.Fatalf("Get over a dangling link = %v; want a refusal that names a link", err)
	}
	if arts, err := store.List(); err == nil {
		t.Fatalf("List silently dropped a dangling link entry: %+v", arts)
	}
}

func TestAdversarialDanglingSidecarLinkIsNotRevisionZero(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("plan.md", "real", Metadata{Summary: "s"}); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(dir, "plan.md.meta.json")
	_ = os.Remove(meta)
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), meta); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got, err := store.Get("plan.md"); err == nil {
		t.Fatalf("Get treated a dangling sidecar link as no history: revision %d", got.Revision)
	}
	if arts, err := store.List(); err == nil {
		t.Fatalf("List treated a dangling sidecar link as no history: %+v", arts)
	}
}

// Attack 4: names.
func TestAdversarialNameShapes(t *testing.T) {
	store, dir := newTestStore(t)
	if a, err := store.Create("lessons/../x.md", "x", Metadata{}); err != nil || a.Name != "x.md" {
		t.Fatalf("lessons/../x.md = %+v, %v", a, err)
	}
	if _, err := store.Create("a/../../x.md", "x", Metadata{}); err == nil {
		t.Fatal("escaping name accepted")
	}
	// Longest-name failure must leave nothing behind (no orphan content or temp).
	long := strings.Repeat("n", 250)
	before, _ := os.ReadDir(dir)
	if _, err := store.Create(long, "x", Metadata{}); err == nil {
		t.Skip("filesystem accepts 250-byte names")
	}
	after, _ := os.ReadDir(dir)
	if len(after) != len(before) {
		t.Fatalf("failed long-name create left entries: before %d after %d", len(before), len(after))
	}
	// Case alias: on a case-insensitive filesystem Get("PLAN.md") reads plan.md
	// but reports Name "PLAN.md", which List never returns.
	if _, err := store.Create("plan.md", "p", Metadata{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "PLAN.md")); err != nil {
		return // case-sensitive filesystem
	}
	if got, err := store.Get("PLAN.md"); err == nil && got.Name != "plan.md" {
		t.Errorf("Get(PLAN.md) names the artifact %q though the store holds plan.md", got.Name)
	}
}

// Attack 5: no read cap. A sparse 512 MiB artifact is read whole into memory
// by Get (io.ReadAll) and copied again by string(). The write side has no cap
// either (Create takes any string), and List loads every artifact's full
// content only for devcouncil_list_artifacts to discard it.
func TestAdversarialGetHasAReadCap(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("big.md", "x", Metadata{}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "big.md"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(512 << 20); err != nil {
		_ = f.Close()
		t.Skipf("sparse files unavailable: %v", err)
	}
	_ = f.Close()
	if got, err := store.Get("big.md"); err == nil {
		t.Fatalf("Get read a %d-byte artifact into memory with no cap", len(got.Content))
	}
}

// Attack 6: Update over a symlinked or hard-linked content path never writes
// through it.
func TestAdversarialUpdateNeverWritesThroughContentLinks(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			store, dir := newTestStore(t)
			if _, err := store.Create("plan.md", "real", Metadata{Summary: "s"}); err != nil {
				t.Fatal(err)
			}
			outside := outsideFile(t)
			content := filepath.Join(dir, "plan.md")
			_ = os.Remove(content)
			var err error
			if kind == "symlink" {
				err = os.Symlink(outside, content)
			} else {
				err = os.Link(outside, content)
			}
			if err != nil {
				t.Skipf("%s unavailable: %v", kind, err)
			}
			_, _ = store.Update("plan.md", "OVERWRITTEN", nil)
			b, _ := os.ReadFile(outside)
			if string(b) != outsideSecret {
				t.Fatalf("Update wrote through the %s: outside now %q", kind, b)
			}
		})
	}
}

// Attack 7: errors name the store path, never the target or its content.
func TestAdversarialErrorsDoNotLeakTarget(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("plan.md", "real", Metadata{}); err != nil {
		t.Fatal(err)
	}
	outside := outsideFile(t)
	swapForSymlink(t, filepath.Join(dir, "plan.md"), outside)
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error", what)
		}
		msg := err.Error()
		if strings.Contains(msg, outsideSecret) || strings.Contains(msg, filepath.Dir(outside)) || strings.Contains(msg, "outside.txt") {
			t.Errorf("%s leaks the link target: %v", what, err)
		}
	}
	_, err := store.Get("plan.md")
	check("Get", err)
	_, err = store.List()
	check("List", err)
	_, err = store.Update("plan.md", "y", nil)
	check("Update", err)
}
