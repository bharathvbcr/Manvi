package session

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// Adversarial tests for lane F (ft-9a5710b6 items 3 and 7). A test whose name
// starts TestAdvW17Break is a defect found on 112759d: it fails there.

func w17Tools() []llm.ToolSchema {
	return []llm.ToolSchema{{Name: "echo", Description: "d", InputSchema: []byte(`{"type":"object"}`)}}
}

// w17Turn is trimTurn with a chosen signature, so a test can say which block was
// produced before the frame and which after.
func w17Turn(t *testing.T, l *Log, label, sig string) {
	t.Helper()
	mustAppend(t, l, TurnStart, nil)
	mustAppend(t, l, UserMessage, MessageData{Message: user(label + strings.Repeat("y", 1500))})
	mustAppend(t, l, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "t", Signature: sig}, llm.TextBlock{Text: "done"}}}})
	mustAppend(t, l, TurnEnd, nil)
}

// w17GrowthFromNothing is the encoding item 3 writes over a log that already
// answered: a WhyToolsAdded frame, with every tool in Added, and no frame before it.
func w17GrowthFromNothing(t *testing.T, l *Log) {
	t.Helper()
	mustAppend(t, l, WireFrame, FrameData{System: "p", Tools: w17Tools(), Why: WhyToolsAdded, Added: []string{"echo"}})
}

func w17PreFramed(t *testing.T, pre, post int) *Log {
	t.Helper()
	l := NewLog()
	for i := 0; i < pre; i++ {
		w17Turn(t, l, fmt.Sprintf("pre%d", i), fmt.Sprintf("sig-pre%d", i))
	}
	w17GrowthFromNothing(t, l)
	for i := 0; i < post; i++ {
		w17Turn(t, l, fmt.Sprintf("post%d", i), fmt.Sprintf("sig-post%d", i))
	}
	return l
}

func w17AnyPre(sigs []string) []string {
	var out []string
	for _, s := range sigs {
		if strings.HasPrefix(s, "sig-pre") {
			out = append(out, s)
		}
	}
	return out
}

// Attack 2. The file item 3 writes uses only what the format already has: the
// reader that decides what to strip is staleSpans, and it reads the WhyToolsAdded
// frame as a boundary whatever came before it. So a reader that is unchanged by
// the lane (session/stale.go is byte-identical between 7b16a09 and 112759d) strips
// the pre-frame block and keeps the later one, at every placement of the retention
// trim around the frame, and the file is still format version 1.
func TestAdvW17GrowthFromNothingStripsPreFrameReasoningAtEveryTrimPlacement(t *testing.T) {
	for pre := 1; pre <= 4; pre++ {
		for post := 1; post <= 6; post++ {
			store := tightStore(t.TempDir())
			id := newID(t)
			l := w17PreFramed(t, pre, post)
			if _, err := store.Save(id, l); err != nil {
				t.Fatal(err)
			}
			raw, err := readFileOf(t, store, id)
			if err != nil {
				t.Fatal(err)
			}
			var head struct {
				Version int `json:"manvi_session"`
			}
			if err := json.Unmarshal(raw, &head); err != nil || head.Version != FormatVersion || FormatVersion != 1 {
				t.Fatalf("pre=%d post=%d: file version %d (FormatVersion %d), want 1", pre, post, head.Version, FormatVersion)
			}
			restored, _, err := NewStore(store.Dir()).Load(id)
			if err != nil {
				t.Fatal(err)
			}
			kept, _, _ := strippedFor(t, restored)
			if bad := w17AnyPre(kept); len(bad) != 0 {
				t.Fatalf("pre=%d post=%d: reasoning produced before the frame was replayed after a save and load: %v", pre, post, bad)
			}
		}
	}
}

// Attack 1a. Pre-frame, then framed, then trimmed, then resumed and trimmed
// again: no pre-frame block is ever replayed, and what survives is a leading-run
// removal (leadingRunOnly).
func TestAdvW17PreFrameThenFramedThenTrimmedThenResumedNeverReplaysPreFrame(t *testing.T) {
	dir, id := t.TempDir(), newID(t)
	store := tightStore(dir)
	l := w17PreFramed(t, 3, 2)
	if _, err := store.Save(id, l); err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < 4; cycle++ {
		next := tightStore(dir)
		loaded, _, err := next.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		w17Turn(t, loaded, fmt.Sprintf("resumed%d", cycle), fmt.Sprintf("sig-resumed%d", cycle))
		kept, _, _ := strippedFor(t, loaded)
		if bad := w17AnyPre(kept); len(bad) != 0 {
			t.Fatalf("cycle %d: pre-frame reasoning replayed: %v", cycle, bad)
		}
		if _, err := next.Save(id, loaded); err != nil {
			t.Fatal(err)
		}
	}
}

// A growth-from-nothing frame must also be a boundary in the live log, before
// any save: Wire() and the strip agree.
func TestAdvW17GrowthFromNothingIsABoundaryInTheLiveLog(t *testing.T) {
	l := w17PreFramed(t, 2, 1)
	kept, dropped, before := strippedFor(t, l)
	if before != 3 || dropped != 2 || !slices.Equal(kept, []string{"sig-post0"}) {
		t.Fatalf("before=%d dropped=%d kept=%v, want 3, 2, [sig-post0]", before, dropped, kept)
	}
	if f := l.Wire().Frame; f == nil || f.Why != WhyToolsAdded {
		t.Fatalf("wire frame = %+v", f)
	}
}

// saveReportFor drives n labelled turns into a log and saves it.
func w17Cycle(t *testing.T, dir, id string, log *Log, labels ...string) SaveReport {
	t.Helper()
	store := tightStore(dir)
	if log == nil {
		var err error
		if log, _, err = store.Load(id); err != nil {
			t.Fatal(err)
		}
	}
	for _, label := range labels {
		trimTurn(t, log, label)
	}
	r, err := store.Save(id, log)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Attack 3. The cumulative count is the same whether the process that trimmed
// it was interrupted part way (SIGKILL-equivalent: panic at the named write step,
// leaving whatever the step left) or not, and SaveReport is the delta against the
// file the save replaced.
func TestAdvW17TrimmedTurnsCumulativeAcrossSaveLoadSaveAndAnInterruptedSave(t *testing.T) {
	for _, step := range []string{"created", "written", "synced"} {
		t.Run(step, func(t *testing.T) {
			dir, id := t.TempDir(), newID(t)
			first := w17Cycle(t, dir, id, NewLog(), "a", "b", "c", "d", "e", "f", "g", "h")
			if first.TrimmedTurns == 0 {
				t.Fatal("fixture did not trim")
			}
			s0 := tightStore(dir)
			if _, _, err := s0.Load(id); err != nil {
				t.Fatal(err)
			}
			base := headerTrimmed(t, s0, id)

			// An interrupted save: the write panics at step, so no new generation is published.
			store := tightStore(dir)
			log, _, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			for _, label := range []string{"i", "j", "k"} {
				trimTurn(t, log, label)
			}
			store.writeStep = func(at string) {
				if at == step {
					panic("interrupted")
				}
			}
			func() {
				defer func() { _ = recover() }()
				_, _ = store.Save(id, log)
			}()
			store.writeStep = nil
			if got := headerTrimmed(t, store, id); got != base {
				t.Fatalf("an interrupted save changed the published count: %d -> %d", base, got)
			}
			// The retry on the same store and log.
			retry, err := store.Save(id, log)
			if err != nil {
				t.Fatal(err)
			}
			got := headerTrimmed(t, store, id)

			// What an uninterrupted save of the same turns writes.
			cleanDir := t.TempDir()
			w17Cycle(t, cleanDir, id, NewLog(), "a", "b", "c", "d", "e", "f", "g", "h")
			clean := w17Cycle(t, cleanDir, id, nil, "i", "j", "k")
			want := headerTrimmed(t, tightStore(cleanDir), id)
			if got != want || retry.TrimmedTurns != clean.TrimmedTurns {
				t.Fatalf("interrupted-then-retried wrote %d (reported %d), uninterrupted wrote %d (reported %d)",
					got, retry.TrimmedTurns, want, clean.TrimmedTurns)
			}
			if retry.TrimmedTurns != got-base {
				t.Fatalf("report %d is not the delta %d-%d", retry.TrimmedTurns, got, base)
			}
		})
	}
}

// Attack 3. Two processes on one session: the second to save is refused, and the
// published count is the first's.
func TestAdvW17TwoProcessesSharingASessionFileNeverDoubleCount(t *testing.T) {
	dir, id := t.TempDir(), newID(t)
	w17Cycle(t, dir, id, NewLog(), "a", "b", "c", "d", "e", "f", "g", "h")
	a, b := tightStore(dir), tightStore(dir)
	la, _, err := a.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	lb, _, err := b.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []*Log{la, lb} {
		trimTurn(t, l, "x")
		trimTurn(t, l, "y")
	}
	if _, err := a.Save(id, la); err != nil {
		t.Fatal(err)
	}
	afterA := headerTrimmed(t, a, id)
	if _, err := b.Save(id, lb); err == nil {
		t.Fatal("the second process overwrote the first's generation")
	}
	if got := headerTrimmed(t, a, id); got != afterA {
		t.Fatalf("the refused save changed the count %d -> %d", afterA, got)
	}
}

// w17Rewrite replaces trimmed_turns in the current generation's header. The
// checksum covers the events only, so the file still loads.
func w17Rewrite(t *testing.T, s *Store, id string, count json.Number) {
	t.Helper()
	s.mu.Lock()
	_, path, raw, err := s.readCurrent(id)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["trimmed_turns"] = json.RawMessage(count.String())
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Attack 3. A crafted or wrong count changes what is reported, never what is
// replayed: for a trimmed file with no carried frame, every surviving signed
// block stays stripped whatever the header says (the sequence numbers see the
// trim), and a file with a carried frame strips exactly what its Through says.
func TestAdvW17CraftedTrimmedTurnsNeverLoosensTheStrip(t *testing.T) {
	for _, framed := range []bool{false, true} {
		for _, craft := range []json.Number{"0", "-1", "-9223372036854775808", "9223372036854775807", "1"} {
			t.Run(fmt.Sprintf("framed=%v/count=%s", framed, craft), func(t *testing.T) {
				dir, id := t.TempDir(), newID(t)
				store := tightStore(dir)
				l := NewLog()
				if framed {
					frameOpened(t, l)
				}
				for _, label := range []string{"one", "two", "three", "four", "five", "six"} {
					trimTurn(t, l, label)
				}
				r, err := store.Save(id, l)
				if err != nil || r.TrimmedTurns == 0 {
					t.Fatalf("save: %v, trimmed %d", err, r.TrimmedTurns)
				}
				honest, _, err := tightStore(dir).Load(id)
				if err != nil {
					t.Fatal(err)
				}
				wantKept, _, _ := strippedFor(t, honest)

				w17Rewrite(t, store, id, craft)
				loaded, _, err := tightStore(dir).Load(id)
				if err != nil {
					t.Fatal(err)
				}
				gotKept, _, _ := strippedFor(t, loaded)
				if !slices.Equal(gotKept, wantKept) {
					t.Fatalf("header count %s changed what is replayed: honest %v, crafted %v", craft, wantKept, gotKept)
				}
				// And resuming on a crafted count never replays older blocks either.
				cs := tightStore(dir)
				resumed, _, err := cs.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				trimTurn(t, resumed, "after")
				k2, _, _ := strippedFor(t, resumed)
				if len(k2) > 1 || (len(k2) == 1 && k2[0] != "sig-after") {
					t.Fatalf("crafted count %s: after resuming, replayed %v", craft, k2)
				}
				if _, err := cs.Save(id, resumed); err != nil {
					t.Fatalf("a save over a crafted count failed: %v", err)
				}
			})
		}
	}
}

// BREAK candidate (reporting only, no effect on replay). A count at the integer
// limit wraps negative when more turns are dropped, and the header is written
// with a negative trimmed_turns that the next Load reports as such.
func TestAdvW17BreakCumulativeCountSaturatesInsteadOfWrapping(t *testing.T) {
	dir, id := t.TempDir(), newID(t)
	store := tightStore(dir)
	l := NewLog()
	for _, label := range []string{"one", "two", "three", "four", "five", "six"} {
		trimTurn(t, l, label)
	}
	if _, err := store.Save(id, l); err != nil {
		t.Fatal(err)
	}
	w17Rewrite(t, store, id, json.Number(fmt.Sprint(math.MaxInt)))
	cs := tightStore(dir)
	resumed, _, err := cs.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"a", "b", "c", "d", "e", "f"} {
		trimTurn(t, resumed, label)
	}
	report, err := cs.Save(id, resumed)
	if err != nil {
		t.Fatal(err)
	}
	if got := headerTrimmed(t, cs, id); got < 0 {
		t.Fatalf("trimmed_turns wrapped to %d after dropping %d more turn(s) over a header at MaxInt", got, report.TrimmedTurns)
	}
}

// BREAK candidate (reporting only). SaveReport counts the new drops. A log loaded
// from a trimmed file and saved where no file exists (the file was removed under
// the run, or it is saved under another store) drops nothing in this save, but
// the report carries the file's old count as if this save had dropped it.
func TestAdvW17BreakReportCountsOnlyNewDropsWhereNoFileExistsYet(t *testing.T) {
	dir, id := t.TempDir(), newID(t)
	w17Cycle(t, dir, id, NewLog(), "a", "b", "c", "d", "e", "f", "g", "h")
	loaded, lr, err := tightStore(dir).Load(id)
	if err != nil || lr.TrimmedTurns == 0 {
		t.Fatalf("load: %v, trimmed %d", err, lr.TrimmedTurns)
	}
	r, err := tightStore(t.TempDir()).Save(id, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if r.TrimmedTurns != 0 {
		t.Fatalf("this save dropped nothing, but its report says %d turn(s) were dropped", r.TrimmedTurns)
	}
}
