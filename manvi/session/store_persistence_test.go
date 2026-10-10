package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// streamedTurn records one turn the way the agent loop does: the model's reply
// arrives as `chunks` stream fragments, then as one complete message.
func streamedTurn(t *testing.T, l *Log, prompt string, chunks int) {
	t.Helper()
	mustAppend(t, l, TurnStart, nil)
	mustAppend(t, l, UserMessage, MessageData{Message: user(prompt)})
	mustAppend(t, l, StepStart, nil)
	for i := 0; i < chunks; i++ {
		mustAppend(t, l, AssistantChunk, llm.Chunk{Kind: llm.ChunkText, Text: "tok"})
	}
	mustAppend(t, l, AssistantMessage, MessageData{Message: assistant("answer to " + prompt)})
	mustAppend(t, l, StepEnd, nil)
	mustAppend(t, l, TurnEnd, nil)
}

// Stream fragments carry no history, so they must not spend the byte budget
// that exists to bound history. Eight turns of 10k fragments is ~11 MB against
// an 8 MiB budget while the history itself is a few kilobytes.
func TestStreamChunksDoNotSpendTheHistoryBudget(t *testing.T) {
	store := NewStore(t.TempDir())
	id := newID(t)
	log := NewLog()
	for i := 0; i < 8; i++ {
		streamedTurn(t, log, fmt.Sprintf("prompt %d", i), 10_000)
	}

	report, err := store.Save(id, log)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if report.TrimmedTurns != 0 || report.Oversize {
		t.Fatalf("history of a few kilobytes was trimmed: TrimmedTurns=%d Oversize=%v",
			report.TrimmedTurns, report.Oversize)
	}

	loaded, got, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Turns != 8 || got.TrimmedTurns != 0 {
		t.Fatalf("loaded %d turns with %d trimmed, want 8 and 0", got.Turns, got.TrimmedTurns)
	}
	if derived(t, loaded) != derived(t, log) {
		t.Fatal("the restored log derives different messages than the in-memory one")
	}
	for _, e := range loaded.Events() {
		if e.Type == AssistantChunk {
			t.Fatalf("a stream fragment was persisted: %s", e.Data)
		}
	}
}

// The complete message is scrubbed; fragments of the same text cannot be, because
// a value split across chunks matches no exact-value scrubber. Not persisting
// them is what keeps the credential out of the file.
func TestStreamChunksDoNotPersistASplitCredential(t *testing.T) {
	const secret = "sk-ant-api03-AUDITKEYVALUE0123456789"
	log := NewLog()
	log.SetScrubber(func(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") })
	mustAppend(t, log, TurnStart, nil)
	mustAppend(t, log, UserMessage, MessageData{Message: user("hi")})
	for _, frag := range []string{secret[:14], secret[14:]} {
		mustAppend(t, log, AssistantChunk, llm.Chunk{Kind: llm.ChunkText, Text: frag})
	}
	mustAppend(t, log, AssistantMessage, MessageData{Message: assistant("here: " + secret)})
	mustAppend(t, log, TurnEnd, nil)

	dir := t.TempDir()
	report, err := NewStore(dir).Save(newID(t), log)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(report.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{secret[:14], secret[14:]} {
		if strings.Contains(string(raw), frag) {
			t.Fatalf("fragment %q of a watched credential reached the session file", frag)
		}
	}
}

// Trimming still bounds history-bearing bytes once chunks are out of the way.
func TestTrimStillBoundsHistoryWhenChunksArePresent(t *testing.T) {
	store := NewStore(t.TempDir())
	store.setLimits(6000, 0)
	log := NewLog()
	for i := 0; i < 12; i++ {
		streamedTurn(t, log, fmt.Sprintf("prompt %d %s", i, strings.Repeat("x", 400)), 50)
	}
	report, err := store.Save(newID(t), log)
	if err != nil {
		t.Fatal(err)
	}
	if report.TrimmedTurns == 0 {
		t.Fatal("oversize history was not trimmed")
	}
}

// preTurn is what both faces record before the loop opens the turn.
func preTurn(t *testing.T, l *Log, tokens int) {
	t.Helper()
	mustAppend(t, l, PromptAssembled, PromptAssembledData{Tokens: tokens, Budget: 100})
}

func TestTrimCountEqualsTheTurnsActuallyDropped(t *testing.T) {
	log := NewLog()
	for i := 0; i < 3; i++ {
		preTurn(t, log, i)
		conversation(t, log, fmt.Sprintf("p%d", i), strings.Repeat("r", 1500), "done")
	}
	events := log.Events()
	_, _, _, per, err := trimToBudget(events, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, p := range per {
		total += len(p) + 1
	}
	// A budget that fits exactly one turn plus its assembly record.
	oneTurn := 0
	for _, p := range per[len(per)/3*2:] {
		oneTurn += len(p) + 1
	}
	from, dropped, _, _, err := trimToBudget(events, oneTurn+len("[]")+10)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 2 {
		t.Fatalf("dropped %d turns, want 2 (of 3, %d bytes total)", dropped, total)
	}
	if got := countTurns(events[from:]); got != 1 {
		t.Fatalf("%d turns survive, want 1", got)
	}
	// The surviving turn keeps the record of how its own prompt was assembled.
	if events[from].Type != PromptAssembled {
		t.Fatalf("the surviving turn starts with %s, want its own %s", events[from].Type, PromptAssembled)
	}
	var data PromptAssembledData
	if err := json.Unmarshal(events[from].Data, &data); err != nil || data.Tokens != 2 {
		t.Fatalf("the surviving turn carries assembly record %+v (err %v), want the third turn's", data, err)
	}
}

// Events a face records before any turn exists are not a turn, whatever they
// are, and dropping them must not be reported as one.
func TestLeadingNonTurnEventsAreNotCountedAsATurn(t *testing.T) {
	log := NewLog()
	mustAppend(t, log, SystemPrompt, SystemPromptData{Text: "sys"})
	for i := 0; i < 3; i++ {
		conversation(t, log, fmt.Sprintf("p%d", i), strings.Repeat("r", 1500), "done")
	}
	events := log.Events()
	_, _, _, per, _ := trimToBudget(events, 1<<30)
	last := 0
	for _, p := range per[len(per)-10:] {
		last += len(p) + 1
	}
	from, dropped, _, _, err := trimToBudget(events, last+len("[]")+10)
	if err != nil {
		t.Fatal(err)
	}
	if got := countTurns(events[from:]); got != 1 || dropped != 2 {
		t.Fatalf("kept %d turns and reported %d dropped, want 1 and 2", got, dropped)
	}
}

// A retention prune that decided a session was stale must not remove a
// generation another run published after it looked.
func TestPruneDoesNotDeleteAGenerationPublishedAfterItLooked(t *testing.T) {
	dir := t.TempDir()
	pruner := NewStore(dir)
	pruner.setLimits(0, 2)

	old := newID(t)
	oldLog := NewLog()
	conversation(t, oldLog, "old", "r", "done")
	if _, err := pruner.Save(old, oldLog); err != nil {
		t.Fatal(err)
	}
	filler := NewLog()
	conversation(t, filler, "filler", "r", "done")
	if _, err := pruner.Save(newID(t), filler); err != nil {
		t.Fatal(err)
	}

	// Another run resumes the oldest session, and publishes its next
	// generation in the window between the pruner listing the directory and
	// acting on the listing.
	resumer := NewStore(dir)
	resumed, _, err := resumer.Load(old)
	if err != nil {
		t.Fatal(err)
	}
	conversation(t, resumed, "again", "r", "done")
	var report SaveReport
	var saveErr error
	pruner.afterList = func() {
		pruner.afterList = nil
		report, saveErr = resumer.Save(old, resumed)
	}

	fresh := NewLog()
	conversation(t, fresh, "new", "r", "done")
	if _, err := pruner.Save(newID(t), fresh); err != nil {
		t.Fatal(err)
	}
	if saveErr != nil {
		t.Fatalf("the concurrent Save failed: %v", saveErr)
	}
	if report.Path == "" {
		t.Fatal("the concurrent Save never ran")
	}
	if _, err := os.Stat(report.Path); err != nil {
		t.Fatalf("Save reported %s saved but a prune removed it: %v", report.Path, err)
	}
	if _, _, err := NewStore(dir).Load(old); err != nil {
		t.Fatalf("the session the resumer saved cannot be loaded: %v", err)
	}
}

// Load picks a generation and then reads it; a concurrent save supersedes and
// removes that generation in between. That is a retry, not an error.
func TestLoadRetriesWhenTheGenerationItPickedIsSuperseded(t *testing.T) {
	dir := t.TempDir()
	writer := NewStore(dir)
	id := newID(t)
	log := NewLog()
	conversation(t, log, "seed", "r", "done")
	if _, err := writer.Save(id, log); err != nil {
		t.Fatal(err)
	}

	reader := NewStore(dir)
	reader.beforeRead = func() {
		reader.beforeRead = nil
		conversation(t, log, "more", "r", "done")
		if _, err := writer.Save(id, log); err != nil {
			t.Errorf("writer: %v", err)
		}
	}
	loaded, report, err := reader.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if report.Generation != 2 || len(loaded.Events()) != len(log.Events()) {
		t.Fatalf("Load returned generation %d with %d events, want the superseding generation 2 with %d",
			report.Generation, len(loaded.Events()), len(log.Events()))
	}
}

// A save finding its chosen generation superseded under it retries rather than
// reporting a raw missing file.
func TestSaveRetriesWhenTheGenerationItPickedIsSuperseded(t *testing.T) {
	dir := t.TempDir()
	first := NewStore(dir)
	id := newID(t)
	log := NewLog()
	conversation(t, log, "seed", "r", "done")
	if _, err := first.Save(id, log); err != nil {
		t.Fatal(err)
	}
	second := NewStore(dir)
	if _, _, err := second.Load(id); err != nil {
		t.Fatal(err)
	}
	first.beforeRead = func() {
		first.beforeRead = nil
		// The generation first picked is gone, and a newer one is in its place.
		conversation(t, log, "other run", "r", "done")
		if _, err := second.Save(id, log); err != nil {
			t.Errorf("second: %v", err)
		}
	}
	conversation(t, log, "mine", "r", "done")
	_, err := first.Save(id, log)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Save error = %v, want a *ConflictError (the other run's newer generation is not this run's)", err)
	}
}
