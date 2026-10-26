package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/artifacts"
)

// A lesson saved with its directory sync unconfirmed is reported as stored,
// with a warning that says not to re-run, and is not an error: a re-run would
// file a second revision of it.
func TestReportStoredWarnsWhenTheDirectorySyncWasUnconfirmed(t *testing.T) {
	var out bytes.Buffer
	if err := reportStored(&out, "lessons/lib/a.md", 2, "lib", "sess", "the cue", false, true); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "stored lessons/lib/a.md (revision 2) in library lib from session sess\n  cue: the cue\n") {
		t.Errorf("the stored line changed shape: %q", got)
	}
	if !strings.Contains(got, "warning: the lesson is saved, but syncing its directory failed") ||
		!strings.Contains(got, "do not re-run") {
		t.Errorf("no durability warning: %q", got)
	}

	out.Reset()
	if err := reportStored(&out, "lessons/lib/a.md", 1, "lib", "sess", "the cue", false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "warning") {
		t.Errorf("a confirmed write was reported with a warning: %q", out.String())
	}
}

// applyReflection end to end over a store whose directory sync fails: the
// lesson is published, so the reflection succeeds, the stored line is printed
// with the do-not-re-run warning, and the lesson reads back.
func TestApplyReflectionOverAFailingDirectorySyncReportsStoredWithAWarning(t *testing.T) {
	store, err := artifacts.NewStoreWithDirSync(filepath.Join(t.TempDir(), "artifacts"), func(string) error {
		return errors.New("injected directory sync failure")
	})
	if err != nil {
		t.Fatal(err)
	}
	var out, notes bytes.Buffer
	if err := applyReflection(&out, &notes, store, "testlib", "sess", reflectGood, nil); err != nil {
		t.Fatalf("a stored lesson was reported as a failure: %v\n%s", err, notes.String())
	}
	if !strings.Contains(out.String(), "stored lessons/testlib/") ||
		!strings.Contains(out.String(), "do not re-run") {
		t.Errorf("out lacks the stored line or the warning: %q", out.String())
	}
	list, err := store.List()
	if err != nil || len(list) != 1 || list[0].Revision != 1 {
		t.Fatalf("the lesson is not readable: %v, %v", list, err)
	}
}
