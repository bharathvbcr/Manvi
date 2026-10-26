package main

import (
	"bytes"
	"strings"
	"testing"
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
