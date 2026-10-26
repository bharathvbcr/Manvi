package artifacts

import (
	"errors"
	"os"
	"testing"
)

// failDirSync makes every directory sync fail until the test ends or the
// returned func restores it.
func failDirSync(t *testing.T) (restore func()) {
	t.Helper()
	old := syncDir
	syncDir = func(string) error { return errors.New("injected directory sync failure") }
	restore = func() { syncDir = old }
	t.Cleanup(restore)
	return restore
}

// A write whose name was published but whose directory could not be synced is
// committed: the bytes are in the file and the name is there, only the proof
// that both survive a crash is missing. Create must hand back the artifact it
// made rather than report "not written", and the sidecar must be written too,
// or Get would read the entry as revision 0 (another writer mid-create).
func TestCreateWhoseDirectorySyncFailsIsStillPublished(t *testing.T) {
	store, _ := newTestStore(t)
	_ = failDirSync(t)

	art, err := store.Create("plan.md", "# plan", Metadata{Summary: "s"})
	if err == nil {
		t.Fatal("an unconfirmed directory sync was reported as plain success")
	}
	if art == nil {
		t.Fatalf("Create reported the artifact as not written although it was published: %v", err)
	}
	if art.Revision != 1 || art.Content != "# plan" {
		t.Fatalf("the returned artifact is wrong: %+v", art)
	}
	assertUnsyncedErr(t, err)

	got, gerr := store.Get("plan.md")
	if gerr != nil || got.Content != "# plan" || got.Revision != 1 || got.Metadata.Summary != "s" {
		t.Fatalf("the published artifact is not readable: %+v, %v", got, gerr)
	}
	all, lerr := store.List()
	if lerr != nil || len(all) != 1 || all[0].Name != "plan.md" || all[0].Revision != 1 {
		t.Fatalf("List disagrees with Get: %+v, %v", all, lerr)
	}
	if _, serr := os.Stat(got.Path + metaSuffix); serr != nil {
		t.Fatalf("the sidecar of a published artifact was not written: %v", serr)
	}

	// A retry of Create gets ErrExists, and only because the artifact exists:
	// that is the answer a caller who retried after a failure report would get,
	// which is why the failure report must not be sent for a published write.
	if _, err := store.Create("plan.md", "again", Metadata{}); !errors.Is(err, ErrExists) {
		t.Fatalf("retry of Create = %v, want ErrExists", err)
	}
}

// Update with a directory sync that fails: the new revision is the current
// one, and what the artifact carried before (created_at, the revision count)
// is continued, not reset.
func TestUpdateWhoseDirectorySyncFailsIsStillPublished(t *testing.T) {
	store, _ := newTestStore(t)
	first, err := store.Create("plan.md", "v1", Metadata{Summary: "one"})
	if err != nil {
		t.Fatal(err)
	}
	restore := failDirSync(t)

	art, err := store.Update("plan.md", "v2", &Metadata{Summary: "two"})
	if err == nil {
		t.Fatal("an unconfirmed directory sync was reported as plain success")
	}
	if art == nil {
		t.Fatalf("Update reported the artifact as not written although it was published: %v", err)
	}
	if art.Revision != 2 || art.Content != "v2" {
		t.Fatalf("the returned artifact is wrong: %+v", art)
	}
	assertUnsyncedErr(t, err)

	got, gerr := store.Get("plan.md")
	if gerr != nil || got.Content != "v2" || got.Revision != 2 || got.Metadata.Summary != "two" {
		t.Fatalf("the new revision is not the current one: %+v, %v", got, gerr)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("the history was reset: created_at %v, was %v", got.CreatedAt, first.CreatedAt)
	}

	// The next update continues from revision 2.
	restore()
	next, err := store.UpdateIfRevision("plan.md", "v3", nil, 2)
	if err != nil || next.Revision != 3 {
		t.Fatalf("the update after an unconfirmed one: %+v, %v", next, err)
	}
}
