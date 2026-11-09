package session

import (
	"encoding/json"
	"testing"
)

// headerTrimmed reads trimmed_turns from the file the store last wrote for id.
func headerTrimmed(t *testing.T, s *Store, id string) int {
	t.Helper()
	raw, err := readFileOf(t, s, id)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		TrimmedTurns int `json:"trimmed_turns"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.TrimmedTurns
}

// The header's trimmed_turns is the cumulative count of whole turns dropped from
// the front of the session. A store that kept the log in memory used to count the
// same turns again on every re-save, because the in-memory log still holds them
// while the file does not (ft-9a5710b6 item 7: a cached store reported 7 where a
// fresh one reported 5).
func TestTrimmedTurnsDoesNotCountTheSameTurnsOnEveryResave(t *testing.T) {
	labels := []string{"one", "two", "three", "four", "five", "six", "seven", "eight"}

	cachedDir, id := t.TempDir(), newID(t)
	cached := tightStore(cachedDir)
	log := NewLog()
	for _, label := range labels {
		trimTurn(t, log, label)
	}
	first, err := cached.Save(id, log)
	if err != nil || first.TrimmedTurns == 0 {
		t.Fatalf("first save: %v, trimmed %d: the fixture trims nothing, so it says nothing", err, first.TrimmedTurns)
	}
	if got := headerTrimmed(t, cached, id); got != first.TrimmedTurns {
		t.Fatalf("first save wrote trimmed_turns %d, reported %d", got, first.TrimmedTurns)
	}

	// The same in-memory log saved again with nothing new: the file already lacks
	// the turns, so nothing more was dropped.
	again, err := cached.Save(id, log)
	if err != nil {
		t.Fatal(err)
	}
	if again.TrimmedTurns != 0 {
		t.Fatalf("a re-save of an unchanged log reports %d turn(s) dropped, want 0", again.TrimmedTurns)
	}
	if got := headerTrimmed(t, cached, id); got != first.TrimmedTurns {
		t.Fatalf("a re-save of an unchanged log wrote trimmed_turns %d, want %d", got, first.TrimmedTurns)
	}

	// One more turn through the cached store, and the same file resumed by a fresh
	// store: both must write the same cumulative count.
	trimTurn(t, log, "nine")
	if _, err := cached.Save(id, log); err != nil {
		t.Fatal(err)
	}
	cachedCount := headerTrimmed(t, cached, id)

	freshDir := t.TempDir()
	fresh := tightStore(freshDir)
	freshLog := NewLog()
	for _, label := range labels {
		trimTurn(t, freshLog, label)
	}
	if _, err := fresh.Save(id, freshLog); err != nil {
		t.Fatal(err)
	}
	resumed := tightStore(freshDir)
	loaded, _, err := resumed.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	trimTurn(t, loaded, "nine")
	if _, err := resumed.Save(id, loaded); err != nil {
		t.Fatal(err)
	}
	if freshCount := headerTrimmed(t, resumed, id); cachedCount != freshCount {
		t.Fatalf("cached store wrote trimmed_turns %d, fresh store %d", cachedCount, freshCount)
	}
}
