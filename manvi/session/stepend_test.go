package session

import (
	"encoding/json"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// A log written before step/end carried a payload, and one written after, are
// the same kind of log: both restore, both derive the same history, and the
// payload survives a save and a load. Jarvis links this package and reads
// whatever is on disk, so the old shape is not allowed to stop working.
func TestStepEndWithAndWithoutPayloadBothRestore(t *testing.T) {
	log := NewLog()
	streamedTurn(t, log, "old", 1) // step/end with no payload, as old logs have it
	mustAppend(t, log, TurnStart, nil)
	mustAppend(t, log, UserMessage, MessageData{Message: user("new")})
	mustAppend(t, log, StepStart, nil)
	mustAppend(t, log, AssistantMessage, MessageData{Message: assistant("answer to new")})
	want := StepEndData{
		Usage:       llm.Usage{InputTokens: 12, OutputTokens: 3, CacheReadTokens: 4},
		CostNanoUSD: 99, Priced: true,
	}
	mustAppend(t, log, StepEnd, want)
	mustAppend(t, log, TurnEnd, nil)

	store := NewStore(t.TempDir())
	id := newID(t)
	if _, err := store.Save(id, log); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, _, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if derived(t, loaded) != derived(t, log) {
		t.Fatal("the restored log derives different messages than the in-memory one")
	}

	var payloads, bare int
	for _, e := range loaded.Events() {
		if e.Type != StepEnd {
			continue
		}
		if len(e.Data) == 0 {
			bare++
			continue
		}
		var got StepEndData
		if err := json.Unmarshal(e.Data, &got); err != nil {
			t.Fatalf("step/end payload does not decode: %v", err)
		}
		if got != want {
			t.Errorf("step/end payload = %+v, want %+v", got, want)
		}
		payloads++
	}
	if payloads != 1 || bare != 1 {
		t.Errorf("restored %d payloaded and %d bare step/end events, want 1 and 1", payloads, bare)
	}
}
