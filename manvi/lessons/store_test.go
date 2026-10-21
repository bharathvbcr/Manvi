package lessons

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/artifacts"
)

func parsed(t *testing.T) Result {
	t.Helper()
	r, err := Parse(good(""))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newStore(t *testing.T) (*artifacts.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "artifacts")
	s, err := artifacts.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestSaveWritesOnlyUnderLessonsLibrary(t *testing.T) {
	s, dir := newStore(t)
	name, rev, err := Save(s, "gitpulse", "sess-1", parsed(t))
	if err != nil {
		t.Fatal(err)
	}
	if rev != 1 {
		t.Errorf("revision %d", rev)
	}
	if !strings.HasPrefix(name, "lessons/gitpulse/") || !strings.HasSuffix(name, ".md") {
		t.Fatalf("stored as %q", name)
	}
	// Every file the store made is under lessons/gitpulse/ — nothing beside it,
	// and in particular no database: this feature owns no schema.
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if !strings.HasPrefix(filepath.ToSlash(rel), "lessons/gitpulse/") {
			t.Errorf("file outside lessons/gitpulse/: %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a verify report degraded", "set the verify command", "sess-1", "helped", "low"} {
		if !strings.Contains(got.Content, want) {
			t.Errorf("entry lacks %q:\n%s", want, got.Content)
		}
	}
}

func TestSaveRefusesTraversalAndMalformedLibraries(t *testing.T) {
	s, dir := newStore(t)
	for _, lib := range []string{
		"", "..", "../x", "a/b", "a\\b", "/abs", ".hidden", "-x", "UPPER", "a b", "x.meta.json",
		strings.Repeat("a", MaxLibraryLen+1), "a\x00b", "lessons/../..",
	} {
		if _, _, err := Save(s, lib, "s", parsed(t)); err == nil {
			t.Errorf("library %q was accepted", lib)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused save left %d entries behind", len(entries))
	}
}

func TestEntryNameIsDeterministicBoundedAndContained(t *testing.T) {
	a := EntryName("lib", "Cue: With  odd/../chars!!")
	b := EntryName("lib", "Cue: With  odd/../chars!!")
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	if EntryName("lib", "one cue") == EntryName("lib", "another cue") {
		t.Error("distinct cues collided")
	}
	long := EntryName("lib", strings.Repeat("word ", 500))
	if len(long) > len("lessons/lib/")+MaxSlugLen+len("-0123456789ab.md") {
		t.Errorf("slug is not bounded: %d", len(long))
	}
	for _, cue := range []string{"../../etc/passwd", "a/b", "..", "\x00", "???", ""} {
		n := EntryName("lib", cue)
		if filepath.Dir(n) != "lessons/lib" || strings.Contains(filepath.Base(n), "..") {
			t.Errorf("cue %q escaped: %q", cue, n)
		}
	}
}

func TestSavingTheSameCueRevisesRatherThanDuplicates(t *testing.T) {
	s, _ := newStore(t)
	n1, r1, err := Save(s, "lib", "s1", parsed(t))
	if err != nil {
		t.Fatal(err)
	}
	n2, r2, err := Save(s, "lib", "s2", parsed(t))
	if err != nil {
		t.Fatal(err)
	}
	if n1 != n2 || r1 != 1 || r2 != 2 {
		t.Fatalf("got %s rev %d, then %s rev %d", n1, r1, n2, r2)
	}
}

func TestSaveRefusesNoEntryAndEmptyResults(t *testing.T) {
	s, dir := newStore(t)
	if _, _, err := Save(s, "lib", "s", Result{NoEntry: true}); err == nil {
		t.Error("NO_ENTRY was saved")
	}
	if _, _, err := Save(s, "lib", "s", Result{}); err == nil {
		t.Error("an empty result was saved")
	}
	if _, err := os.Stat(dir); err == nil {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Error("a refused save wrote something")
		}
	}
}

func TestDefaultLibraryDerivesFromTheRepositoryName(t *testing.T) {
	cases := map[string]string{"/home/u/Manvi": "manvi", "/x/Git Pulse!": "git-pulse", "/x/.git_hub": "git_hub"}
	for root, want := range cases {
		got, err := DefaultLibrary(root)
		if err != nil || got != want {
			t.Errorf("DefaultLibrary(%q) = %q, %v; want %q", root, got, err, want)
		}
	}
	if _, err := DefaultLibrary("/"); err == nil {
		t.Error("a root with no usable name produced a library")
	}
}

// This feature owns no DevCouncil schema: it must not import the dc-store or
// devmap-store clients, so a schema bump elsewhere cannot reach it.
func TestPackageImportsNoDevCouncilStore(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsedFile, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsedFile.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(path, "/store") || strings.Contains(path, "dcstore") || strings.Contains(path, "devmap") {
				t.Errorf("%s imports %s", f, path)
			}
		}
	}
}
