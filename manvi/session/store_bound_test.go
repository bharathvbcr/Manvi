package session

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

// Save and Load share one bound: a final turn past what Load will read is
// refused, the previous generation is untouched and still loads.
func TestSaveRefusesAFileLoadWouldRefuse(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates ~65 MiB of payload")
	}
	dir := t.TempDir()
	store := NewStore(dir)
	id := newID(t)
	log := NewLog()
	conversation(t, log, "kept", "tool", "done")
	first, err := store.Save(id, log)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}

	mustAppend(t, log, TurnStart, nil)
	mustAppend(t, log, UserMessage, MessageData{Message: user(strings.Repeat("x", maxLoadBytes+1024))})
	_, err = store.Save(id, log)
	var tooLarge *TooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("Save of a file over the load bound returned %v, want *TooLargeError", err)
	}
	if tooLarge.Limit != maxLoadBytes || tooLarge.Size <= maxLoadBytes {
		t.Errorf("error carries size %d limit %d", tooLarge.Size, tooLarge.Limit)
	}

	after, err := os.ReadFile(first.Path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the previous generation changed (err %v)", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries after a refused save, want only the first generation", len(entries))
	}
	loaded, report, err := NewStore(dir).Load(id)
	if err != nil || report.Generation != 1 || loaded.Len() != first.Events {
		t.Fatalf("the earlier session no longer loads: gen %d, err %v", report.Generation, err)
	}
}

// A file exactly at the bound is saved and loads; the attempts that land over
// it are refused, so the search below also exercises the one-byte-over side.
func TestSaveAcceptsAFileExactlyAtTheLoadBound(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates ~65 MiB of payload")
	}
	// The header's timestamps vary in width, so the padding is tuned by what
	// each attempt actually wrote rather than predicted.
	pad := maxLoadBytes - 2048
	for attempt := 0; attempt < 12; attempt++ {
		dir := t.TempDir()
		store := NewStore(dir)
		id := newID(t)
		log := NewLog()
		mustAppend(t, log, TurnStart, nil)
		mustAppend(t, log, UserMessage, MessageData{Message: user(strings.Repeat("x", pad))})
		report, err := store.Save(id, log)
		var tooLarge *TooLargeError
		switch {
		case errors.As(err, &tooLarge):
			pad -= tooLarge.Size - maxLoadBytes
			continue
		case err != nil:
			t.Fatal(err)
		}
		info, err := os.Stat(report.Path)
		if err != nil {
			t.Fatal(err)
		}
		if int(info.Size()) < maxLoadBytes {
			pad += maxLoadBytes - int(info.Size())
			continue
		}
		if _, _, err := NewStore(dir).Load(id); err != nil {
			t.Fatalf("a file exactly at the bound does not load: %v", err)
		}
		return
	}
	t.Fatal("could not land a file exactly on the bound")
}
