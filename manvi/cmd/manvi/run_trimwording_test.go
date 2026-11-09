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
// replay (session.StripStaleReasoning). Every surface that reports a trim says so
// in the one wording trimNote builds (ft-9a5710b6 item 8): the resume and save
// messages and the reflection packet.
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
	loaded, report, err := session.NewStore(sessionsDir()).Load(id)
	if err != nil || loaded == nil || report.TrimmedTurns == 0 {
		t.Fatalf("the fixture was not trimmed (%+v, %v), so the test says nothing", report, err)
	}
	want := trimNote(report.TrimmedTurns)
	if !strings.Contains(want, "reasoning from before it is no longer replayed") {
		t.Fatalf("trimNote does not say the reasoning is not replayed: %q", want)
	}
	if got := notes.String(); !strings.Contains(got, want) {
		t.Errorf("save message %q lacks %q", got, want)
	}

	notes.Reset()
	if _, _, err := openSession(&notes, session.NewStore(sessionsDir()), runOptions{resume: id}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := notes.String(); !strings.Contains(got, want) {
		t.Errorf("resume message %q lacks %q", got, want)
	}

	notes.Reset()
	packet, err := reflectionPacket(&notes, session.NewStore(sessionsDir()), id)
	if err != nil {
		t.Fatalf("reflectionPacket: %v", err)
	}
	if !strings.Contains(notes.String(), want) || !strings.Contains(packet, want) {
		t.Errorf("reflection note %q / packet tail lacks %q", notes.String(), want)
	}
	if !strings.Contains(packet, "NOTE: "+want) {
		t.Errorf("the reflector's packet does not carry the note: %q", packet[max(len(packet)-400, 0):])
	}
}
