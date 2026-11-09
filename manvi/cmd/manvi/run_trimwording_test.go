package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// A session trimmed to its size bound loses its oldest turns, and with them the
// reasoning that was signed behind those turns, which the next request does not
// replay (session.StripStaleReasoning). The resume and save messages say so
// (ft-9a5710b6 item 8), so the operator is told about the reasoning and not only
// the history.
func TestTrimMessagesSayReasoningBeforeTheTrimIsNotReplayed(t *testing.T) {
	t.Setenv("MANVI_STATE_DIR", t.TempDir())
	store := session.NewStore(sessionsDir())
	var notes bytes.Buffer
	id, log, err := openSession(&notes, store, runOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Ten ~1 MiB turns overrun the 8 MiB bound, so the save trims the oldest.
	filler := strings.Repeat("x", 1<<20)
	for i := 0; i < 10; i++ {
		if _, err := log.Append(session.TurnStart, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(session.UserMessage, session.MessageData{Message: llm.Message{
			Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: filler}},
		}}); err != nil {
			t.Fatal(err)
		}
	}

	notes.Reset()
	if err := saveSession(&notes, store, id, log); err != nil {
		t.Fatalf("saveSession: %v", err)
	}
	if got := notes.String(); !strings.Contains(got, "oldest turn(s) were dropped") ||
		!strings.Contains(got, "reasoning from before the trim is no longer replayed") {
		t.Errorf("save message does not say reasoning from before the trim is not replayed: %q", got)
	}

	notes.Reset()
	if _, _, err := openSession(&notes, session.NewStore(sessionsDir()), runOptions{resume: id}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := notes.String(); !strings.Contains(got, "oldest turn(s) were dropped") ||
		!strings.Contains(got, "reasoning from before it is no longer replayed") {
		t.Errorf("resume message does not say reasoning from before the trim is not replayed: %q", got)
	}
}
