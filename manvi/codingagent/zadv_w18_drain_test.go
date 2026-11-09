package codingagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The backlog bound is one number that counts a turn end's withdrawals and its
// status: the preflight's own 128 events plus the per-run pending cap (32) plus
// the status. These literals are deliberate: they pin the number independently of
// the constants, so changing the bound has to change a test.
const (
	w18DrainItems = 128
	w18Backlog    = 128 + 32 + 1
)

func w18Frame(method, params string) packet {
	return packet{Method: method, Params: json.RawMessage(params)}
}

func w18Chunk() wireRead {
	return wireRead{value: w18Frame("session/update", `{"sessionId":"S","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}`)}
}

// drainBacklog itself, at the exact edge: a backlog may reach the bound and may
// not pass it, whatever the events it parks were (here every item is one event).
func TestW18DrainBacklogBoundIsExactAtTheEdge(t *testing.T) {
	one := func(n int) (Event, bool, error) { return Event{Text: "x"}, true, nil }
	for _, c := range []struct {
		name    string
		prefill int
		wantErr bool
	}{
		{"bound-1 plus one event reaches the bound", w18Backlog - 1, false},
		{"at the bound, one more event is refused", w18Backlog, true},
		{"past the bound, one more event is refused", w18Backlog + 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			deferred := make([]Event, c.prefill)
			queue := []int{1}
			err := drainBacklog("Test", advWire(t, 1), &queue, &deferred, nil, one)
			if c.wantErr != (err != nil) {
				t.Fatalf("err=%v, wantErr=%v (backlog %d)", err, c.wantErr, len(deferred))
			}
			if c.wantErr && !strings.Contains(err.Error(), "event budget") {
				t.Fatalf("err=%v", err)
			}
			if !c.wantErr && len(deferred) != w18Backlog {
				t.Fatalf("backlog = %d, want %d", len(deferred), w18Backlog)
			}
		})
	}
}

// A turn end that releases the run, behind as many buffered events as one
// preflight can read, with the per-run maximum of callbacks pending, must not
// fail with "exceeded its event budget". The backlog it leaves is exactly
// chunks + 32 withdrawals + the status, and never passes the bound.
func TestW18AgyTurnEndBehindAFullBufferWithMaxPendingIsNotRefused(t *testing.T) {
	for chunks := 0; chunks < w18DrainItems; chunks++ {
		s := advAgyIdle(t)
		for i := 0; i < 32; i++ {
			advAgyOffer(t, s, fmt.Sprint(100+i))
		}
		s.deferred = []Event{}
		for i := 0; i < chunks; i++ {
			s.wire.frames <- w18Chunk()
		}
		s.wire.frames <- wireRead{value: advAgyEnd}
		if err := s.drain(); err != nil {
			t.Fatalf("chunks=%d: legitimate turn end refused: %v", chunks, err)
		}
		if got, want := len(s.deferred), chunks+32+1; got != want {
			t.Fatalf("chunks=%d: backlog %d, want %d", chunks, got, want)
		}
		if len(s.deferred) > w18Backlog {
			t.Fatalf("chunks=%d: backlog %d passes the bound %d", chunks, len(s.deferred), w18Backlog)
		}
		if n := len(s.deferred); n == 0 || s.deferred[n-1].Status == "" {
			t.Fatalf("chunks=%d: status is not last: %+v", chunks, s.deferred)
		}
	}
}

// The same for Claude: result frame behind buffered events with 32 pending.
func TestW18ClaudeTurnEndBehindAFullBufferWithMaxPendingIsNotRefused(t *testing.T) {
	for _, chunks := range []int{0, 50, 95, 96, 120, w18DrainItems - 1} {
		s := &Claude{sessionID: "S", started: true, turnID: "T", pending: map[string]*claudePending{}, seen: map[string]bool{}, wire: advWire(t, 512)}
		for i := 0; i < 32; i++ {
			e, ok, err := s.event([]byte(fmt.Sprintf(`{"type":"control_request","request_id":"r%d","session_id":"S","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`, i)))
			if err != nil || !ok || e.Request == nil {
				t.Fatalf("capture %d: ok=%v err=%v", i, ok, err)
			}
		}
		s.deferred = []Event{}
		for i := 0; i < chunks; i++ {
			s.wire.frames <- wireRead{raw: []byte(`{"type":"user","session_id":"S","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]}}`)}
		}
		s.wire.frames <- wireRead{raw: []byte(`{"type":"result","subtype":"success","session_id":"S","result":"done"}`)}
		if err := s.drain(); err != nil {
			t.Fatalf("chunks=%d: legitimate turn end refused: %v", chunks, err)
		}
		if got, want := len(s.deferred), chunks+32+1; got != want {
			t.Fatalf("chunks=%d: backlog %d, want %d", chunks, got, want)
		}
	}
}

// Past the preflight's 128 items the drain stops reading and reports it could
// not drain: the documented cap, kept, because an unbounded read of a peer's
// buffered output is what the bound protects against.
func TestW18AgyPreflightReadingMoreThanItsItemBudgetFailsClosed(t *testing.T) {
	s := advAgyIdle(t)
	s.deferred = []Event{}
	for i := 0; i < w18DrainItems+1; i++ {
		s.wire.frames <- w18Chunk()
	}
	err := s.drain()
	if err == nil || !strings.Contains(err.Error(), "could not drain") {
		t.Fatalf("err = %v", err)
	}
	if len(s.deferred) > w18Backlog {
		t.Fatalf("backlog %d passes the bound", len(s.deferred))
	}
}

type w18Sink struct{}

func (w18Sink) Write(p []byte) (int, error) { return len(p), nil }
func (w18Sink) Close() error                { return nil }

// ---- seen -------------------------------------------------------------------

// `seen` gains an id only where requestCount does, after the budget check, so a
// flood of distinct ids can never grow it past the run budget: the refused ones
// are not recorded. Each adapter is flooded with 2.5x the budget, answering (or
// forgetting) each so the pending cap is not what stops it.
func TestW18SeenIsBoundedByTheRunBudget(t *testing.T) {
	const flood = 2048 * 5 / 2
	t.Run("agy", func(t *testing.T) {
		s := advAgyIdle(t)
		refused := 0
		for i := 0; i < flood; i++ {
			if _, _, err := s.event(advAgyPerm(fmt.Sprint(i))); err != nil {
				refused++
			}
			clear(s.pending)
		}
		if refused != flood-2048 || len(s.seen) != 2048 || s.requestCount != 2048 {
			t.Fatalf("refused=%d seen=%d requestCount=%d", refused, len(s.seen), s.requestCount)
		}
	})
	t.Run("agy preapproved", func(t *testing.T) {
		s := advAgyIdle(t)
		s.options.Mode = "preapproved"
		s.wire = advWire(t, 1)
		s.wire.stdin = w18Sink{}
		for i := 0; i < flood; i++ {
			_, _, _ = s.event(advAgyPerm(fmt.Sprint(i)))
		}
		if len(s.seen) != 2048 {
			t.Fatalf("seen=%d", len(s.seen))
		}
	})
	t.Run("claude", func(t *testing.T) {
		s := &Claude{sessionID: "S", started: true, turnID: "T", pending: map[string]*claudePending{}, seen: map[string]bool{}, wire: advWire(t, 1)}
		for i := 0; i < flood; i++ {
			_, _, _ = s.event([]byte(fmt.Sprintf(`{"type":"control_request","request_id":"r%d","session_id":"S","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`, i)))
			clear(s.pending)
		}
		if len(s.seen) != claudeMaxRequests {
			t.Fatalf("seen=%d, want %d", len(s.seen), claudeMaxRequests)
		}
	})
	t.Run("codex", func(t *testing.T) {
		s := w18Codex(t)
		for i := 0; i < flood; i++ {
			_, _, _ = s.event(w18CodexAsk(i))
			clear(s.pending)
		}
		if len(s.seen) != 2048 {
			t.Fatalf("seen=%d", len(s.seen))
		}
	})
}

// ---- Codex withdrawals ------------------------------------------------------

func w18Codex(t *testing.T) *Codex {
	t.Helper()
	s := &Codex{started: true, turnID: "turn", items: map[string]json.RawMessage{}, pending: map[string]*pendingRequest{}, seen: map[string]bool{}, wire: advWire(t, 512)}
	s.effective.Thread.ID = "thread"
	return s
}

func w18CodexAsk(i int) packet {
	return packet{ID: json.RawMessage(fmt.Sprint(i)), Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","itemId":"i","command":"ls"}`)}
}

var w18CodexEnd = w18Frame("turn/completed", `{"threadId":"thread","turn":{"id":"turn","status":"completed"}}`)

// A turn end withdraws every callback still awaiting an answer, ahead of the
// status, as agy and Claude do; an answer that races it is told it was
// withdrawn, not that it was stale.
func TestW18CodexTurnEndWithdrawsPendingAheadOfTheStatus(t *testing.T) {
	s := w18Codex(t)
	var reqs []Request
	for i := 1; i <= 3; i++ {
		e, ok, err := s.event(w18CodexAsk(i))
		if err != nil || !ok || e.Request == nil {
			t.Fatalf("capture %d: ok=%v err=%v", i, ok, err)
		}
		reqs = append(reqs, *e.Request)
	}
	e, ok, err := s.event(w18CodexEnd)
	if err != nil || !ok {
		t.Fatalf("turn end: ok=%v err=%v", ok, err)
	}
	got := []Event{e}
	got = append(got, s.deferred...)
	if len(got) != 4 || got[3].Status != "completed" {
		t.Fatalf("events = %+v", got)
	}
	for i, r := range reqs {
		if got[i].CancelledID != r.ID {
			t.Fatalf("event %d = %+v, want withdrawal of %s", i, got[i], r.ID)
		}
		if err := s.ValidateResponse(r, Response{Decision: "allow_once"}); !errors.Is(err, ErrCallbackWithdrawn) {
			t.Fatalf("answer to %s = %v, want ErrCallbackWithdrawn", r.ID, err)
		}
	}
}

// A Codex turn end behind a full buffer with the maximum pending is not refused.
func TestW18CodexTurnEndBehindAFullBufferWithMaxPendingIsNotRefused(t *testing.T) {
	s := w18Codex(t)
	for i := 1; i <= 32; i++ {
		if _, ok, err := s.event(w18CodexAsk(i)); err != nil || !ok {
			t.Fatalf("capture %d: ok=%v err=%v", i, ok, err)
		}
	}
	s.deferred = []Event{}
	for i := 0; i < w18DrainItems-1; i++ {
		s.wire.frames <- wireRead{value: w18Frame("item/commandExecution/outputDelta", `{"threadId":"thread","turnId":"turn","delta":"x"}`)}
	}
	s.wire.frames <- wireRead{value: w18CodexEnd}
	if err := s.drain(); err != nil {
		t.Fatalf("legitimate turn end refused: %v", err)
	}
	if got, want := len(s.deferred), w18DrainItems-1+32+1; got != want {
		t.Fatalf("backlog %d, want %d", got, want)
	}
}
