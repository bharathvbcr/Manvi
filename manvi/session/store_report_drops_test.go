package session

import (
	"encoding/json"
	"os"
	"testing"
)

// dropCreatedAt removes created_at from the current generation's header. The
// checksum covers the events only, so the file still loads.
func dropCreatedAt(t *testing.T, s *Store, id string) {
	t.Helper()
	s.mu.Lock()
	_, path, raw, err := s.readCurrent(id)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "created_at")
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A header with no created_at still states how many turns the earlier
// generations trimmed. The report is the delta against it, so reading it only
// when a date was present reported the whole cumulative count as this save's.
func TestSaveReportsOnlyNewDropsWhenTheEarlierHeaderLacksCreatedAt(t *testing.T) {
	dir, id := t.TempDir(), newID(t)
	first := w17Cycle(t, dir, id, NewLog(), "a", "b", "c", "d", "e", "f", "g", "h")
	if first.TrimmedTurns == 0 {
		t.Fatal("fixture did not trim")
	}
	store := tightStore(dir)
	dropCreatedAt(t, store, id)
	base := headerTrimmed(t, store, id)
	if base == 0 {
		t.Fatal("header carries no trimmed_turns")
	}

	second := w17Cycle(t, dir, id, nil, "i", "j", "k")
	got := headerTrimmed(t, tightStore(dir), id)
	if got < base {
		t.Fatalf("cumulative trimmed_turns went down: %d -> %d", base, got)
	}
	if second.TrimmedTurns != got-base {
		t.Fatalf("report says %d turn(s) dropped, but the cumulative count moved %d -> %d (%d)",
			second.TrimmedTurns, base, got, got-base)
	}
	if second.TrimmedTurns >= base {
		t.Fatalf("report %d repeats the earlier generations' %d", second.TrimmedTurns, base)
	}
}

// The same save with the date present is the control: report is the delta and
// the cumulative count is what the earlier headers claimed plus it.
func TestSaveReportsOnlyNewDropsWithCreatedAt(t *testing.T) {
	dir, id := t.TempDir(), newID(t)
	w17Cycle(t, dir, id, NewLog(), "a", "b", "c", "d", "e", "f", "g", "h")
	base := headerTrimmed(t, tightStore(dir), id)
	second := w17Cycle(t, dir, id, nil, "i", "j", "k")
	if got := headerTrimmed(t, tightStore(dir), id); second.TrimmedTurns != got-base {
		t.Fatalf("report %d, cumulative %d -> %d", second.TrimmedTurns, base, got)
	}
}
