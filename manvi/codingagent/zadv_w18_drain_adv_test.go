package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Adversarial tests for ft-5d29c46d (deferred-backlog bound, Codex withdrawals,
// bounded seen). A test named ...Gap... asserts the correct behaviour and fails
// while the defect exists; the rest document hardened behaviour.

type w18aRecorder struct {
	mu   sync.Mutex
	data []string
}

func (r *w18aRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = append(r.data, string(p))
	return len(p), nil
}
func (r *w18aRecorder) Close() error { return nil }
func (r *w18aRecorder) writes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.data)
}

const w18aCodexDelta = `{"threadId":"thread","turnId":"turn","delta":"x"}`

func w18aCodexChunk() wireRead {
	return wireRead{value: w18Frame("item/commandExecution/outputDelta", w18aCodexDelta)}
}

const w18aClaudeChunk = `{"type":"user","session_id":"S","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]}}`
const w18aClaudeEnd = `{"type":"result","subtype":"success","session_id":"S","result":"done"}`

func w18aClaude(t *testing.T) *Claude {
	t.Helper()
	return &Claude{sessionID: "S", started: true, turnID: "T", pending: map[string]*claudePending{}, seen: map[string]bool{}, wire: advWire(t, 512)}
}

func w18aClaudeAsk(t *testing.T, s *Claude, i int) Request {
	t.Helper()
	e, ok, err := s.event([]byte(fmt.Sprintf(`{"type":"control_request","request_id":"r%d","session_id":"S","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`, i)))
	if err != nil || !ok || e.Request == nil {
		t.Fatalf("claude capture %d: ok=%v err=%v", i, ok, err)
	}
	return *e.Request
}

// adapter bundles what the edge tests need, per adapter.
type w18aAdapter struct {
	name   string
	chunk  func(s any) wireRead
	end    func() wireRead
	build  func(t *testing.T, pending int) any
	drain  func(s any) error
	next   func(s any) (Event, error)
	frames func(s any) int
	deferd func(s any) int
}

// w18aAs is the checked assertion of an adapter handed around as any.
func w18aAs[T any](s any) T {
	v, ok := s.(T)
	if !ok {
		panic(fmt.Sprintf("adapter is %T", s))
	}
	return v
}

func w18aAdapters() []w18aAdapter {
	return []w18aAdapter{
		{
			name:  "agy",
			chunk: func(any) wireRead { return w18Chunk() },
			end:   func() wireRead { return wireRead{value: advAgyEnd} },
			build: func(t *testing.T, pending int) any {
				s := advAgyIdle(t)
				for i := 0; i < pending; i++ {
					advAgyOffer(t, s, fmt.Sprint(100+i))
				}
				s.deferred = []Event{}
				return s
			},
			drain:  func(s any) error { return w18aAs[*Agy](s).drain() },
			next:   func(s any) (Event, error) { return w18aAs[*Agy](s).Next(context.Background()) },
			frames: func(s any) int { return len(w18aAs[*Agy](s).wire.frames) },
			deferd: func(s any) int { return len(w18aAs[*Agy](s).deferred) },
		},
		{
			name:  "claude",
			chunk: func(any) wireRead { return wireRead{raw: []byte(w18aClaudeChunk)} },
			end:   func() wireRead { return wireRead{raw: []byte(w18aClaudeEnd)} },
			build: func(t *testing.T, pending int) any {
				s := w18aClaude(t)
				for i := 0; i < pending; i++ {
					w18aClaudeAsk(t, s, i)
				}
				s.deferred = []Event{}
				return s
			},
			drain:  func(s any) error { return w18aAs[*Claude](s).drain() },
			next:   func(s any) (Event, error) { return w18aAs[*Claude](s).Next(context.Background()) },
			frames: func(s any) int { return len(w18aAs[*Claude](s).wire.frames) },
			deferd: func(s any) int { return len(w18aAs[*Claude](s).deferred) },
		},
		{
			name:  "codex",
			chunk: func(any) wireRead { return w18aCodexChunk() },
			end:   func() wireRead { return wireRead{value: w18CodexEnd} },
			build: func(t *testing.T, pending int) any {
				s := w18Codex(t)
				for i := 1; i <= pending; i++ {
					if _, ok, err := s.event(w18CodexAsk(i)); err != nil || !ok {
						t.Fatalf("codex capture %d: ok=%v err=%v", i, ok, err)
					}
				}
				s.deferred = []Event{}
				return s
			},
			drain:  func(s any) error { return w18aAs[*Codex](s).drain() },
			next:   func(s any) (Event, error) { return w18aAs[*Codex](s).Next(context.Background()) },
			frames: func(s any) int { return len(w18aAs[*Codex](s).wire.frames) },
			deferd: func(s any) int { return len(w18aAs[*Codex](s).deferred) },
		},
	}
}

func (a w18aAdapter) frameChan(s any) chan wireRead {
	switch v := s.(type) {
	case *Agy:
		return v.wire.frames
	case *Claude:
		return v.wire.frames
	case *Codex:
		return v.wire.frames
	default:
		return nil
	}
}

// Exact item edge: 128 buffered items succeed, 129 fail closed, for every
// adapter, with nothing buffered left unread after a success.
func TestAdvW18DrainItemEdge127_128_129(t *testing.T) {
	for _, a := range w18aAdapters() {
		for _, n := range []int{127, 128, 129} {
			t.Run(fmt.Sprintf("%s/%d", a.name, n), func(t *testing.T) {
				s := a.build(t, 0)
				for i := 0; i < n; i++ {
					a.frameChan(s) <- a.chunk(s)
				}
				err := a.drain(s)
				if n <= 128 {
					if err != nil {
						t.Fatalf("%d buffered items refused: %v", n, err)
					}
					if a.frames(s) != 0 {
						t.Fatalf("drain reported success with %d frames still buffered", a.frames(s))
					}
					if a.deferd(s) != n {
						t.Fatalf("deferred=%d want %d", a.deferd(s), n)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "could not drain") {
					t.Fatalf("129 buffered items: err=%v", err)
				}
			})
		}
	}
}

// GAP: when the item budget is hit, the preflight must not consume a frame it
// then discards. drainBacklog reads the 129th frame only to learn something is
// buffered and drops it ("the frame read to find out is dropped"). If that
// frame is the turn end, the status and the withdrawals are lost for good: the
// refused preflight's caller continues to Next and the run never ends.
// The pre-change loop stopped at the budget without reading the 129th frame.
func TestAdvW18DrainGapBudgetHitMustNotConsumeTheNextFrame(t *testing.T) {
	for _, a := range w18aAdapters() {
		t.Run(a.name, func(t *testing.T) {
			s := a.build(t, 2)
			for i := 0; i < 128; i++ {
				a.frameChan(s) <- a.chunk(s)
			}
			a.frameChan(s) <- a.end() // the 129th frame is the turn end
			if err := a.drain(s); err == nil {
				t.Fatal("expected the preflight to refuse past its item budget")
			}
			if got := a.frames(s); got != 1 {
				t.Fatalf("the refused preflight left %d frames unread, want 1: the turn end was consumed and dropped", got)
			}
		})
	}
}

// The queue branch breaks without consuming: 129 queued items leave exactly one.
func TestAdvW18DrainQueueBudgetLeavesTheRemainderQueued(t *testing.T) {
	s := w18Codex(t)
	for i := 0; i < 129; i++ {
		s.queue = append(s.queue, w18Frame("item/commandExecution/outputDelta", w18aCodexDelta))
	}
	if err := s.drain(); err == nil {
		t.Fatal("129 queued items passed")
	}
	if len(s.queue) != 1 {
		t.Fatalf("queue=%d, want 1 unread", len(s.queue))
	}
}

// Queue and wire share one item budget: 100 queued + 28 framed passes, +29 fails.
func TestAdvW18DrainItemBudgetIsSharedByQueueAndWire(t *testing.T) {
	for _, c := range []struct {
		frames  int
		wantErr bool
	}{{28, false}, {29, true}} {
		s := w18Codex(t)
		for i := 0; i < 100; i++ {
			s.queue = append(s.queue, w18Frame("item/commandExecution/outputDelta", w18aCodexDelta))
		}
		for i := 0; i < c.frames; i++ {
			s.wire.frames <- w18aCodexChunk()
		}
		err := s.drain()
		if c.wantErr != (err != nil) {
			t.Fatalf("frames=%d err=%v", c.frames, err)
		}
	}
}

// Backlog edge via drainBacklog with the real per-event decode: 160 and 161
// pass, 162 refuses. Prefill goes through the deferred slice an earlier
// preflight would have left.
func TestAdvW18DrainBacklogEdge160_161_162(t *testing.T) {
	for _, c := range []struct {
		backlog int
		wantErr bool
	}{{160, false}, {161, false}, {162, true}} {
		s := w18Codex(t)
		s.deferred = make([]Event, c.backlog-1)
		s.wire.frames <- w18aCodexChunk()
		err := s.drain()
		if c.wantErr != (err != nil) {
			t.Fatalf("backlog %d: err=%v", c.backlog, err)
		}
	}
	if maxDeferredBacklog != 161 || maxDrainItems != 128 || maxPendingCallbacks != 32 || maxRunCallbacks != 2048 {
		t.Fatalf("constants drifted: %d %d %d %d", maxDeferredBacklog, maxDrainItems, maxPendingCallbacks, maxRunCallbacks)
	}
}

// Pending 31 / 32 / 33: the 33rd offer is refused at capture, so a turn end
// never carries more than 32 withdrawals; 31 and 32 are both withdrawn in full.
func TestAdvW18PendingCapEdgeAndTurnEndWithdrawsAll(t *testing.T) {
	for _, a := range w18aAdapters() {
		for _, n := range []int{31, 32} {
			t.Run(fmt.Sprintf("%s/%d", a.name, n), func(t *testing.T) {
				s := a.build(t, n)
				for i := 0; i < 127; i++ {
					a.frameChan(s) <- a.chunk(s)
				}
				a.frameChan(s) <- a.end()
				if err := a.drain(s); err != nil {
					t.Fatalf("turn end refused: %v", err)
				}
				if got, want := a.deferd(s), 127+n+1; got != want {
					t.Fatalf("backlog %d want %d", got, want)
				}
				var cancelled int
				total := a.deferd(s)
				for i := 0; i < total; i++ {
					e, err := a.next(s)
					if err != nil {
						t.Fatal(err)
					}
					if e.CancelledID != "" {
						cancelled++
					}
					if e.Status != "" && i != 127+n {
						t.Fatalf("status at %d, want last (%d)", i, 127+n)
					}
				}
				if cancelled != n {
					t.Fatalf("withdrawals=%d want %d", cancelled, n)
				}
			})
		}
	}
	t.Run("33rd refused", func(t *testing.T) {
		s := advAgyIdle(t)
		for i := 0; i < 32; i++ {
			advAgyOffer(t, s, fmt.Sprint(100+i))
		}
		if _, _, err := s.event(advAgyPerm("999")); err == nil {
			t.Fatal("33rd pending callback admitted")
		}
		if s.seen["999"] || s.requestCount != 32 {
			t.Fatalf("refused id recorded: seen=%v count=%d", s.seen["999"], s.requestCount)
		}
		c := w18Codex(t)
		for i := 1; i <= 32; i++ {
			_, _, _ = c.event(w18CodexAsk(i))
		}
		if _, _, err := c.event(w18CodexAsk(999)); err == nil || c.seen["999"] {
			t.Fatalf("codex 33rd: err=%v seen=%v", err, c.seen["999"])
		}
		cl := w18aClaude(t)
		for i := 0; i < 32; i++ {
			w18aClaudeAsk(t, cl, i)
		}
		if _, _, err := cl.event([]byte(`{"type":"control_request","request_id":"x","session_id":"S","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)); err == nil || cl.seen["x"] {
			t.Fatalf("claude 33rd: err=%v seen=%v", err, cl.seen["x"])
		}
	})
}

// Ordering: events decoded before the turn end, the withdrawals in id order,
// the status, then an event decoded after it in the same drain, all come out of
// Next in that order, through a drain that ends exactly at the budget.
func TestAdvW18OrderingAcrossADrainThatHitsTheBound(t *testing.T) {
	s := w18Codex(t)
	for i := 1; i <= 32; i++ {
		_, _, _ = s.event(w18CodexAsk(i))
	}
	s.deferred = []Event{}
	for i := 0; i < 100; i++ {
		s.wire.frames <- wireRead{value: w18Frame("item/commandExecution/outputDelta", fmt.Sprintf(`{"threadId":"thread","turnId":"turn","delta":"c%03d"}`, i))}
	}
	s.wire.frames <- wireRead{value: w18CodexEnd}
	for i := 0; i < 27; i++ { // 100 + end + 27 = 128 items; the 27 follow the status
		s.wire.frames <- wireRead{value: w18Frame("item/commandExecution/outputDelta", fmt.Sprintf(`{"threadId":"thread","turnId":"turn","delta":"t%03d"}`, i))}
	}
	if err := s.drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(s.deferred) != 100+32+1+27 {
		t.Fatalf("backlog %d", len(s.deferred))
	}
	var got []Event
	for len(s.deferred) > 0 {
		e, err := s.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if len(got) < 100 {
		t.Fatalf("got %d events, want at least 100", len(got))
	}
	for i := 0; i < 100; i++ {
		if got[i].Text != fmt.Sprintf("c%03d", i) {
			t.Fatalf("event %d = %+v", i, got[i])
		}
	}
	for i := 0; i < 32; i++ {
		if got[100+i].CancelledID == "" {
			t.Fatalf("event %d = %+v, want a withdrawal", 100+i, got[100+i])
		}
		if i > 0 && got[100+i-1].CancelledID >= got[100+i].CancelledID {
			// ids are sorted as strings by withdrawalEvents
			t.Fatalf("withdrawals out of order: %q then %q", got[100+i-1].CancelledID, got[100+i].CancelledID)
		}
	}
	if got[132].Status != "completed" {
		t.Fatalf("event 132 = %+v, want the status", got[132])
	}
	for i := 0; i < 27; i++ {
		if got[133+i].Text != fmt.Sprintf("t%03d", i) {
			t.Fatalf("event %d = %+v", 133+i, got[133+i])
		}
	}
}

// A backlog already parked by an earlier preflight keeps its place ahead of a
// later preflight's turn end and its withdrawals (agy, Claude, Codex).
func TestAdvW18PriorBacklogStaysAheadOfALaterTurnEnd(t *testing.T) {
	for _, a := range w18aAdapters() {
		t.Run(a.name, func(t *testing.T) {
			s := a.build(t, 3)
			a.frameChan(s) <- a.chunk(s)
			if err := a.drain(s); err != nil {
				t.Fatal(err)
			}
			a.frameChan(s) <- a.chunk(s)
			a.frameChan(s) <- a.end()
			if err := a.drain(s); err != nil {
				t.Fatal(err)
			}
			if a.deferd(s) != 2+3+1 {
				t.Fatalf("deferred=%d", a.deferd(s))
			}
			var kinds []string
			for a.deferd(s) > 0 {
				e, err := a.next(s)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case e.CancelledID != "":
					kinds = append(kinds, "W")
				case e.Status != "":
					kinds = append(kinds, "S")
				default:
					kinds = append(kinds, "T")
				}
			}
			if got := strings.Join(kinds, ""); got != "TTWWWS" {
				t.Fatalf("order = %s, want TTWWWS", got)
			}
		})
	}
}

// The bound is on the TOTAL parked backlog, so two validations with no Next
// between them can refuse a legitimate turn end that the first validation
// alone would have passed. This documents the limit (NOT A BUG: the bound is
// what protects the run, and a refusal here leaves every event deliverable).
func TestAdvW18CumulativeBacklogAcrossPreflightsRefusesAtTheBoundAndLosesNothing(t *testing.T) {
	s := w18Codex(t)
	for i := 1; i <= 32; i++ {
		_, _, _ = s.event(w18CodexAsk(i))
	}
	s.deferred = []Event{}
	for i := 0; i < 128; i++ {
		s.wire.frames <- w18aCodexChunk()
	}
	if err := s.drain(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		s.wire.frames <- w18aCodexChunk()
	}
	s.wire.frames <- wireRead{value: w18CodexEnd}
	err := s.drain()
	if err == nil || !strings.Contains(err.Error(), "event budget") {
		t.Fatalf("err=%v, want the event budget refusal", err)
	}
	// Half state after the refusal: every decoded event is parked, in order,
	// including the whole turn end; nothing is half-inserted.
	var statuses, withdrawals, texts int
	for len(s.deferred) > 0 {
		e, nerr := s.Next(t.Context())
		if nerr != nil {
			t.Fatal(nerr)
		}
		switch {
		case e.Status != "":
			statuses++
		case e.CancelledID != "":
			withdrawals++
		case e.Text != "":
			texts++
		}
	}
	if statuses != 1 || withdrawals != 32 || texts != 133 {
		t.Fatalf("after refusal: statuses=%d withdrawals=%d texts=%d", statuses, withdrawals, texts)
	}
	if !s.finished || len(s.pending) != 0 {
		t.Fatalf("finished=%v pending=%d", s.finished, len(s.pending))
	}
}

// ---- Codex behaviour change -------------------------------------------------

func w18aCodexOffer(t *testing.T, s *Codex, id int) Request {
	t.Helper()
	e, ok, err := s.event(w18CodexAsk(id))
	if err != nil || !ok || e.Request == nil {
		t.Fatalf("offer %d: ok=%v err=%v", id, ok, err)
	}
	return *e.Request
}

// An answer that arrives after the turn end is ErrCallbackWithdrawn through the
// real Respond path and nothing reaches the provider; an answer before it is
// written.
func TestAdvW18CodexAnswerAfterTurnEndIsWithdrawnAndNeverWritten(t *testing.T) {
	s := w18Codex(t)
	rec := &w18aRecorder{}
	s.wire.stdin = rec
	before := w18aCodexOffer(t, s, 1)
	after := w18aCodexOffer(t, s, 2)
	if err := s.Respond(t.Context(), before, Response{Decision: "allow_once"}); err != nil {
		t.Fatalf("answer before the turn end: %v", err)
	}
	if rec.writes() != 1 {
		t.Fatalf("writes=%d", rec.writes())
	}
	s.wire.frames <- wireRead{value: w18CodexEnd}
	var got []Event
	for i := 0; i < 2; i++ { // withdrawal of #2, then status; #1 was answered
		e, err := s.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if len(got) != 2 || got[0].CancelledID != after.ID || got[1].Status != "completed" {
		t.Fatalf("events = %+v", got)
	}
	if err := s.Respond(t.Context(), after, Response{Decision: "allow_once"}); !errors.Is(err, ErrCallbackWithdrawn) {
		t.Fatalf("answer after turn end = %v", err)
	}
	if err := s.Respond(t.Context(), before, Response{Decision: "allow_once"}); err == nil || errors.Is(err, ErrCallbackWithdrawn) {
		t.Fatalf("a second answer to the answered callback = %v, want stale (not withdrawn)", err)
	}
	if rec.writes() != 1 {
		t.Fatalf("a late answer reached the provider: writes=%d", rec.writes())
	}
}

// An already-answered (sent) callback is not withdrawn at turn end: the
// provider's own serverRequest/resolved for it is still accepted afterwards.
func TestAdvW18CodexSentCallbackSurvivesTurnEndAndItsResolutionIsAccepted(t *testing.T) {
	s := w18Codex(t)
	s.wire.stdin = &w18aRecorder{}
	r := w18aCodexOffer(t, s, 1)
	if err := s.Respond(t.Context(), r, Response{Decision: "allow_once"}); err != nil {
		t.Fatal(err)
	}
	s.wire.frames <- wireRead{value: w18CodexEnd}
	s.wire.frames <- wireRead{value: w18Frame("serverRequest/resolved", `{"threadId":"thread","requestId":1}`)}
	e, err := s.Next(t.Context())
	if err != nil || e.Status != "completed" || e.CancelledID != "" {
		t.Fatalf("turn end: %+v %v", e, err)
	}
	e, err = s.Next(t.Context())
	if err != nil || e.ResolvedID != r.ID {
		t.Fatalf("resolution: %+v %v", e, err)
	}
}

// GAP: a serverRequest/resolved for a callback that the turn end withdrew is a
// normal protocol race (the provider resolves what it will no longer wait for
// at the turn boundary). Before the change the callback was still pending, so
// the receipt was accepted as ResolvedID. Now pending no longer holds the id and
// event() fails the whole run with "Codex resolved an unknown callback".
// agy has no resolved frame; Claude's analogue (control_cancel_request for an
// already-withdrawn id) is benign (withdraw returns not-meaningful, no error).
func TestAdvW18CodexGapResolvedAfterTurnEndWithdrawalMustNotFailTheRun(t *testing.T) {
	s := w18Codex(t)
	w18aCodexOffer(t, s, 1)
	s.wire.frames <- wireRead{value: w18CodexEnd}
	s.wire.frames <- wireRead{value: w18Frame("serverRequest/resolved", `{"threadId":"thread","requestId":1}`)}
	for i := 0; i < 2; i++ { // withdrawal, status
		if _, err := s.Next(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	e, err := s.Next(ctx)
	if err != nil && strings.Contains(err.Error(), "unknown callback") {
		t.Fatalf("a resolution receipt for a withdrawn callback failed the run: %v", err)
	}
	_ = e
}

// Claude's analogue, for the record: a cancel for an already-withdrawn id is
// benign.
func TestAdvW18ClaudeCancelAfterTurnEndWithdrawalIsBenign(t *testing.T) {
	s := w18aClaude(t)
	w18aClaudeAsk(t, s, 1)
	if _, ok, err := s.event([]byte(w18aClaudeEnd)); err != nil || !ok {
		t.Fatalf("end: %v %v", ok, err)
	}
	if _, ok, err := s.event([]byte(`{"type":"control_cancel_request","request_id":"r1"}`)); err != nil || ok {
		t.Fatalf("cancel after withdrawal: ok=%v err=%v", ok, err)
	}
}

// Multi-turn bookkeeping: after a Codex turn end with withdrawals is fully
// delivered, the session is no longer backlogged, the withdrawn ids stay
// refused, a replay of a withdrawn id is a duplicate (seen), and a fresh id in
// the next turn is admitted and answerable (not mistaken for withdrawn).
func TestAdvW18CodexNextTurnAfterWithdrawalKeepsBookkeepingSound(t *testing.T) {
	s := w18Codex(t)
	s.wire.stdin = &w18aRecorder{}
	old := w18aCodexOffer(t, s, 1)
	s.wire.frames <- wireRead{value: w18CodexEnd}
	if err := s.drain(); err != nil {
		t.Fatal(err)
	}
	if !s.backlogged() {
		t.Fatal("undelivered withdrawal must count as backlog")
	}
	for len(s.deferred) > 0 {
		if _, err := s.Next(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if s.backlogged() {
		t.Fatal("still backlogged after delivering the turn end")
	}
	// Next turn, as ContinueTurn leaves it.
	s.finished, s.turnID = false, "turn2"
	replay := w18CodexAsk(1)
	replay.Params = json.RawMessage(`{"threadId":"thread","turnId":"turn2","itemId":"i","command":"ls"}`)
	if _, _, err := s.event(replay); err == nil {
		t.Fatal("a replayed wire id was admitted in the next turn")
	}
	fresh := w18CodexAsk(2)
	fresh.Params = json.RawMessage(`{"threadId":"thread","turnId":"turn2","itemId":"i","command":"ls"}`)
	e, ok, err := s.event(fresh)
	if err != nil || !ok || e.Request == nil {
		t.Fatalf("fresh id in turn 2: ok=%v err=%v", ok, err)
	}
	if err := s.ValidateResponse(*e.Request, Response{Decision: "allow_once"}); err != nil {
		t.Fatalf("fresh callback refused: %v", err)
	}
	if err := s.ValidateResponse(old, Response{Decision: "allow_once"}); !errors.Is(err, ErrCallbackWithdrawn) {
		t.Fatalf("old = %v", err)
	}
}

// seen: ids the run refuses are not remembered, a refused id is not a poisoned
// one for the pending-cap case (it can be admitted once a slot frees), and
// pre-approved paths count against the same budget.
func TestAdvW18SeenRefusedForPendingCapIsNotRecordedAndMayBeRetried(t *testing.T) {
	s := w18Codex(t)
	var first Request
	for i := 1; i <= 32; i++ {
		r := w18aCodexOffer(t, s, i)
		if i == 1 {
			first = r
		}
	}
	if _, _, err := s.event(w18CodexAsk(500)); err == nil {
		t.Fatal("33rd admitted")
	}
	delete(s.pending, first.ID)
	if _, ok, err := s.event(w18CodexAsk(500)); err != nil || !ok {
		t.Fatalf("id refused only for the pending cap must be admissible later: ok=%v err=%v", ok, err)
	}
}

// Past the budget every adapter keeps refusing and seen never grows, including
// ids already seen (replays), at the 2047/2048/2049 edge.
func TestAdvW18SeenBudgetEdge(t *testing.T) {
	s := w18Codex(t)
	for i := 1; i <= 2049; i++ {
		_, _, err := s.event(w18CodexAsk(i))
		clear(s.pending)
		if (i <= 2048) != (err == nil) {
			t.Fatalf("id %d: err=%v", i, err)
		}
	}
	if len(s.seen) != 2048 || s.requestCount != 2048 {
		t.Fatalf("seen=%d count=%d", len(s.seen), s.requestCount)
	}
}

// Withdrawn ids past 2048 evict the oldest; with at most 2048 admissions per
// run nothing is ever evicted in practice. Prove the set is bounded and that
// the newest withdrawn id is always remembered.
func TestAdvW18WithdrawnSetBoundedAndKeepsNewest(t *testing.T) {
	var w withdrawnIDs
	for i := 0; i < 3000; i++ {
		w.withdrawalEvents([]string{fmt.Sprint(i)})
	}
	if len(w.set) != maxWithdrawnIDs || len(w.order) != maxWithdrawnIDs {
		t.Fatalf("set=%d order=%d", len(w.set), len(w.order))
	}
	if !w.has("2999") || w.has("0") {
		t.Fatal("eviction order wrong")
	}
}

// Concurrency: validating answers while Next consumes a turn end and the
// provider keeps writing. Run with -race.
func TestAdvW18CodexConcurrentValidateAndNextAroundTurnEnd(t *testing.T) {
	s := w18Codex(t)
	var reqs []Request
	for i := 1; i <= 32; i++ {
		reqs = append(reqs, w18aCodexOffer(t, s, i))
	}
	for i := 0; i < 60; i++ {
		s.wire.frames <- w18aCodexChunk()
	}
	s.wire.frames <- wireRead{value: w18CodexEnd}
	var wg sync.WaitGroup
	for _, r := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.ValidateResponse(r, Response{Decision: "allow_once"})
			if err != nil && !errors.Is(err, ErrCallbackWithdrawn) {
				t.Errorf("validate %s: %v", r.ID, err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		for {
			e, err := s.Next(ctx)
			if err != nil {
				t.Errorf("next: %v", err)
				return
			}
			if e.Status != "" {
				return
			}
		}
	}()
	wg.Wait()
}
