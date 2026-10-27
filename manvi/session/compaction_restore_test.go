package session

import (
	"encoding/json"
	"testing"
)

// journalWithCompaction is a live log's events plus one hand-written compaction
// record, the way an old or damaged journal can carry one that Append would
// have refused.
func journalWithCompaction(t *testing.T, record map[string]any, build func(l *Log) Event) []Event {
	t.Helper()
	l := NewLog()
	appendEvent(t, l, UserMessage, MessageData{Message: user("go")})
	appendEvent(t, l, AssistantMessage, MessageData{Message: callMessage("c1")})
	result := build(l)
	if record["result_seq"] == nil {
		record["result_seq"] = result.Seq
	}
	events := l.Events()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	return append(events, Event{Seq: last.Seq + 1, Type: ToolResultCompacted, Turn: last.Turn, Data: data})
}

func putResult(l *Log) Event {
	ev, err := l.Append(ToolResult, ToolResultData{ToolCallID: "c1", Text: "BODY"})
	if err != nil {
		panic(err)
	}
	return ev
}

// Append refuses a compaction that names a result the log does not hold; a
// restore has to stay lenient, because an old journal can carry one (retention
// trims a result off the front and leaves its compaction behind). The record is
// ignored by the projection, whatever it names, and counted.
func TestRestoreIgnoresACompactionThatNamesNoResultAndCountsIt(t *testing.T) {
	for name, record := range map[string]map[string]any{
		"a sequence that is not in the log":  {"tool_call_id": "c1", "result_seq": 999, "text": "SHORT"},
		"a result that answers another call": {"tool_call_id": "other", "text": "SHORT"},
	} {
		t.Run(name, func(t *testing.T) {
			l, err := RestoreLog(journalWithCompaction(t, record, putResult))
			if err != nil {
				t.Fatalf("restore refused the journal: %v", err)
			}
			msgs, err := l.DeriveMessages()
			if err != nil {
				t.Fatal(err)
			}
			if texts := toolResultTexts(t, msgs); len(texts) != 1 || texts[0] != "BODY" {
				t.Fatalf("a compaction that names no result of its call changed the history: %v", texts)
			}
			if n, err := l.IgnoredCompactions(); err != nil || n != 1 {
				t.Fatalf("IgnoredCompactions = %d, %v; want 1", n, err)
			}
			if keys, _ := l.CompactedResults(); len(keys) != 0 {
				t.Fatalf("CompactedResults = %v, want none", keys)
			}
		})
	}
}

// A record that does name its result is not counted.
func TestARecordThatNamesItsResultIsNotIgnored(t *testing.T) {
	l, err := RestoreLog(journalWithCompaction(t, map[string]any{"tool_call_id": "c1", "text": "SHORT"}, putResult))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := l.IgnoredCompactions(); n != 0 {
		t.Fatalf("IgnoredCompactions = %d for a record that names its result", n)
	}
	msgs, _ := l.DeriveMessages()
	if texts := toolResultTexts(t, msgs); texts[0] != "SHORT" {
		t.Fatalf("the record was not applied: %v", texts)
	}
}

// The store reports the count when it loads such a file.
func TestLoadReportsIgnoredCompactions(t *testing.T) {
	l, err := RestoreLog(journalWithCompaction(t, map[string]any{"tool_call_id": "c1", "result_seq": 999, "text": "SHORT"}, putResult))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(t.TempDir())
	if _, err := store.Save("abc123", l); err != nil {
		t.Fatal(err)
	}
	_, report, err := store.Load("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if report.IgnoredCompactions != 1 {
		t.Fatalf("LoadReport.IgnoredCompactions = %d, want 1", report.IgnoredCompactions)
	}
}
