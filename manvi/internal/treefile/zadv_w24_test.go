package treefile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// foldMatrix are spelling pairs the folding has to get right or leave to the
// file system: the stored spelling first, the candidate second.
var zadvFoldMatrix = [][2]string{
	{"k", "\u212a"},            // Kelvin sign
	{"s", "\u017f"},            // long s
	{"i", "\u0130"},            // Turkish dotted capital I
	{"i", "\u0131"},            // Turkish dotless i
	{"stra\u00dfe", "STRASSE"}, // sharp s
	{"stra\u00dfe", "STRA\u1e9eE"},
	{"\u03c3", "\u03c2"}, // sigma, final sigma
	{"\u03a3", "\u03c2"},
	{"caf\u00e9", "cafe\u0301"}, // NFC vs NFD
	{"cafe\u0301", "caf\u00e9"},
	{"\u1100\u1161", "\uac00"}, // Hangul jamo vs syllable
	{"\uff41", "a"},            // fullwidth a
	{"\uff21", "\uff41"},       // fullwidth A, a
	{"Foo", "fOO"},
}

// zadvFSAnswers asks the real file system whether candidate resolves to an entry
// stored as stored.
func zadvFSAnswers(t *testing.T, root *os.Root, candidate string) bool {
	t.Helper()
	_, err := root.Lstat(candidate)
	if err == nil {
		return true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	t.Fatalf("lstat %q: %v", candidate, err)
	return false
}

// SAFETY: wherever the file system reads the candidate as the stored entry,
// CheckAlias must refuse it. A nil there is a Create that lands in someone else's
// slot.
func TestZadvW24CheckAliasNeverPassesWhatTheFileSystemReadsAsExisting(t *testing.T) {
	over := 0
	for _, pair := range zadvFoldMatrix {
		stored, cand := pair[0], pair[1]
		anchor, _ := tree(t)
		if err := os.WriteFile(filepath.Join(anchor, stored), []byte("x"), 0o600); err != nil {
			t.Logf("file system refused to store %q: %v", stored, err)
			continue
		}
		root, err := Open(anchor)
		if err != nil {
			t.Fatal(err)
		}
		fsSays := zadvFSAnswers(t, root, cand)
		err = CheckAlias(root, cand)
		_ = root.Close()
		if fsSays && err == nil {
			t.Errorf("stored %+q, candidate %+q: the file system reads them as one entry and CheckAlias passed it", stored, cand)
		}
		if !fsSays && err != nil {
			over++
			t.Logf("over-refusal (conservative): stored %+q, candidate %+q are distinct on this file system, refused: %v", stored, cand, err)
		}
		// The stored spelling itself must always pass.
		root, _ = Open(anchor)
		if err := CheckAlias(root, stored); err != nil {
			t.Errorf("the stored spelling %+q was refused: %v", stored, err)
		}
		_ = root.Close()
	}
	t.Logf("%d of %d pairs over-refused", over, len(zadvFoldMatrix))
}

// An entry another writer creates under the exact name between the listing and
// the look is no alias. Race a creator against CheckAlias: the answer for the name
// being created must never be an alias refusal once it exists exactly.
func TestZadvW24CheckAliasIgnoresAnExactEntryAnotherWriterJustMade(t *testing.T) {
	anchor, _ := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 200; i++ {
		name := fmt.Sprintf("Race%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-stop:
			default:
				_ = os.WriteFile(filepath.Join(anchor, name), nil, 0o600)
			}
		}()
		if err := CheckAlias(root, name); err != nil {
			t.Errorf("CheckAlias(%q) refused a name that is either absent or exact: %v", name, err)
		}
	}
	close(stop)
	wg.Wait()
}

// How much does one check cost in a large directory? It lists the whole
// directory, so it is linear: bound it for 10k entries.
func TestZadvW24CheckAliasCostInATenThousandEntryDirectory(t *testing.T) {
	anchor, _ := tree(t)
	for i := 0; i < 10000; i++ {
		f, err := os.Create(filepath.Join(anchor, fmt.Sprintf("entry-%05d", i))) // #nosec G304 -- a path under this test's own t.TempDir
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	start := time.Now()
	const rounds = 20
	for i := 0; i < rounds; i++ {
		if err := CheckAlias(root, "fresh-name"); err != nil {
			t.Fatal(err)
		}
	}
	per := time.Since(start) / rounds
	t.Logf("CheckAlias in a 10k-entry directory: %v per call", per)
	if per > 100*time.Millisecond {
		t.Errorf("one alias check costs %v in a 10k directory", per)
	}
	if err := CheckAlias(root, "ENTRY-00042"); !errors.Is(err, ErrAliased) {
		t.Errorf("an alias in a big directory was not found: %v", err)
	}
}

// The reader of an entry that is being published by link is refused as a hard
// link to a file outside the tree when the publisher is slow between the link and
// the removal of its stage. That slow step is the directory fsync, which on a
// loaded disk takes tens of milliseconds, far over the 15 ms settle window.
func TestZadvW24ASlowPublishIsNotReadAsAPlantedHardLink(t *testing.T) {
	anchor, _ := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	linked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Publish(root, "entry", []byte("payload"), PublishOptions{SyncDir: func(*os.Root) error {
			close(linked)
			<-release // a slow directory fsync
			return nil
		}})
	}()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	select {
	case <-linked:
	case <-time.After(10 * time.Second):
		t.Fatal("publish never reached its directory sync")
	}
	// The fsync of a loaded host: 100 ms.
	go func() { time.Sleep(100 * time.Millisecond); close(release) }()
	data, err := ReadFile(root, "entry", ReadOptions{Limit: 1 << 20, SingleLink: true})
	if perr := <-done; perr != nil {
		t.Fatalf("publish: %v", perr)
	}
	if err != nil {
		t.Fatalf("a reader that arrived while a legitimate publish was fsyncing was refused: %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("read %q", data)
	}
}

// A planted hard link is never returned, inside the settle window or after it,
// and is refused within a bounded time.
func TestZadvW24APlantedHardLinkIsNeverReadAndIsRefusedQuickly(t *testing.T) {
	anchor, outside := tree(t)
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("outside data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(anchor, "entry")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	start := time.Now()
	data, err := ReadFile(root, "entry", ReadOptions{Limit: 1 << 20, SingleLink: true})
	took := time.Since(start)
	if err == nil || !errors.Is(err, ErrRefused) {
		t.Fatalf("a planted link was read (%q) or not refused: %v", data, err)
	}
	if len(data) != 0 {
		t.Fatalf("data returned beside a refusal: %q", data)
	}
	if took > 500*time.Millisecond {
		t.Errorf("refusal took %v", took)
	}
	// Racing: a link planted while another reader is mid-wait is still refused.
	for i := 0; i < 20; i++ {
		_ = os.Remove(filepath.Join(anchor, "e2"))
		_ = os.Link(secret, filepath.Join(anchor, "e2"))
		if d, err := ReadFile(root, "e2", ReadOptions{Limit: 1 << 20, SingleLink: true}); err == nil {
			t.Fatalf("round %d: planted link read: %q", i, d)
		}
	}
}

// EACCES is a fact about this process, not a refusal of the entry.
func TestZadvW24PermissionFailuresAreNotRefusals(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits")
	}
	anchor, _ := tree(t)
	sub := filepath.Join(anchor, "sub")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(anchor, "locked"), []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o750) }) // #nosec G302 -- a path under this test's own t.TempDir
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"locked", "sub/f"} {
		_, err := ReadFile(root, name, ReadOptions{Limit: 1 << 20})
		if err == nil {
			t.Errorf("%s: read succeeded", name)
			continue
		}
		if errors.Is(err, ErrRefused) {
			t.Errorf("%s: EACCES reported as a refusal of the entry: %v", name, err)
		}
	}
}

// A stage that cannot be removed after the link landed is a published write.
// Make the removal fail by making the directory unwritable between link and
// removal: the sync hook is the moment.
func TestZadvW24AFailedStageRemovalByPermissionStillReportsPublished(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits")
	}
	anchor, _ := tree(t)
	dirPath := filepath.Join(anchor, "d")
	if err := os.Mkdir(dirPath, 0o750); err != nil {
		t.Fatal(err)
	}
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	child, err := Child(root, "d", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Close() }()
	t.Cleanup(func() { _ = os.Chmod(dirPath, 0o750) }) // #nosec G302 -- a path under this test's own t.TempDir
	// The stage only outlives a publish that linked, and a link publish removes it
	// before the directory sync, so the unremovable stage is arranged where the name
	// lands: the stand-in for the rename links the stage onto the name itself, makes
	// the directory read-only, and reports success, as a rename that landed would.
	saved := noReplaceRename
	defer func() { noReplaceRename = saved }()
	noReplaceRename = func(dir *os.Root, stage, name string) error {
		if err := dir.Link(stage, name); err != nil {
			return err
		}
		return os.Chmod(dirPath, 0o500) // no removal possible
	}
	err = Publish(child, "entry", []byte("p"), PublishOptions{})
	if err == nil {
		t.Fatal("an unremovable stage was not reported")
	}
	if !errors.Is(err, ErrPublishedUnsynced) {
		t.Errorf("the write landed but is reported as a plain failure: %v", err)
	}
	if !strings.Contains(err.Error(), "entry") {
		t.Errorf("the error does not name the entry: %v", err)
	}
}
