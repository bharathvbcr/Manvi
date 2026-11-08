package session

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// Retention trims whole turns off the front (trimEncoded). On a provider that
// binds thinking to the history, every signed block that survives was produced
// behind messages the file no longer holds, so it is stale; blocks produced
// after the trimmed file was loaded are signed under the trimmed prefix and
// stay valid. The removal that is allowed is a leading run, oldest first
// (shared/model-migration.md, "Breaking change 3", "What keeps later blocks
// valid"; shared/preserved-thinking-migration/causes.md, rows rolling_truncation
// and tail_kept).

func trimTurn(t *testing.T, l *Log, label string) {
	t.Helper()
	mustAppend(t, l, TurnStart, nil)
	mustAppend(t, l, UserMessage, MessageData{Message: user(label + strings.Repeat("y", 1500))})
	mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "t", Signature: "sig-" + label}, llm.TextBlock{Text: "done"}}}})
	mustAppend(t, l, TurnEnd, nil)
}

// signatures lists the signed blocks msgs carry, in order.
func signatures(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if r, ok := b.(llm.ReasoningBlock); ok && r.Signature != "" {
				out = append(out, r.Signature)
			}
		}
	}
	return out
}

func strippedFor(t *testing.T, l *Log) (kept []string, dropped int, before int) {
	t.Helper()
	msgs, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	out, n, err := l.StripStaleReasoning(msgs)
	if err != nil {
		t.Fatal(err)
	}
	return signatures(out), n, len(signatures(msgs))
}

func trimmedMarkers(l *Log) int {
	n := 0
	for _, e := range l.Events() {
		if e.Type == WireFrame && strings.Contains(string(e.Data), `"why":"trimmed"`) {
			n++
		}
	}
	return n
}

// A trimmed file with no wire frame at all (a log written before wave 13, or on
// a provider that binds nothing and later replayed to Anthropic) still reports
// that its surviving blocks are stale, and only those.
func TestAdvTrimmedLogStripsEverySignedBlockItKept(t *testing.T) {
	store := NewStore(t.TempDir())
	store.setLimits(6000, 0)
	id := newID(t)
	l := NewLog()
	for _, label := range []string{"one", "two", "three", "four", "five"} {
		trimTurn(t, l, label)
	}
	report, err := store.Save(id, l)
	if err != nil {
		t.Fatal(err)
	}
	if report.TrimmedTurns == 0 {
		t.Fatal("the fixture was not trimmed, so the test says nothing")
	}
	restored, _, err := NewStore(store.Dir()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	kept, dropped, before := strippedFor(t, restored)
	if before == 0 {
		t.Fatal("the trim left no signed block, so the test says nothing")
	}
	if dropped != before || len(kept) != 0 {
		t.Fatalf("%d signed blocks survived the trim and %d were stripped (kept %v); each was signed behind turns the file no longer holds",
			before, dropped, kept)
	}

	trimTurn(t, restored, "after")
	kept, dropped, _ = strippedFor(t, restored)
	if dropped != before || len(kept) != 1 || kept[0] != "sig-after" {
		t.Errorf("after one more turn stripped %d (want %d) and kept %v, want only the block signed after the load",
			dropped, before, kept)
	}
}

// A second trim makes the blocks of the first resumed run stale too, and the
// marker survives the file being written and read again without multiplying.
func TestAdvRepeatedTrimsMarkEachResume(t *testing.T) {
	store := NewStore(t.TempDir())
	store.setLimits(6000, 0)
	id := newID(t)
	l := NewLog()
	for _, label := range []string{"one", "two", "three", "four", "five"} {
		trimTurn(t, l, label)
	}
	if _, err := store.Save(id, l); err != nil {
		t.Fatal(err)
	}
	// The same store loads and saves, as a run does.
	reloader := NewStore(store.Dir())
	reloader.setLimits(6000, 0)
	log, _, err := reloader.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := trimmedMarkers(log); got != 1 {
		t.Fatalf("a freshly trimmed file loads with %d markers, want 1", got)
	}
	for _, label := range []string{"six", "seven", "eight", "nine"} {
		trimTurn(t, log, label)
	}
	report, err := reloader.Save(id, log)
	if err != nil {
		t.Fatal(err)
	}
	if report.TrimmedTurns == 0 {
		t.Fatal("the second save did not trim, so the test says nothing")
	}
	third, _, err := NewStore(store.Dir()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	kept, dropped, before := strippedFor(t, third)
	if dropped != before || len(kept) != 0 {
		t.Fatalf("second trim: %d signed blocks, %d stripped, kept %v; the blocks of the resumed run were signed behind turns that went too",
			before, dropped, kept)
	}
	trimTurn(t, third, "last")
	kept, _, _ = strippedFor(t, third)
	if len(kept) != 1 || kept[0] != "sig-last" {
		t.Errorf("kept %v, want only sig-last", kept)
	}
}

// Loading adds nothing the file already answers for, and an untrimmed log gets
// no marker.
func TestAdvTrimMarkerIsWrittenOncePerTrim(t *testing.T) {
	store := NewStore(t.TempDir())
	id := newID(t)
	plain := NewLog()
	trimTurn(t, plain, "solo")
	if _, err := store.Save(id, plain); err != nil {
		t.Fatal(err)
	}
	got, _, err := NewStore(store.Dir()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if n := trimmedMarkers(got); n != 0 {
		t.Errorf("an untrimmed session loaded with %d trim markers", n)
	}
	if kept, dropped, _ := strippedFor(t, got); dropped != 0 || len(kept) != 1 {
		t.Errorf("an untrimmed session lost reasoning: kept %v, dropped %d", kept, dropped)
	}

	trimming := NewStore(t.TempDir())
	trimming.setLimits(6000, 0)
	id = newID(t)
	l := NewLog()
	for _, label := range []string{"one", "two", "three", "four", "five"} {
		trimTurn(t, l, label)
	}
	if _, err := trimming.Save(id, l); err != nil {
		t.Fatal(err)
	}
	first, _, err := NewStore(trimming.Dir()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	roomy := NewStore(trimming.Dir())
	loaded, _, err := roomy.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := roomy.Save(id, loaded); err != nil {
		t.Fatal(err)
	}
	again, _, err := NewStore(trimming.Dir()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := trimmedMarkers(first), trimmedMarkers(again); a != 1 || b != 1 {
		t.Errorf("markers = %d then %d after a save and a load with no new trim, want 1 and 1", a, b)
	}
}
