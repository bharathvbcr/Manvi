package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// Save reuses the per-event encodings it measured with. That is only sound if
// joining them is exactly what marshalling the kept slice produces — escaping,
// empty payloads and all — at every trim point.
func TestJoinedEncodingsMatchMarshallingTheSlice(t *testing.T) {
	log := NewLog()
	for turn := 0; turn < 4; turn++ {
		if _, err := log.Append(TurnStart, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(UserMessage, MessageData{Message: llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: "<html> &   " + strings.Repeat("x", 200)}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	events := log.snapshot()
	for _, budget := range []int{1 << 20, 1200, 600, 10} {
		from, _, _, perEvent, err := trimToBudget(events, budget)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(events[from:])
		if err != nil {
			t.Fatal(err)
		}
		if got := joinArray(perEvent[from:]); !bytes.Equal(got, want) {
			t.Fatalf("budget %d: joined encoding differs\n got %s\nwant %s", budget, got, want)
		}
	}
	if got := joinArray(nil); string(got) != "[]" {
		t.Fatalf("empty join = %s", got)
	}
}

// encodedLen is the size of the events array exactly as the file holds it:
// what the budget is a bound on.
func encodedLen(t *testing.T, events []Event) int {
	t.Helper()
	b, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func threeTurns(t *testing.T) *Log {
	t.Helper()
	log := NewLog()
	for i := 0; i < 3; i++ {
		conversation(t, log, fmt.Sprintf("p%d", i), strings.Repeat("r", 300), "done")
	}
	return log
}

// A history whose real encoding is exactly the budget fits. The budget used to
// charge a comma after every element, one more than joinArray writes, so the
// oldest turn was dropped (or, with one turn, the save was flagged Oversize)
// although every byte of it fit.
func TestHistoryThatFitsTheBudgetExactlyIsKept(t *testing.T) {
	events := threeTurns(t).Events()
	exact := encodedLen(t, events)

	from, dropped, oversize, _, err := trimToBudget(events, exact)
	if err != nil {
		t.Fatal(err)
	}
	if from != 0 || dropped != 0 || oversize {
		t.Fatalf("budget %d equals the encoding: from=%d dropped=%d oversize=%v, want 0 0 false",
			exact, from, dropped, oversize)
	}

	// One byte under really does not fit, and still trims.
	from, dropped, oversize, _, err = trimToBudget(events, exact-1)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 || from == 0 || oversize {
		t.Fatalf("budget %d is one byte short: from=%d dropped=%d oversize=%v, want a one-turn trim",
			exact-1, from, dropped, oversize)
	}
}

func TestSaveKeepsAHistoryThatFitsTheBudgetExactly(t *testing.T) {
	log := threeTurns(t)
	store := NewStore(t.TempDir())
	store.setLimits(encodedLen(t, log.Events()), 0)

	report, err := store.Save(newID(t), log)
	if err != nil {
		t.Fatal(err)
	}
	if report.TrimmedTurns != 0 || report.Oversize || report.Events != log.Len() {
		t.Fatalf("an exactly-fitting history was cut: %+v (log has %d events)", report, log.Len())
	}
}

// One turn that exactly fits is not oversize; one byte less is.
func TestASingleTurnThatFitsExactlyIsNotOversize(t *testing.T) {
	log := NewLog()
	conversation(t, log, "only", strings.Repeat("z", 200), "done")
	events := log.Events()
	exact := encodedLen(t, events)

	for budget, want := range map[int]bool{exact: false, exact - 1: true} {
		from, dropped, oversize, _, err := trimToBudget(events, budget)
		if err != nil {
			t.Fatal(err)
		}
		if from != 0 || dropped != 0 || oversize != want {
			t.Errorf("budget %d: from=%d dropped=%d oversize=%v, want 0 0 %v", budget, from, dropped, oversize, want)
		}
	}
}

func TestArrayLenIsWhatJoinArrayWrites(t *testing.T) {
	for n := 0; n < 5; n++ {
		els := make([]json.RawMessage, n)
		for i := range els {
			els[i] = json.RawMessage(strings.Repeat("1", i+1))
		}
		if got, want := arrayLen(els), len(joinArray(els)); got != want {
			t.Errorf("%d elements: arrayLen=%d, joinArray wrote %d", n, got, want)
		}
	}
}

// lcg is a fixed-seed linear congruential generator: the histories are reproducible and the
// test needs no source of real randomness.
type lcg int

func (x *lcg) Intn(n int) int {
	*x = (*x*1103515245 + 12345) & 0x7fffffff
	return int(*x>>8) % n
}

// randomLog builds a history of random shape: leading events before any turn,
// prompt-assembly records ahead of turns, multibyte and escaped text of random
// length.
func randomLog(t *testing.T, rng *lcg, assembly bool) *Log {
	t.Helper()
	pieces := []string{"x", "é", "日本", "<&>", "\"q\"", "😀", "\n"}
	text := func() string {
		var b strings.Builder
		for i, n := 0, rng.Intn(60); i < n; i++ {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		return b.String()
	}
	log := NewLog()
	for i, n := 0, rng.Intn(3); i < n; i++ {
		mustAppend(t, log, SystemPrompt, SystemPromptData{Text: text()})
	}
	for turns := rng.Intn(6); turns > 0; turns-- {
		if assembly && rng.Intn(2) == 0 {
			mustAppend(t, log, PromptAssembled, PromptAssembledData{Tokens: rng.Intn(1000), Budget: 100})
		}
		mustAppend(t, log, TurnStart, nil)
		for i, n := 0, rng.Intn(4); i < n; i++ {
			mustAppend(t, log, UserMessage, MessageData{Message: llm.Message{
				Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: text()}},
			}})
		}
		mustAppend(t, log, TurnEnd, nil)
	}
	if log.Len() == 0 {
		mustAppend(t, log, SystemPrompt, SystemPromptData{Text: "s"})
	}
	return log
}

// The budget arithmetic must equal the length of the array that is actually
// persisted, at every budget, for random histories — and the kept suffix must
// be the longest one at a turn boundary that fits.
func TestBudgetEqualsTheLengthOfThePersistedArray(t *testing.T) {
	rng := lcg(20261021)
	for iter := 0; iter < 400; iter++ {
		assembly := iter%2 == 0
		events := randomLog(t, &rng, assembly).Events()
		perEvent, err := encodeEvents(events)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := arrayLen(perEvent), encodedLen(t, events); got != want {
			t.Fatalf("iter %d: computed %d, marshalled %d", iter, got, want)
		}
		full := encodedLen(t, events)
		budgets := []int{full, full - 1, full + 1, 2, 0, rng.Intn(full + 1)}
		for _, budget := range budgets {
			from, dropped, oversize, _, err := trimToBudget(events, budget)
			if err != nil {
				t.Fatal(err)
			}
			kept := encodedLen(t, events[from:])
			if oversize != (kept > budget) {
				t.Fatalf("iter %d budget %d: oversize=%v but the kept array is %d bytes", iter, budget, oversize, kept)
			}
			if full <= budget && (from != 0 || dropped != 0) {
				t.Fatalf("iter %d: history of %d bytes fits budget %d yet from=%d dropped=%d", iter, full, budget, from, dropped)
			}
			if dropped > countTurns(events) || countTurns(events[from:]) != countTurns(events)-dropped {
				t.Fatalf("iter %d budget %d: dropped=%d but turns went %d -> %d",
					iter, budget, dropped, countTurns(events), countTurns(events[from:]))
			}
			if assembly {
				continue
			}
			// Without assembly records the boundaries are the turn starts
			// (plus the leading group at 0). The result must be the longest
			// suffix among them that fits, or the last turn alone.
			wantFrom := lastBoundary(events)
			for i := 0; i < len(events); i++ {
				if (i == 0 || events[i].Type == TurnStart) && encodedLen(t, events[i:]) <= budget {
					wantFrom = i
					break
				}
			}
			if from != wantFrom {
				t.Fatalf("iter %d budget %d: kept from %d, the longest fitting turn boundary is %d", iter, budget, from, wantFrom)
			}
		}
	}
}

// lastBoundary is where the final turn starts: the floor a budget cannot cut
// below. With no turns at all nothing is ever dropped.
func lastBoundary(events []Event) int {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == TurnStart {
			return i
		}
	}
	return 0
}

// Attack: a final turn larger than the whole budget is kept whole and flagged;
// earlier turns still go.
func TestOutsizedFinalTurnIsKeptAfterEarlierTurnsGo(t *testing.T) {
	log := NewLog()
	conversation(t, log, "small", "s", "done")
	conversation(t, log, "huge", strings.Repeat("h", 5000), "done")
	events := log.Events()

	from, dropped, oversize, _, err := trimToBudget(events, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 || !oversize || from != lastBoundary(events) {
		t.Fatalf("from=%d dropped=%d oversize=%v, want the huge turn alone, flagged", from, dropped, oversize)
	}
	if !strings.Contains(string(events[from+2].Data), "huge") {
		t.Errorf("the kept turn is not the final one")
	}
}

// Attack: events with no turn at all are never counted as dropped turns, and
// an empty log encodes to the two brackets.
func TestBudgetEdgeShapes(t *testing.T) {
	if from, dropped, oversize, _, err := trimToBudget(nil, 2); err != nil || from != 0 || dropped != 0 || oversize {
		t.Errorf("zero events at budget 2: %d %d %v %v", from, dropped, oversize, err)
	}
	log := NewLog()
	mustAppend(t, log, SystemPrompt, SystemPromptData{Text: strings.Repeat("é", 100)})
	events := log.Events()
	exact := encodedLen(t, events)
	if from, dropped, oversize, _, _ := trimToBudget(events, exact); from != 0 || dropped != 0 || oversize {
		t.Errorf("turnless history that fits exactly: %d %d %v", from, dropped, oversize)
	}
	if _, dropped, oversize, _, _ := trimToBudget(events, exact-1); dropped != 0 || !oversize {
		t.Errorf("turnless history one byte over: dropped=%d oversize=%v, want flagged and untouched", dropped, oversize)
	}
}

// Attack: the assembly record directly before a turn travels with it at the
// exact-fit boundary, and is not left behind on the dropped side.
func TestExactFitKeepsTheAssemblyRecordWithItsTurn(t *testing.T) {
	log := NewLog()
	for i := 0; i < 3; i++ {
		preTurn(t, log, i)
		conversation(t, log, fmt.Sprintf("p%d", i), strings.Repeat("r", 200), "done")
	}
	events := log.Events()
	perTurn := len(events) / 3
	lastTurn := events[2*perTurn:]
	exact := encodedLen(t, lastTurn)

	from, dropped, oversize, _, err := trimToBudget(events, exact)
	if err != nil {
		t.Fatal(err)
	}
	if from != 2*perTurn || dropped != 2 || oversize {
		t.Fatalf("budget %d = last turn with its record: from=%d (want %d) dropped=%d oversize=%v",
			exact, from, 2*perTurn, dropped, oversize)
	}
	if events[from].Type != PromptAssembled {
		t.Fatalf("kept history starts with %s, want the turn's own %s", events[from].Type, PromptAssembled)
	}
	if _, dropped, _, _, _ := trimToBudget(events, exact-1); dropped != 2 {
		t.Errorf("one byte short of the last turn: dropped %d, want 2 (never below the last turn)", dropped)
	}
}

// Attack: streamed chunks are not charged to the budget, so a history that
// fits without them is not trimmed because of them, at the exact edge.
func TestChunksDoNotMoveTheExactFitEdge(t *testing.T) {
	streamed := NewLog()
	for i := 0; i < 3; i++ {
		streamedTurn(t, streamed, fmt.Sprintf("p%d", i), 40)
	}
	store := NewStore(t.TempDir())
	_, perEvent, err := streamed.persistableEncoded()
	if err != nil {
		t.Fatal(err)
	}
	store.setLimits(arrayLen(perEvent), 0)
	report, err := store.Save(newID(t), streamed)
	if err != nil {
		t.Fatal(err)
	}
	if report.TrimmedTurns != 0 || report.Oversize {
		t.Fatalf("history that fits without its chunks was trimmed: %+v", report)
	}
}

// Attack: concurrent saves of different sessions through one store stay
// within budget and agree with the arithmetic.
func TestConcurrentSavesRespectTheBudget(t *testing.T) {
	store := NewStore(t.TempDir())
	log := threeTurns(t)
	store.setLimits(encodedLen(t, log.Events()), 0)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report, err := store.Save(newID(t), log)
			if err == nil && (report.TrimmedTurns != 0 || report.Oversize) {
				err = fmt.Errorf("exact-fit save was cut: %+v", report)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}
