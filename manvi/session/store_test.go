package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"manvi/llm"
)

// conversation records one complete turn: prompt, tool call, result, answer.
func conversation(t *testing.T, l *Log, prompt, toolText, answer string) {
	t.Helper()
	mustAppend(t, l, TurnStart, nil)
	mustAppend(t, l, SystemPrompt, SystemPromptData{Text: "you are a builder"})
	mustAppend(t, l, UserMessage, MessageData{Message: user(prompt)})
	mustAppend(t, l, StepStart, nil)
	mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.ToolCallBlock{ID: llm.CallID(prompt), Name: "read", Arguments: json.RawMessage(`{"p":"a<b>&c"}`)},
		},
		Provenance: &llm.AssistantProvenance{Provider: "replay", Model: "m1"},
	}})
	mustAppend(t, l, ToolCall, ToolCallData{ID: llm.CallID(prompt), Name: "read", Arguments: json.RawMessage(`{"p":"a"}`)})
	mustAppend(t, l, ToolResult, ToolResultData{ToolCallID: llm.CallID(prompt), Text: toolText})
	mustAppend(t, l, StepEnd, nil)
	mustAppend(t, l, AssistantMessage, MessageData{Message: assistant(answer)})
	mustAppend(t, l, TurnEnd, nil)
}

func newID(t *testing.T) string {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	return id
}

func derived(t *testing.T, l *Log) string {
	t.Helper()
	msgs, err := l.DeriveMessages()
	if err != nil {
		t.Fatalf("DeriveMessages: %v", err)
	}
	encoded, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	return string(encoded)
}

func TestRoundTripRestoresTheDerivedHistoryExactly(t *testing.T) {
	store := NewStore(t.TempDir())
	id := newID(t)

	original := NewLog()
	conversation(t, original, "first", "the file says hello", "done")
	conversation(t, original, "second", "and it still does", "done again")
	want := derived(t, original)

	if _, err := store.Save(id, original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	restored, report, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if report.Events != original.Len() {
		t.Fatalf("restored %d events, saved %d", report.Events, original.Len())
	}
	if report.Turns != 2 {
		t.Errorf("report.Turns = %d, want 2", report.Turns)
	}
	// The point of the whole exercise: a resumed run must build its request
	// from the same projection the original run did, not from a second
	// reconstruction that happens to look similar.
	if got := derived(t, restored); got != want {
		t.Fatalf("restored history differs from the original projection\n got: %s\nwant: %s", got, want)
	}
}

func TestResumedLogContinuesSequenceAndTurnNumbering(t *testing.T) {
	store := NewStore(t.TempDir())
	id := newID(t)

	original := NewLog()
	conversation(t, original, "first", "result", "answer")
	lastSeq := original.Events()[original.Len()-1].Seq
	if _, err := store.Save(id, original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	restored, _, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	event, err := restored.Append(TurnStart, nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if event.Seq != lastSeq+1 {
		t.Errorf("next sequence = %d, want %d — a resumed session must not reissue sequence numbers",
			event.Seq, lastSeq+1)
	}
	if event.Turn != 2 {
		t.Errorf("next turn = %d, want 2", event.Turn)
	}
}

// A session whose oldest turns were dropped by the retention bound no longer
// starts at sequence 1. Numbering that restarted from the event count would
// hand two different events in one session's history the same id, and the
// evidence trail names events by that id.
func TestResumedTrimmedLogDoesNotReissueSequenceNumbers(t *testing.T) {
	restored, err := RestoreLog([]Event{
		{Seq: 40, Type: TurnStart, Turn: 3},
		{Seq: 41, Type: UserMessage, Turn: 3, Data: mustJSON(t, MessageData{Message: user("carry on")})},
		{Seq: 42, Type: TurnEnd, Turn: 3},
	})
	if err != nil {
		t.Fatalf("RestoreLog: %v", err)
	}
	event, err := restored.Append(TurnStart, nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if event.Seq != 43 {
		t.Errorf("next sequence = %d, want 43", event.Seq)
	}
	if event.Turn != 4 {
		t.Errorf("next turn = %d, want 4", event.Turn)
	}

	seen := map[int]bool{}
	for _, e := range restored.Events() {
		if seen[e.Seq] {
			t.Fatalf("sequence %d was issued twice", e.Seq)
		}
		seen[e.Seq] = true
	}
}

func mustJSON(t *testing.T, payload any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestSaveRefusesAnEmptyLog(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Save(newID(t), NewLog()); err == nil {
		t.Fatal("saving a log with no events should fail: an empty file is what Load calls corruption")
	}
}

func TestLoadRejectsACorruptFile(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	id := newID(t)

	log := NewLog()
	conversation(t, log, "first", "the file says hello", "done")
	report, err := store.Save(id, log)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A byte flipped inside an event: still valid JSON, still a well-formed
	// session, and no longer what was written.
	raw, err := os.ReadFile(report.Path)
	if err != nil {
		t.Fatal(err)
	}
	altered := strings.Replace(string(raw), "the file says hello", "the file says hellp", 1)
	if altered == string(raw) {
		t.Fatal("test setup: nothing was altered")
	}
	if err := os.WriteFile(report.Path, []byte(altered), 0o644); err != nil {
		t.Fatal(err)
	}

	fresh := NewStore(dir)
	restored, _, err := fresh.Load(id)
	if err == nil {
		t.Fatal("a tampered session file must not load")
	}
	var corrupt *CorruptError
	if !errors.As(err, &corrupt) {
		t.Fatalf("error = %v, want a CorruptError", err)
	}
	if restored != nil {
		t.Fatal("a corrupt session must not yield a log — resuming into one looks like a fresh start")
	}
}

func TestLoadRejectsATruncatedFile(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	id := newID(t)

	log := NewLog()
	conversation(t, log, "first", strings.Repeat("x", 4096), "done")
	report, err := store.Save(id, log)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(report.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report.Path, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatal(err)
	}

	fresh := NewStore(dir)
	if _, _, err := fresh.Load(id); err == nil {
		t.Fatal("a half-written session file must not load")
	} else if corrupt := (*CorruptError)(nil); !errors.As(err, &corrupt) {
		t.Fatalf("error = %v, want a CorruptError", err)
	}
}

func TestLoadRejectsAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	id := newID(t)

	log := NewLog()
	conversation(t, log, "first", "result", "done")
	report, err := store.Save(id, log)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(report.Path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	fresh := NewStore(dir)
	log2, _, err := fresh.Load(id)
	if err == nil {
		t.Fatal("a zero-byte session file must be reported, not read as an empty session")
	}
	if log2 != nil {
		t.Fatal("no log may be returned for an unreadable file")
	}
}

func TestLoadRejectsAFileFiledUnderTheWrongID(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	id := newID(t)
	log := NewLog()
	conversation(t, log, "first", "result", "done")
	if _, err := store.Save(id, log); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, name(id, 1)))
	if err != nil {
		t.Fatal(err)
	}
	other := newID(t)
	if err := os.WriteFile(filepath.Join(dir, name(other, 1)), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewStore(dir).Load(other); err == nil {
		t.Fatal("a session copied under another id must not load as that session")
	}
}

func TestResolveAmbiguousPrefixNamesTheCandidates(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// Ids are written directly so the shared prefix is deterministic.
	for _, id := range []string{"abc1111111111111", "abc2222222222222", "def3333333333333"} {
		log := NewLog()
		conversation(t, log, "p"+id, "result", "done")
		if _, err := store.Save(id, log); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}

	if _, err := store.Resolve("abc"); err == nil {
		t.Fatal("an ambiguous prefix must not resolve to an arbitrary session")
	} else {
		var ambiguous *AmbiguousError
		if !errors.As(err, &ambiguous) {
			t.Fatalf("error = %v, want an AmbiguousError", err)
		}
		if len(ambiguous.Candidates) != 2 {
			t.Fatalf("candidates = %v, want both abc sessions", ambiguous.Candidates)
		}
		for _, want := range []string{"abc1111111111111", "abc2222222222222"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name candidate %s", err.Error(), want)
			}
		}
	}

	got, err := store.Resolve("def")
	if err != nil {
		t.Fatalf("unique prefix should resolve: %v", err)
	}
	if got != "def3333333333333" {
		t.Errorf("resolved %q", got)
	}
	if got, err := store.Resolve("abc1111111111111"); err != nil || got != "abc1111111111111" {
		t.Errorf("a full id must resolve even when it is a prefix of nothing else: %q, %v", got, err)
	}
}

func TestResolveAndLatestOnAnEmptyDirectory(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "sessions"))

	if _, err := store.Latest(); !errors.Is(err, ErrNoSessions) {
		t.Fatalf("Latest on an empty store = %v, want ErrNoSessions", err)
	}
	if _, err := store.Resolve("anything"); err == nil {
		t.Fatal("resolving in an empty store must fail")
	} else if missing := (*NotFoundError)(nil); !errors.As(err, &missing) {
		t.Fatalf("error = %v, want a NotFoundError", err)
	}
	if _, _, err := store.Load("0123456789abcdef"); err == nil {
		t.Fatal("loading a session that does not exist must fail")
	}
	sessions, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("List = %v, want none", sessions)
	}
}

func TestLatestNamesTheMostRecentlyWrittenSession(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	first, second := "aaaa000000000000", "bbbb000000000000"
	for _, id := range []string{first, second} {
		log := NewLog()
		conversation(t, log, "p"+id, "result", "done")
		if _, err := store.Save(id, log); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	latest, err := store.Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest != second {
		t.Errorf("Latest = %s, want %s", latest, second)
	}
}

func TestASecondRunCannotSilentlyOverwriteASession(t *testing.T) {
	dir := t.TempDir()
	id := newID(t)

	seed := NewLog()
	conversation(t, seed, "first", "result", "done")
	if _, err := NewStore(dir).Save(id, seed); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Two runs resume the same session, as two CI jobs pointed at one state
	// directory would.
	runA, runB := NewStore(dir), NewStore(dir)
	logA, _, err := runA.Load(id)
	if err != nil {
		t.Fatalf("run A Load: %v", err)
	}
	logB, _, err := runB.Load(id)
	if err != nil {
		t.Fatalf("run B Load: %v", err)
	}

	conversation(t, logA, "from A", "result", "done")
	if _, err := runA.Save(id, logA); err != nil {
		t.Fatalf("run A Save: %v", err)
	}

	conversation(t, logB, "from B", "result", "done")
	if _, err := runB.Save(id, logB); err == nil {
		t.Fatal("the second run overwrote the first run's turn without noticing")
	} else {
		var conflict *ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("error = %v, want a ConflictError", err)
		}
	}

	// A's turn is still what the session holds.
	restored, _, err := NewStore(dir).Load(id)
	if err != nil {
		t.Fatalf("Load after the conflict: %v", err)
	}
	if !strings.Contains(derived(t, restored), "from A") {
		t.Error("the surviving session is not the one that saved first")
	}
	if strings.Contains(derived(t, restored), "from B") {
		t.Error("the refused save landed anyway")
	}
}

func TestPublishingAGenerationTwiceIsRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, name("abcdef0123456789", 1))

	if err := writeNew(dir, target, []byte("{}\n")); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	err := writeNew(dir, target, []byte("{}\n"))
	if err == nil {
		t.Fatal("publishing over an existing generation must fail rather than replace it")
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want a ConflictError", err)
	}
}

func TestConcurrentSavesDoNotInterleave(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	id := newID(t)

	log := NewLog()
	conversation(t, log, "seed", "result", "done")
	if _, err := store.Save(id, log); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Save reads the log under its own lock; concurrent savers of one
			// session in one process must serialise rather than produce a file
			// that parses as neither.
			if _, err := store.Save(id, log); err != nil {
				t.Errorf("concurrent Save: %v", err)
			}
		}()
	}
	wg.Wait()

	restored, _, err := NewStore(dir).Load(id)
	if err != nil {
		t.Fatalf("Load after concurrent saves: %v", err)
	}
	if restored.Len() != log.Len() {
		t.Errorf("restored %d events, want %d", restored.Len(), log.Len())
	}
}

func TestACrashedRunLeavesTheSessionResumable(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	id := newID(t)

	log := NewLog()
	conversation(t, log, "first", "result", "done")
	if _, err := store.Save(id, log); err != nil {
		t.Fatalf("Save: %v", err)
	}
	want := derived(t, log)

	// What a killed process leaves behind: a temporary file holding a partial
	// document, and nothing else. It must not be mistaken for the session.
	partial, err := os.CreateTemp(dir, ".session-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := partial.WriteString(`{"manvi_session":1,"id":"` + id + `","events":[{"seq":1`); err != nil {
		t.Fatal(err)
	}
	if err := partial.Close(); err != nil {
		t.Fatal(err)
	}

	restored, _, err := NewStore(dir).Load(id)
	if err != nil {
		t.Fatalf("a crashed run poisoned the next resume: %v", err)
	}
	if got := derived(t, restored); got != want {
		t.Errorf("resumed history changed after a crash\n got: %s\nwant: %s", got, want)
	}

	sessions, err := NewStore(dir).List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("List = %v, want only the one real session", sessions)
	}
}

func TestSessionFileIsBoundedByDroppingWholeTurns(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	store.SetLimits(6000, 0)
	id := newID(t)

	log := NewLog()
	for _, prompt := range []string{"one", "two", "three", "four", "five"} {
		conversation(t, log, prompt, strings.Repeat("y", 1500), "done")
	}

	report, err := store.Save(id, log)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if report.TrimmedTurns == 0 {
		t.Fatal("an oversized session was saved whole; the bound did nothing")
	}
	if report.Oversize {
		t.Error("the surviving turns still exceed the bound")
	}
	if report.Events >= log.Len() {
		t.Errorf("kept %d of %d events; nothing was dropped", report.Events, log.Len())
	}

	info, err := os.Stat(report.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 6000+2048 {
		t.Errorf("file is %d bytes against a %d-byte bound", info.Size(), 6000)
	}

	// What survives must still be a conversation: whole turns, beginning at a
	// user message rather than partway through a tool exchange.
	restored, loaded, err := NewStore(dir).Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.TrimmedTurns != report.TrimmedTurns {
		t.Errorf("the trim was not recorded: header says %d, save reported %d",
			loaded.TrimmedTurns, report.TrimmedTurns)
	}
	msgs, err := restored.DeriveMessages()
	if err != nil {
		t.Fatalf("DeriveMessages after trimming: %v", err)
	}
	if len(msgs) == 0 || msgs[0].Role != llm.RoleUser {
		t.Fatalf("a trimmed session does not begin at a user message: %+v", msgs)
	}
	if !strings.Contains(derived(t, restored), "five") {
		t.Error("the most recent turn was dropped; a bound must never discard the turn being resumed")
	}
}

func TestASingleOversizedTurnIsKeptAndFlagged(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	store.SetLimits(512, 0)
	id := newID(t)

	log := NewLog()
	conversation(t, log, "only", strings.Repeat("z", 4096), "done")

	report, err := store.Save(id, log)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !report.Oversize {
		t.Fatal("a turn that cannot be trimmed below the bound must say so")
	}
	restored, loaded, err := NewStore(dir).Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.Oversize {
		t.Error("the header did not carry the oversize flag")
	}
	if restored.Len() != log.Len() {
		t.Error("the only turn was partly dropped")
	}
}

func TestTheDirectoryKeepsABoundedNumberOfSessions(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	store.SetLimits(0, 3)

	var ids []string
	for i := 0; i < 6; i++ {
		id := newID(t)
		ids = append(ids, id)
		log := NewLog()
		conversation(t, log, "prompt", "result", "done")
		if _, err := store.Save(id, log); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	sessions, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) > 3 {
		t.Fatalf("kept %d sessions against a bound of 3", len(sessions))
	}
	if _, _, err := store.Load(ids[0]); err == nil {
		t.Error("the oldest session survived the retention bound")
	}
	if _, _, err := store.Load(ids[len(ids)-1]); err != nil {
		t.Errorf("the newest session was pruned: %v", err)
	}
}

func TestSupersededGenerationsAreRemoved(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	id := newID(t)

	log := NewLog()
	conversation(t, log, "first", "result", "done")
	for i := 0; i < 4; i++ {
		if _, err := store.Save(id, log); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	kept := 0
	for _, entry := range entries {
		if _, _, ok := parseName(entry.Name()); ok {
			kept++
		}
	}
	if kept != 1 {
		t.Errorf("%d generations survive; only the current one should", kept)
	}
}

// The header is an allowlist on purpose. Nothing about the process — its
// environment, its resolved credentials, the lease token it holds — belongs in
// a durable file, and the way that stays true is a test that fails when a field
// is added rather than a comment asking future callers not to.
func TestPersistedFileCarriesOnlyEventsAndSessionMetadata(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	id := newID(t)

	log := NewLog()
	conversation(t, log, "first", "result", "done")
	report, err := store.Save(id, log)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(report.Path)
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	allowed := map[string]bool{
		"manvi_session": true, "id": true, "created_at": true, "updated_at": true,
		"turns": true, "trimmed_turns": true, "oversize": true,
		"checksum": true, "events": true,
	}
	for key := range doc {
		if !allowed[key] {
			t.Errorf("the session file carries an unexpected field %q; "+
				"a durable record must hold the log and nothing else", key)
		}
	}

	// The events themselves are the log, and the lease token is deliberately
	// kept out of every model-visible payload upstream. Nothing here may
	// reintroduce it.
	for _, forbidden := range []string{"lease_token", "api_key", "authorization", "\"token\""} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Errorf("the session file contains %q", forbidden)
		}
	}
}

func TestRestoreLogRejectsBrokenEventStreams(t *testing.T) {
	cases := []struct {
		name   string
		events []Event
	}{
		{"repeated sequence", []Event{{Seq: 1, Type: TurnStart}, {Seq: 1, Type: TurnEnd}}},
		{"sequence goes backwards", []Event{{Seq: 2, Type: TurnStart}, {Seq: 1, Type: TurnEnd}}},
		{"zero sequence", []Event{{Seq: 0, Type: TurnStart}}},
		{"no type", []Event{{Seq: 1}}},
		{"turn goes backwards", []Event{{Seq: 1, Type: TurnStart, Turn: 2}, {Seq: 2, Type: TurnEnd, Turn: 1}}},
		{"unprojectable message", []Event{
			{Seq: 1, Type: UserMessage, Turn: 1, Data: json.RawMessage(`{"message":"not a message"}`)},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RestoreLog(tc.events); err == nil {
				t.Fatal("a broken event stream must be refused, not resumed")
			}
		})
	}
}

func TestSaveRejectsAnInvalidID(t *testing.T) {
	store := NewStore(t.TempDir())
	log := NewLog()
	conversation(t, log, "first", "result", "done")
	for _, id := range []string{"", "../escape", "NOTHEX", "abc.def"} {
		if _, err := store.Save(id, log); err == nil {
			t.Errorf("Save accepted the id %q", id)
		}
	}
}
