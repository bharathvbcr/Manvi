package session

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// Adversarial probes for ft-ff7dfd1a (trim budget comma count). The oracle here
// is the real artefact: the file Save wrote, the events array inside it, and
// what Load hands back.

// advBoundaries is an independent statement of where whole turns begin: the
// start of a run of prompt-assembly records that ends in a turn/start, or a
// turn/start not preceded by one. Index 0 always starts the leading group.
func advBoundaries(events []Event) []int {
	var out []int
	for i, e := range events {
		switch {
		case e.Type == TurnStart && (i == 0 || events[i-1].Type != PromptAssembled):
			out = append(out, i)
		case e.Type == PromptAssembled && (i == 0 || events[i-1].Type != PromptAssembled):
			j := i
			for j < len(events) && events[j].Type == PromptAssembled {
				j++
			}
			if j < len(events) && events[j].Type == TurnStart {
				out = append(out, i)
			}
		}
	}
	if len(out) == 0 || out[0] != 0 {
		out = append([]int{0}, out...)
	}
	return out
}

func advLastTurnStart(events []Event) int {
	k := -1
	for i, e := range events {
		if e.Type == TurnStart {
			k = i
		}
	}
	last := 0
	for _, b := range advBoundaries(events) {
		if b <= k {
			last = b
		}
	}
	return last
}

func advAppend(log *Log, typ Type, payload any) {
	_, _ = log.Append(typ, payload)
}

// advShapes builds histories the existing generator does not: runs of
// prompt-assembly records, one at index 0, empty turns, step/end payloads,
// stream chunks, a huge first turn, and empty-ish payloads.
func advLog(rng *lcg, shape int) *Log {
	log := NewLog()
	text := func(n int) string { return strings.Repeat("é<", n) }
	msg := func(n int) any {
		return MessageData{Message: llm.Message{Role: llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: text(n)}}}}
	}
	turns := 1 + rng.Intn(6)
	for k := 0; k < turns; k++ {
		for r := rng.Intn(4); r > 0 && shape%2 == 0; r-- {
			advAppend(log, PromptAssembled, PromptAssembledData{Tokens: rng.Intn(100000), Budget: 1})
		}
		advAppend(log, TurnStart, nil)
		size := rng.Intn(40)
		if shape%3 == 0 && k == 0 {
			size = 4000 // a huge first turn
		}
		switch rng.Intn(4) {
		case 0: // an empty turn: nothing but its boundaries
		case 1:
			advAppend(log, UserMessage, msg(size))
		default:
			advAppend(log, UserMessage, msg(size))
			advAppend(log, StepStart, nil)
			for c := rng.Intn(5); c > 0; c-- {
				advAppend(log, AssistantChunk, llm.Chunk{Kind: llm.ChunkText, Text: text(rng.Intn(10))})
			}
			advAppend(log, StepEnd, StepEndData{Usage: llm.Usage{InputTokens: rng.Intn(1 << 20), OutputTokens: rng.Intn(5000)},
				CostNanoUSD: int64(rng.Intn(1 << 30)), Priced: rng.Intn(2) == 0})
		}
		if rng.Intn(5) != 0 {
			advAppend(log, TurnEnd, nil)
		}
	}
	return log
}

func TestAdvSaveLoadRoundTripAgreesWithTheBudget(t *testing.T) {
	rng := lcg(7919)
	for iter := 0; iter < 50; iter++ {
		log := advLog(&rng, iter)
		events, perEvent, err := log.persistableEncoded()
		if err != nil || len(events) == 0 {
			continue
		}
		bounds := advBoundaries(events)
		last := advLastTurnStart(events)

		budgets := map[int]bool{1: true, 2: true, 3: true}
		for _, b := range bounds {
			if len(budgets) > 14 {
				break
			}
			n := arrayLen(perEvent[b:])
			budgets[n], budgets[n-1] = true, true
		}
		for budget := range budgets {
			if budget < 1 {
				continue
			}
			store := NewStore(t.TempDir())
			store.setLimits(budget, 0)
			id := newID(t)
			report, err := store.Save(id, log)
			if err != nil {
				t.Fatalf("iter %d budget %d: save: %v", iter, budget, err)
			}
			_, path, _ := store.current(id)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var doc header
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}

			// Oracle: longest suffix at a whole-turn boundary that really fits,
			// measured on the bytes in the file, else the final turn.
			wantFrom := last
			for _, b := range bounds {
				if b > last {
					break
				}
				if len(joinArray(perEvent[b:])) <= budget {
					wantFrom = b
					break
				}
			}
			wantKept := joinArray(perEvent[wantFrom:])
			if string(doc.Events) != string(wantKept) {
				t.Fatalf("iter %d budget %d: file holds %d event bytes, oracle keeps from %d (%d bytes); report %+v",
					iter, budget, len(doc.Events), wantFrom, len(wantKept), report)
			}
			if doc.Oversize != (len(doc.Events) > budget) || report.Oversize != doc.Oversize {
				t.Fatalf("iter %d budget %d: Oversize report=%v file=%v but the file's array is %d bytes",
					iter, budget, report.Oversize, doc.Oversize, len(doc.Events))
			}
			wantDropped := 0
			for _, e := range events[:wantFrom] {
				if e.Type == TurnStart {
					wantDropped++
				}
			}
			if report.TrimmedTurns != wantDropped || doc.TrimmedTurns != wantDropped {
				t.Fatalf("iter %d budget %d: TrimmedTurns report=%d file=%d, %d turn/starts were dropped",
					iter, budget, report.TrimmedTurns, doc.TrimmedTurns, wantDropped)
			}

			loaded, lr, err := store.Load(id)
			if err != nil {
				t.Fatalf("iter %d budget %d: load of what save wrote: %v", iter, budget, err)
			}
			got := loaded.Events()
			if len(got) != len(events)-wantFrom || lr.Oversize != doc.Oversize || lr.TrimmedTurns != wantDropped {
				t.Fatalf("iter %d budget %d: loaded %d events (want %d), report %+v", iter, budget, len(got), len(events)-wantFrom, lr)
			}
			for i, e := range got {
				if e.Seq != events[wantFrom+i].Seq || string(e.Data) != string(events[wantFrom+i].Data) {
					t.Fatalf("iter %d budget %d: event %d differs after the round trip", iter, budget, i)
				}
			}

			// Saving what was loaded, under the same budget, is a no-op.
			again, err := store.Save(id, loaded)
			if err != nil {
				t.Fatalf("iter %d budget %d: second save: %v", iter, budget, err)
			}
			if again.TrimmedTurns != 0 || again.Oversize != doc.Oversize || again.Events != len(got) {
				t.Fatalf("iter %d budget %d: re-saving the loaded history changed it: %+v (first %+v)", iter, budget, again, report)
			}
		}
	}
}

// Budgets under len("[]") with real content, and an empty array at budget 1.
func TestAdvBudgetBelowTheBrackets(t *testing.T) {
	log := NewLog()
	advAppend(log, TurnStart, nil)
	advAppend(log, TurnEnd, nil)
	events := log.Events()
	for _, budget := range []int{-5, 0, 1} {
		if _, dropped, oversize, _, err := trimToBudget(events, budget); err != nil || dropped != 0 || !oversize {
			t.Errorf("budget %d: dropped=%d oversize=%v err=%v; the single turn cannot fit, so it is kept and flagged", budget, dropped, oversize, err)
		}
	}
	// No events at all: "[]" is 2 bytes. A budget of 1 cannot hold it, but there
	// is nothing to drop either; report which way the code goes.
	from, dropped, oversize, _, err := trimToBudget(nil, 1)
	t.Logf("empty history at budget 1: from=%d dropped=%d oversize=%v err=%v", from, dropped, oversize, err)
	if from != 0 || dropped != 0 {
		t.Errorf("empty history trimmed: from=%d dropped=%d", from, dropped)
	}
}

// The comma claim, broken directly: the kept set is never empty, but is the
// *dropped* prefix always followed by a kept element? A history whose last
// start index is also its last event (a final turn/start with nothing after
// it) must still subtract exactly len+1 per dropped element.
func TestAdvFinalTurnIsASingleEvent(t *testing.T) {
	log := NewLog()
	for i := 0; i < 3; i++ {
		advAppend(log, PromptAssembled, PromptAssembledData{Tokens: i})
		advAppend(log, TurnStart, nil)
		advAppend(log, UserMessage, MessageData{Message: user(strings.Repeat("p", 100))})
	}
	advAppend(log, TurnStart, nil) // the final turn is one bare event
	events, perEvent, err := log.persistableEncoded()
	if err != nil {
		t.Fatal(err)
	}
	for b := 0; b <= len(events); b++ {
		for _, delta := range []int{-1, 0, 1} {
			budget := arrayLen(perEvent[min(b, len(events)):]) + delta
			if budget < 1 {
				continue
			}
			from, _, oversize := trimEncoded(events, perEvent, budget)
			real := len(joinArray(perEvent[from:]))
			if oversize != (real > budget) {
				t.Fatalf("budget %d: oversize=%v but kept array is %d bytes (from=%d)", budget, oversize, real, from)
			}
		}
	}
}

// A final turn bigger than maxLoadBytes: Save accepts and flags it, Load then
// refuses the file. A session that can be written and never resumed is the
// "resume means start over" failure the oversize rule exists to rule out.
func TestAdvOversizeFinalTurnStaysLoadable(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates ~65 MiB of payload")
	}
	log := NewLog()
	advAppend(log, TurnStart, nil)
	advAppend(log, UserMessage, MessageData{Message: user(strings.Repeat("x", maxLoadBytes+1024))})
	advAppend(log, TurnEnd, nil)

	store := NewStore(t.TempDir())
	id := newID(t)
	report, saveErr := store.Save(id, log)
	if saveErr != nil {
		return // refusing at Save is an acceptable resolution
	}
	if _, _, err := store.Load(id); err != nil {
		t.Fatalf("Save accepted a %d-event history (Oversize=%v) that Load then refuses: %v",
			report.Events, report.Oversize, err)
	}
}

// What the budget measures is the events array only, not the file. A budget
// equal to the array leaves the header on top; the file may exceed DefaultMaxBytes
// by the header, which maxLoadBytes absorbs. Reported, not asserted against.
func TestAdvFileSizeVersusBudget(t *testing.T) {
	log := NewLog()
	for i := 0; i < 3; i++ {
		conversation(t, log, fmt.Sprintf("p%d", i), strings.Repeat("r", 300), "done")
	}
	_, perEvent, _ := log.persistableEncoded()
	store := NewStore(t.TempDir())
	store.setLimits(arrayLen(perEvent), 0)
	id := newID(t)
	if _, err := store.Save(id, log); err != nil {
		t.Fatal(err)
	}
	_, path, _ := store.current(id)
	info, _ := os.Stat(path)
	t.Logf("budget %d, file %d bytes (header overhead %d)", arrayLen(perEvent), info.Size(), int(info.Size())-arrayLen(perEvent))
}
