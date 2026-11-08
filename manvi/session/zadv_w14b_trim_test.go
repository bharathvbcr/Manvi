package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// Second adversarial pass over w14-trim (card ft-d8db16de). Doc citations are to
// the claude-api skill's shared/ directory:
//   - model-migration.md, "Breaking change 3": "What invalidates every later
//     thinking block" (removing a block from anywhere other than the start of the
//     run) and "What keeps later blocks valid" (a leading run, oldest first).
//   - preserved-thinking-migration/causes.md: rows rolling_truncation and
//     tail_kept ("strip the retained turns' thinking as a deterministic, recorded
//     strip ... never mid tool-round") and the keep list ("An assistant turn whose
//     tool_use still awaits its tool_result should keep its thinking").

// leadingRunOnly fails unless the signed blocks msgs carried and out kept form a
// leading run of removals: once one block is kept, no later block was removed
// (model-migration.md, "What keeps later blocks valid": removing a leading run,
// oldest first; causes.md keep list: "the kept blocks must stay an unbroken run").
func leadingRunOnly(t *testing.T, msgs, out []llm.Message) {
	t.Helper()
	if len(msgs) != len(out) {
		t.Fatalf("the strip changed the message count: %d -> %d", len(msgs), len(out))
	}
	keptSeen := false
	for i := range msgs {
		before, after := len(signatures([]llm.Message{msgs[i]})), len(signatures([]llm.Message{out[i]}))
		if before == 0 {
			continue
		}
		if after > 0 {
			keptSeen = true
			continue
		}
		if keptSeen {
			t.Fatalf("message %d lost its signed block behind an earlier kept one: a removal from the middle (model-migration.md, \"What invalidates every later thinking block\")", i)
		}
	}
}

// stripOf is the strip a request makes, with the leading-run check applied.
func stripOf(t *testing.T, l *Log) (kept []string, dropped, before int) {
	t.Helper()
	msgs, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	out, n, err := l.StripStaleReasoning(msgs)
	if err != nil {
		t.Fatal(err)
	}
	leadingRunOnly(t, msgs, out)
	return signatures(out), n, len(signatures(msgs))
}

func tightStore(dir string) *Store {
	s := NewStore(dir)
	s.setLimits(6000, 0)
	return s
}

func carriedThrough(t *testing.T, l *Log) (int, bool) {
	t.Helper()
	for _, e := range l.Events() {
		if e.Type != WireFrame {
			continue
		}
		var f FrameData
		if err := json.Unmarshal(e.Data, &f); err != nil {
			t.Fatal(err)
		}
		if f.Why == WhyCarried {
			return f.Through, true
		}
	}
	return 0, false
}

// Item 1. The newest turn of a saved log can end on an assistant message whose
// tool_use has no result (Save does not look; the loop answers open calls before
// the face persists, loop.go answerOpenCalls, but the store takes any log).
// trimEncoded never drops the newest turn and never splits one, so that turn is
// kept whole, and its thinking was signed behind the dropped turns: replaying it
// is a 400 on an enforced account (model-migration.md, "What invalidates every
// later thinking block"). The keep list's "should keep its thinking" is advice
// for an intact prefix; with the prefix gone the only valid choices are strip or
// drop_block, and the latter drops that turn's blocks too ("including blocks in an
// assistant turn whose tool_use is still waiting on its tool_result",
// model-migration.md, drop_block paragraph). So the strip must take the block and
// must leave the tool_use.
func TestAdvBInFlightToolUseTurnIsKeptWholeAndItsStaleThinkingStripped(t *testing.T) {
	store := tightStore(t.TempDir())
	id := newID(t)
	l := NewLog()
	frameOpened(t, l)
	for _, label := range []string{"one", "two", "three", "four", "five"} {
		trimTurn(t, l, label)
	}
	mustAppend(t, l, TurnStart, nil)
	mustAppend(t, l, UserMessage, MessageData{Message: user("live" + strings.Repeat("y", 1500))})
	mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "plan", Signature: "sig-inflight"},
			llm.ToolCallBlock{ID: "c-live", Name: "echo", Arguments: json.RawMessage(`{}`)},
		}}})
	last := l.Events()[l.Len()-1].Seq

	report, err := store.Save(id, l)
	if err != nil || report.TrimmedTurns == 0 {
		t.Fatalf("save: %v, trimmed %d", err, report.TrimmedTurns)
	}
	loaded, _, err := NewStore(store.Dir()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	// The newest turn survived whole: its prompt and its call are in the file.
	msgs, err := loaded.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	tail := msgs[len(msgs)-1]
	calls := 0
	for _, b := range tail.Content {
		if _, ok := b.(llm.ToolCallBlock); ok {
			calls++
		}
	}
	if tail.Role != llm.RoleAssistant || calls != 1 {
		t.Fatalf("the in-flight turn did not survive whole: tail %+v", tail)
	}
	if through, ok := carriedThrough(t, loaded); !ok || through != last {
		t.Fatalf("carried Through = %d (found %v), want the in-flight event %d", through, ok, last)
	}
	out, n, err := loaded.StripStaleReasoning(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if got := signatures(out); len(got) != 0 || n != len(signatures(msgs)) {
		t.Fatalf("kept %v, stripped %d of %d; the in-flight block was signed behind turns the file lost", got, n, len(signatures(msgs)))
	}
	var after int
	for _, b := range out[len(out)-1].Content {
		if _, ok := b.(llm.ToolCallBlock); ok {
			after++
		}
	}
	if after != 1 {
		t.Fatalf("the strip removed the tool_use of the in-flight turn: %+v", out[len(out)-1])
	}

	// Control: with no trim the same turn keeps its thinking.
	roomy := NewStore(t.TempDir())
	if _, err := roomy.Save(id, l); err != nil {
		t.Fatal(err)
	}
	whole, _, err := NewStore(roomy.Dir()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if kept, _, _ := stripOf(t, whole); len(kept) == 0 || kept[len(kept)-1] != "sig-inflight" {
		t.Fatalf("an untrimmed log lost the in-flight turn's thinking: kept %v", kept)
	}
}

// trimmedStoreCycle loads the session through a store of the given budget,
// appends the given turns, saves, and returns the log as a fresh process reads
// it. An empty labels is a resume that adds nothing.
func trimmedStoreCycle(t *testing.T, dir, id string, maxBytes int, labels ...string) *Log {
	t.Helper()
	s := NewStore(dir)
	if maxBytes > 0 {
		s.setLimits(maxBytes, 0)
	}
	log, _, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range labels {
		trimTurn(t, log, label)
	}
	if _, err := s.Save(id, log); err != nil {
		t.Fatal(err)
	}
	fresh, _, err := NewStore(dir).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

// Item 2. Blocks produced after a trim are valid until the next trim, which
// stales them together with everything older. Across three resumes (the first
// two without a trim, a trim, then one without) Through only moves forward,
// the strip is always a leading run, and a stale block never survives behind a
// kept one.
func TestAdvBThroughIsMonotonicAndTheStripIsALeadingRunAcrossResumes(t *testing.T) {
	dir := t.TempDir()
	id := newID(t)
	store := tightStore(dir)
	l := NewLog()
	frameOpened(t, l)
	for _, label := range []string{"one", "two", "three", "four", "five"} {
		trimTurn(t, l, label)
	}
	if r, err := store.Save(id, l); err != nil || r.TrimmedTurns == 0 {
		t.Fatalf("first save: %v, trimmed %d", err, r.TrimmedTurns)
	}
	log := trimmedStoreCycle(t, dir, id, 0) // resume 0: load, save, nothing new
	t1, ok := carriedThrough(t, log)
	if !ok || t1 == 0 {
		t.Fatal("the first trim recorded no Through")
	}
	if kept, _, _ := stripOf(t, log); len(kept) != 0 {
		t.Fatalf("after the first trim kept %v", kept)
	}

	// Resume 1, no trim: a produced block is valid, and survives two more
	// load/save cycles that trim nothing.
	log = trimmedStoreCycle(t, dir, id, 0, "r1")
	for i := 0; i < 2; i++ {
		log = trimmedStoreCycle(t, dir, id, 0)
	}
	if got, _ := carriedThrough(t, log); got != t1 {
		t.Fatalf("Through moved from %d to %d across saves that trimmed nothing", t1, got)
	}
	if kept, _, _ := stripOf(t, log); !reflect.DeepEqual(kept, []string{"sig-r1"}) {
		t.Fatalf("after resume 1 kept %v, want [sig-r1]", kept)
	}

	// Resume 2 trims: r1 and r2 are stale, and Through advances.
	log = trimmedStoreCycle(t, dir, id, 6000, "r2a", "r2b", "r2c", "r2d")
	t2, ok := carriedThrough(t, log)
	if !ok || t2 <= t1 {
		t.Fatalf("second trim Through = %d (found %v), want more than %d", t2, ok, t1)
	}
	if kept, _, before := stripOf(t, log); len(kept) != 0 || before == 0 {
		t.Fatalf("after the second trim kept %v of %d signed", kept, before)
	}

	// Resume 3, no trim: only what it produces is valid; the log stays a leading run
	// while it grows behind the second Through.
	log = trimmedStoreCycle(t, dir, id, 0, "r3a", "r3b")
	if got, _ := carriedThrough(t, log); got != t2 {
		t.Fatalf("Through = %d, want the second trim's %d", got, t2)
	}
	if kept, _, _ := stripOf(t, log); !reflect.DeepEqual(kept, []string{"sig-r3a", "sig-r3b"}) {
		t.Fatalf("after resume 3 kept %v, want [sig-r3a sig-r3b]", kept)
	}

	// A third trim of a log that already holds a carried frame: the old frame is in
	// the dropped front, the new Through is later still.
	log = trimmedStoreCycle(t, dir, id, 6000, "r4a", "r4b", "r4c", "r4d")
	t3, ok := carriedThrough(t, log)
	if !ok || t3 <= t2 {
		t.Fatalf("third trim Through = %d (found %v), want more than %d", t3, ok, t2)
	}
	frames := 0
	for _, e := range log.Events() {
		if e.Type != WireFrame {
			continue
		}
		var f FrameData
		if err := json.Unmarshal(e.Data, &f); err != nil {
			t.Fatal(err)
		}
		if f.Why == WhyCarried {
			frames++
		}
	}
	if frames != 1 {
		t.Fatalf("the file holds %d carried frames, want exactly one (the latest trim record)", frames)
	}
	if kept, _, _ := stripOf(t, log); len(kept) != 0 {
		t.Fatalf("after the third trim kept %v", kept)
	}
}

// Item 3. A trimmed file with no frame has no Through, so Load treats every
// event it holds as stale. Over-stripping is a leading run, so it is valid
// (model-migration.md, "What keeps later blocks valid"), but it has a price: the
// reasoning a resumed run produced is lost at the next reload, and again at every
// reload, until something trims. This test pins the behaviour: the next save
// writes no Through (the file has no frame to restate), the header keeps
// trimmed_turns, and no stale block can survive.
func TestAdvBFramelessTrimmedFileIsFullyStaleOnEveryLoad(t *testing.T) {
	dir := t.TempDir()
	id := newID(t)
	store := tightStore(dir)
	l := NewLog()
	for _, label := range []string{"one", "two", "three", "four", "five"} {
		trimTurn(t, l, label)
	}
	if r, err := store.Save(id, l); err != nil || r.TrimmedTurns == 0 {
		t.Fatalf("save: %v, trimmed %d", err, r.TrimmedTurns)
	}
	for cycle := 0; cycle < 3; cycle++ {
		label := fmt.Sprintf("after%d", cycle)
		s := NewStore(dir)
		log, lr, err := s.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if lr.TrimmedTurns == 0 {
			t.Fatalf("cycle %d: the header lost trimmed_turns", cycle)
		}
		// In the process that resumed, the block it produces is valid and replayed.
		trimTurn(t, log, label)
		if kept, _, _ := stripOf(t, log); !reflect.DeepEqual(kept, []string{"sig-" + label}) {
			t.Fatalf("cycle %d: in the resuming process kept %v, want only sig-%s", cycle, kept, label)
		}
		if _, err := s.Save(id, log); err != nil {
			t.Fatal(err)
		}
		reloaded, _, err := NewStore(dir).Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := carriedThrough(t, reloaded); ok {
			t.Fatalf("cycle %d: a frame-less file grew a carried frame without a trim", cycle)
		}
		// The price: the reasoning produced after the resume is gone after the reload.
		if kept, _, before := stripOf(t, reloaded); len(kept) != 0 || before == 0 {
			t.Fatalf("cycle %d: after reload kept %v of %d signed blocks; a stale block must never survive a frame-less trim", cycle, kept, before)
		}
	}
}

// A frame that appears after the trim (the loop's settleFrame on a legacy log)
// has Through zero, so it does not rescue the file: the fallback still reads
// trimmed_turns and treats every block as stale. Nothing stale can be sent, but
// a frame-less trim is permanent until the next trim writes a carried frame.
func TestAdvBFrameAddedAfterAFramelessTrimDoesNotMakeLaterBlocksValid(t *testing.T) {
	dir := t.TempDir()
	id := newID(t)
	store := tightStore(dir)
	l := NewLog()
	for _, label := range []string{"one", "two", "three", "four", "five"} {
		trimTurn(t, l, label)
	}
	if _, err := store.Save(id, l); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	log, _, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	frameOpened(t, log)
	trimTurn(t, log, "after")
	if _, err := s.Save(id, log); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := NewStore(dir).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	kept, _, before := stripOf(t, reloaded)
	if len(kept) != 0 {
		t.Fatalf("kept %v; only the loss of reasoning is acceptable here, and nothing stale may be sent", kept)
	}
	t.Logf("frame added after a frame-less trim: %d signed blocks, all stripped on reload (the cost of item 3)", before)
}

// Item 4. Load returns exactly the events the file holds, byte for byte, for a
// framed and a frame-less trimmed log, and a cached store writes what a fresh
// store writes for a log that carries signed reasoning and a wire frame.
func TestAdvBLoadReturnsExactlyTheFilesEventsAndCachedEqualsFresh(t *testing.T) {
	for _, framed := range []bool{true, false} {
		for _, maxBytes := range []int{DefaultMaxBytes, 6000, 9000} {
			name := fmt.Sprintf("framed=%v/max=%d", framed, maxBytes)
			cached := NewStore(t.TempDir())
			cached.setLimits(maxBytes, 0)
			id := newID(t)
			log := NewLog()
			if framed {
				frameOpened(t, log)
			}
			for i := 0; i < 14; i++ {
				trimTurn(t, log, fmt.Sprintf("t%02d", i))
				if i%3 != 0 {
					continue
				}
				if _, err := cached.Save(id, log); err != nil {
					t.Fatalf("%s turn %d: %v", name, i, err)
				}
				loaded, lr, err := cached.Load(id)
				if err != nil {
					t.Fatalf("%s turn %d: %v", name, i, err)
				}
				fresh := NewStore(t.TempDir())
				fresh.setLimits(maxBytes, 0)
				if _, err := fresh.Save(id, log); err != nil {
					t.Fatal(err)
				}
				want, wr, err := fresh.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(loaded.Events(), want.Events()) {
					t.Fatalf("%s turn %d: a cached store wrote different events than a fresh one", name, i)
				}
				if lr.Events != wr.Events {
					t.Fatalf("%s turn %d: report %+v differs from %+v", name, i, lr, wr)
				}
				// Load returns what the file holds: the events array of the file, re-encoded.
				raw, err := readFileOf(t, cached, id)
				if err != nil {
					t.Fatal(err)
				}
				var doc struct {
					Events []json.RawMessage `json:"events"`
				}
				if err := json.Unmarshal(raw, &doc); err != nil {
					t.Fatal(err)
				}
				got := loaded.Events()
				if len(got) != len(doc.Events) {
					t.Fatalf("%s turn %d: Load returned %d events, the file holds %d", name, i, len(got), len(doc.Events))
				}
				for k, e := range got {
					enc, err := json.Marshal(e)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(enc, doc.Events[k]) {
						t.Fatalf("%s turn %d: event %d is not what the file holds:\n%s\n%s", name, i, k, enc, doc.Events[k])
					}
				}
			}
		}
	}
}

// Item 4. Through survives any number of saves and loads that trim nothing, and
// the carried frame is neither duplicated nor rewritten.
func TestAdvBThroughSurvivesSavesThatTrimNothing(t *testing.T) {
	dir := t.TempDir()
	id := newID(t)
	store := tightStore(dir)
	l := NewLog()
	frameOpened(t, l)
	for _, label := range []string{"one", "two", "three", "four", "five"} {
		trimTurn(t, l, label)
	}
	if _, err := store.Save(id, l); err != nil {
		t.Fatal(err)
	}
	first, _, err := NewStore(dir).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	want, ok := carriedThrough(t, first)
	if !ok {
		t.Fatal("no carried frame")
	}
	prev := first.Events()[0]
	for i := 0; i < 5; i++ {
		log := trimmedStoreCycle(t, dir, id, 0)
		got, ok := carriedThrough(t, log)
		if !ok || got != want {
			t.Fatalf("cycle %d: Through = %d (found %v), want %d", i, got, ok, want)
		}
		if cur := log.Events()[0]; !reflect.DeepEqual(cur, prev) {
			t.Fatalf("cycle %d: the carried frame was rewritten by a save that trimmed nothing", i)
		}
	}
}

func readFileOf(t *testing.T, s *Store, id string) ([]byte, error) {
	t.Helper()
	_, path, raw, err := func() (int, string, []byte, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.readCurrent(id)
	}()
	_ = path
	return raw, err
}

// A frame-less log whose trim drops only the leading group (the events ahead of
// the first TurnStart, which trimEncoded does not count as a turn) writes
// trimmed_turns 0, so Load's fallback has nothing to read. If that group held a
// message, every block kept was signed behind it. The strip must still take them.
func TestAdvBFramelessTrimOfOnlyTheLeadingGroupStillStripsWhatItKept(t *testing.T) {
	store := tightStore(t.TempDir())
	id := newID(t)
	l := NewLog()
	mustAppend(t, l, UserMessage, MessageData{Message: user("lead" + strings.Repeat("z", 3000))})
	mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "t", Signature: "sig-lead"}, llm.TextBlock{Text: "ok"}}}})
	for _, label := range []string{"a", "b"} {
		trimTurn(t, l, label)
	}
	report, err := store.Save(id, l)
	if err != nil {
		t.Fatal(err)
	}
	loaded, lr, err := NewStore(store.Dir()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := loaded.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) >= 6 {
		t.Skipf("the leading messages were kept (report %+v), so the fixture says nothing", report)
	}
	if kept, _, before := stripOf(t, loaded); before == 0 || len(kept) != 0 {
		t.Fatalf("the leading message was dropped (header trimmed_turns %d) but %d signed block(s) remain, kept %v", lr.TrimmedTurns, before, kept)
	}
}
