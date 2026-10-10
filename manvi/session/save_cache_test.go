package session

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func userTurn(tb testing.TB, log *Log, text string) {
	tb.Helper()
	benchAppend(tb, log, TurnStart, nil)
	benchAppend(tb, log, UserMessage, MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: text}}}})
	benchAppend(tb, log, AssistantChunk, map[string]any{"text": "tok"})
	benchAppend(tb, log, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "re: " + text}}}})
}

// A save encodes the events appended since the last one and reuses the rest.
// Re-encoding the whole session each turn was thousands of allocations per
// save; the bound is a few hundred, well under what encoding a 2000-event log
// even once would take.
func TestSaveEncodesOnlyWhatWasAppended(t *testing.T) {
	log := NewLog()
	for i := 0; i < 500; i++ {
		userTurn(t, log, "turn "+strconv.Itoa(i))
	}
	store := NewStore(t.TempDir())
	if _, err := store.Save("a1", log); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(5, func() {
		userTurn(t, log, "another")
		if _, err := store.Save("a1", log); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 1500 {
		t.Fatalf("a save of a 2000-event log made %.0f allocations; it is encoding the whole session again", allocs)
	}
}

// Reusing encodings must not change what is written: across saves, with and
// without retention trimming, the file holds what a log that never cached
// would write, chunks excluded, and still verifies its own checksum on load.
func TestCachedSavesWriteWhatAFreshSaveWrites(t *testing.T) {
	for _, maxBytes := range []int{DefaultMaxBytes, 6000} {
		cached := NewStore(t.TempDir())
		cached.maxBytes = maxBytes
		log := NewLog()
		for i := 0; i < 40; i++ {
			userTurn(t, log, strings.Repeat("w", 40+i))
			if i%3 != 0 {
				continue
			}
			if _, err := cached.Save("b2", log); err != nil {
				t.Fatalf("max %d turn %d: %v", maxBytes, i, err)
			}
			loaded, report, err := cached.Load("b2")
			if err != nil {
				t.Fatalf("max %d turn %d: the saved file does not load: %v", maxBytes, i, err)
			}

			fresh := NewStore(t.TempDir())
			fresh.maxBytes = maxBytes
			if _, err := fresh.Save("b2", log); err != nil {
				t.Fatal(err)
			}
			want, wantReport, err := fresh.Load("b2")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.Events(), want.Events()) || report.Events != wantReport.Events || report.Turns != wantReport.Turns {
				t.Fatalf("max %d turn %d: a cached save wrote %d events / %d turns, a fresh one %d / %d",
					maxBytes, i, report.Events, report.Turns, wantReport.Events, wantReport.Turns)
			}
			for _, e := range loaded.Events() {
				if e.Type == AssistantChunk {
					t.Fatalf("a chunk was saved")
				}
			}
		}
	}
}
