package codingagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// failingWriter stands in for a provider's stdin that breaks mid-write: what the
// adapter sees from a pipe that errors is exactly what it sees when a frame may
// have been half-delivered, and it cannot tell the two apart.
type failingWriter struct{ writes atomic.Int32 }

func (f *failingWriter) Write([]byte) (int, error) {
	f.writes.Add(1)
	return 0, errors.New("broken pipe")
}
func (f *failingWriter) Close() error { return nil }

func requireRefused(t *testing.T, err error, want error) {
	t.Helper()
	if err == nil || !errors.Is(err, want) || !ContinueRefused(err) || DeliveryUnresolved(err) {
		t.Fatalf("got %v (refused=%t unresolved=%t), want a refusal %q", err, ContinueRefused(err), DeliveryUnresolved(err), want)
	}
}

func TestContinuableProvidersAreTheManagedProviders(t *testing.T) {
	for _, provider := range ManagedProviders {
		if !SupportsContinue(provider) {
			t.Fatalf("managed provider %q has no ContinueTurn", provider)
		}
	}
	if SupportsContinue("gemini") || SupportsContinue("") {
		t.Fatal("a provider with no adapter claimed to take follow-ups")
	}
}

func TestClaudeContinueTurnRunsASecondTurnOnTheSameSession(t *testing.T) {
	s := openFixture(t, "multiturn", "ask")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	text, status, _ := collect(t, s)
	if status != "completed" || !strings.Contains(text, "TURN_1") || s.turnID != "msg_t1" {
		t.Fatalf("first turn: %q %q %q", text, status, s.turnID)
	}
	session := s.Effective().Thread.ID
	if err := s.ContinueTurn(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	text, status, _ = collect(t, s)
	if status != "completed" || !strings.Contains(text, "TURN_2") || strings.Contains(text, "TURN_1") {
		t.Fatalf("second turn: %q %q", text, status)
	}
	if s.turnID != "msg_t2" || s.Effective().Thread.ID != session || s.ledger.turns != 2 {
		t.Fatalf("second turn identity: turn %q session %q->%q turns %d", s.turnID, session, s.Effective().Thread.ID, s.ledger.turns)
	}
}

func TestClaudeContinueTurnRefusesEveryPreconditionWithoutWriting(t *testing.T) {
	s := openFixture(t, "multiturn", "ask")
	requireRefused(t, s.ContinueTurn(t.Context(), "too early"), ErrNoCompletedTurn)
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	// The turn is running until Next has delivered its result.
	requireRefused(t, s.ContinueTurn(t.Context(), "while running"), ErrTurnInFlight)
	collect(t, s)
	for name, message := range map[string]string{
		"blank":     "  \n\t",
		"oversize":  strings.Repeat("x", MaxContinueMessageBytes+1),
		"bad utf-8": "ok\xff",
		"nul":       "a\x00b",
	} {
		t.Run(name, func(t *testing.T) {
			requireRefused(t, s.ContinueTurn(t.Context(), message), ErrContinueMessage)
		})
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	if err := s.ContinueTurn(canceled, "late"); !ContinueRefused(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled context was not a refusal: %v", err)
	}
	// None of the refusals reached the child: its next turn is still its second.
	if err := s.ContinueTurn(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	if text, _, _ := collect(t, s); !strings.Contains(text, "TURN_2") {
		t.Fatalf("a refused follow-up reached the provider: %q", text)
	}
}

func TestClaudeContinueTurnRefusesWhileAReadIsBlocked(t *testing.T) {
	s := openFixture(t, "silent", "ask")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	read := make(chan struct{})
	go func() {
		defer close(read)
		_, _ = s.Next(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	requireRefused(t, s.ContinueTurn(t.Context(), "while the turn is read"), ErrTurnInFlight)
	stop()
	<-read
}

func TestClaudeContinueTurnRefusesAfterCloseAndAfterTheProcessIsGone(t *testing.T) {
	closed := openFixture(t, "multiturn", "ask")
	if err := closed.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collect(t, closed)
	if _, err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	requireRefused(t, closed.ContinueTurn(t.Context(), "after close"), ErrSessionClosed)

	gone := openFixture(t, "one-shot", "ask")
	if err := gone.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collect(t, gone)
	select {
	case <-gone.wire.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the fixture process did not exit")
	}
	requireRefused(t, gone.ContinueTurn(t.Context(), "to nobody"), ErrSessionClosed)
}

func TestClaudeContinueTurnIsOnlyForACompletedTurn(t *testing.T) {
	s := openFixture(t, "interrupted", "ask")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := collect(t, s); status != "interrupted" {
		t.Fatalf("status %q", status)
	}
	requireRefused(t, s.ContinueTurn(t.Context(), "after an interrupted turn"), ErrNoCompletedTurn)
}

func TestClaudeContinueTurnStopsAtItsTurnBound(t *testing.T) {
	s := openFixture(t, "multiturn", "ask")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	for i := 1; i <= MaxContinuations; i++ {
		if err := s.ContinueTurn(t.Context(), fmt.Sprintf("follow-up %d", i)); err != nil {
			t.Fatalf("follow-up %d: %v", i, err)
		}
		if text, status, _ := collect(t, s); status != "completed" || !strings.Contains(text, fmt.Sprintf("TURN_%d", i+1)) {
			t.Fatalf("follow-up %d: %q %q", i, text, status)
		}
	}
	requireRefused(t, s.ContinueTurn(t.Context(), "one too many"), ErrContinueLimit)
}

func TestClaudeAmbiguousFollowUpIsNeverSentAgain(t *testing.T) {
	s := openFixture(t, "multiturn", "ask")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	broken := &failingWriter{}
	s.wire.stdin = broken
	err := s.ContinueTurn(t.Context(), "maybe delivered")
	if err == nil || ContinueRefused(err) || !DeliveryUnresolved(err) {
		t.Fatalf("a failed write was not reported as unresolved: %v", err)
	}
	// Any later offer, the same text included, is refused and writes nothing.
	requireRefused(t, s.ContinueTurn(t.Context(), "maybe delivered"), ErrSessionClosed)
	if broken.writes.Load() != 1 {
		t.Fatalf("the follow-up was written %d times", broken.writes.Load())
	}
	if !s.ledger.halted {
		t.Fatal("an ambiguous delivery did not halt the session")
	}
}

func TestClaudeHaltedSessionRefusesEvenWhileItsConnectionLives(t *testing.T) {
	s := openFixture(t, "multiturn", "ask")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	s.ledger.halted = true
	requireRefused(t, s.ContinueTurn(t.Context(), "after an unresolved one"), ErrContinuationHalted)
}

func TestClaudeSecondTurnMustKeepTheModelTheRunRecorded(t *testing.T) {
	s := openFixture(t, "model-drift", "ask")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	if err := s.ContinueTurn(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	for {
		_, err := s.Next(ctx)
		if err != nil {
			if !strings.Contains(err.Error(), "changed model") {
				t.Fatalf("unexpected error: %v", err)
			}
			return
		}
	}
}

func TestClaudeSecondInitInsideOneTurnIsStillAFault(t *testing.T) {
	// The reinit scenario sends two inits for one user frame.
	s := openFixture(t, "reinit", "ask")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	_, err := s.Next(ctx)
	if err == nil || !strings.Contains(err.Error(), "re-initialized") {
		t.Fatalf("a second init in one turn was accepted: %v", err)
	}
}

func collectCodex(t *testing.T, s *Codex) (string, string) {
	t.Helper()
	text := strings.Builder{}
	ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	for {
		e, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		text.WriteString(e.Text)
		if e.Status != "" {
			return text.String(), e.Status
		}
	}
}

func TestCodexContinueTurnRunsASecondTurnOnTheSameThread(t *testing.T) {
	for _, scenario := range []string{"multiturn", "multiturn-early"} {
		t.Run(scenario, func(t *testing.T) {
			s := fixture(t, scenario)
			if err := s.StartTurn(t.Context(), "first"); err != nil {
				t.Fatal(err)
			}
			if text, status := collectCodex(t, s); status != "completed" || !strings.Contains(text, "REPLY_turn-1") {
				t.Fatalf("first turn: %q %q", text, status)
			}
			if err := s.ContinueTurn(t.Context(), "second"); err != nil {
				t.Fatal(err)
			}
			text, status := collectCodex(t, s)
			if status != "completed" || !strings.Contains(text, "REPLY_turn-2") || strings.Contains(text, "REPLY_turn-1") {
				t.Fatalf("second turn: %q %q", text, status)
			}
			if s.turnID != "turn-2" || s.effective.Thread.ID != "thread" || s.ledger.turns != 2 {
				t.Fatalf("second turn identity: turn %q thread %q turns %d", s.turnID, s.effective.Thread.ID, s.ledger.turns)
			}
		})
	}
}

func TestCodexContinueTurnRefusesEveryPreconditionWithoutWriting(t *testing.T) {
	s := fixture(t, "multiturn")
	requireRefused(t, s.ContinueTurn(t.Context(), "too early"), ErrNoCompletedTurn)
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	requireRefused(t, s.ContinueTurn(t.Context(), "while running"), ErrTurnInFlight)
	collectCodex(t, s)
	for name, message := range map[string]string{
		"blank":    " ",
		"oversize": strings.Repeat("x", MaxContinueMessageBytes+1),
		"nul":      "a\x00b",
	} {
		t.Run(name, func(t *testing.T) {
			requireRefused(t, s.ContinueTurn(t.Context(), message), ErrContinueMessage)
		})
	}
	// Nothing above reached the child: its next turn/start is its second.
	if err := s.ContinueTurn(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	if text, _ := collectCodex(t, s); !strings.Contains(text, "REPLY_turn-2") {
		t.Fatalf("a refused follow-up reached the provider: %q", text)
	}
}

func TestCodexContinueTurnRefusesWhileATurnIsInFlight(t *testing.T) {
	s := fixture(t, "silent")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	requireRefused(t, s.ContinueTurn(t.Context(), "second"), ErrTurnInFlight)
}

func TestCodexContinueTurnRefusesAfterClose(t *testing.T) {
	s := fixture(t, "multiturn")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collectCodex(t, s)
	if _, err := s.Close(); err != nil {
		t.Fatal(err)
	}
	requireRefused(t, s.ContinueTurn(t.Context(), "after close"), ErrSessionClosed)
}

func TestCodexContinueTurnStopsAtItsTurnBound(t *testing.T) {
	s := fixture(t, "multiturn")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collectCodex(t, s)
	for i := 1; i <= MaxContinuations; i++ {
		if err := s.ContinueTurn(t.Context(), fmt.Sprintf("follow-up %d", i)); err != nil {
			t.Fatalf("follow-up %d: %v", i, err)
		}
		if text, status := collectCodex(t, s); status != "completed" || !strings.Contains(text, fmt.Sprintf("REPLY_turn-%d", i+1)) {
			t.Fatalf("follow-up %d: %q %q", i, text, status)
		}
	}
	requireRefused(t, s.ContinueTurn(t.Context(), "one too many"), ErrContinueLimit)
}

// A JSON-RPC error is the provider's answer that it started nothing, so the
// session is between turns again and the same follow-up may be offered once
// more; a failure to get any answer is not that.
func TestCodexProviderRejectionIsARefusalAndTheSessionRecovers(t *testing.T) {
	s := fixture(t, "reject-second")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collectCodex(t, s)
	err := s.ContinueTurn(t.Context(), "second")
	requireRefused(t, err, errProviderRejected)
	if s.ledger.turns != 1 || !s.finished || s.ledger.halted {
		t.Fatalf("a rejected follow-up left the session mid-turn: turns %d finished %t halted %t", s.ledger.turns, s.finished, s.ledger.halted)
	}
	if err := s.ContinueTurn(t.Context(), "second, again"); err != nil {
		t.Fatal(err)
	}
	if text, status := collectCodex(t, s); status != "completed" || !strings.Contains(text, "REPLY_turn-3") {
		t.Fatalf("the retry after a rejection: %q %q", text, status)
	}
}

func TestCodexAmbiguousFollowUpIsNeverSentAgain(t *testing.T) {
	s := fixture(t, "multiturn")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collectCodex(t, s)
	broken := &failingWriter{}
	s.wire.stdin = broken
	err := s.ContinueTurn(t.Context(), "maybe delivered")
	if err == nil || ContinueRefused(err) || !DeliveryUnresolved(err) {
		t.Fatalf("a failed write was not reported as unresolved: %v", err)
	}
	requireRefused(t, s.ContinueTurn(t.Context(), "maybe delivered"), ErrSessionClosed)
	if broken.writes.Load() != 1 || !s.ledger.halted {
		t.Fatalf("writes %d halted %t", broken.writes.Load(), s.ledger.halted)
	}
}

func TestCodexHaltedSessionRefusesEvenWhileItsConnectionLives(t *testing.T) {
	s := fixture(t, "multiturn")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collectCodex(t, s)
	s.ledger.halted = true
	requireRefused(t, s.ContinueTurn(t.Context(), "after an unresolved one"), ErrContinuationHalted)
}

// An approval raised in a follow-up turn is bound to that turn, the thread or
// session, its provider request id and its payload digest, exactly as one in
// the first turn is: a copy that names the finished turn, or whose payload
// changed, is not the callback the provider asked, and an answer is single-use.
func TestClaudeFollowUpTurnApprovalIsBoundToTheFollowUpTurn(t *testing.T) {
	s := openFixture(t, "multiturn-permission", "ask")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := collect(t, s); status != "completed" {
		t.Fatalf("first turn: %q", status)
	}
	if err := s.ContinueTurn(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	_, _, request := collect(t, s)
	if request == nil || request.TurnID != "msg_t2" || request.ThreadID != s.Effective().Thread.ID || request.ID != "req-2" {
		t.Fatalf("the follow-up turn's approval: %+v", request)
	}
	allow := Response{Decision: "allow_once"}
	stale := *request
	stale.TurnID = "msg_t1"
	tampered := *request
	tampered.Payload += " "
	for name, bad := range map[string]Request{"finished turn": stale, "changed payload": tampered} {
		if err := s.ValidateResponse(bad, allow); err == nil {
			t.Fatalf("a %s copy of the approval was accepted", name)
		}
		if err := s.Respond(t.Context(), bad, allow); err == nil {
			t.Fatalf("a %s copy of the approval was answered", name)
		}
	}
	if err := s.Respond(t.Context(), *request, allow); err != nil {
		t.Fatal(err)
	}
	if err := s.Respond(t.Context(), *request, allow); err == nil {
		t.Fatal("an approval was answered twice")
	}
	if _, status, _ := collect(t, s); status != "completed" {
		t.Fatalf("second turn after the answer: %q", status)
	}
}

func TestCodexFollowUpTurnApprovalIsBoundToTheFollowUpTurn(t *testing.T) {
	s := fixture(t, "multiturn-callback")
	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collectCodex(t, s)
	if err := s.ContinueTurn(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	var request *Request
	for request == nil {
		e, err := s.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		request = e.Request
	}
	if request.TurnID != "turn-2" || request.ThreadID != "thread" {
		t.Fatalf("the follow-up turn's approval: %+v", request)
	}
	allow := Response{Decision: "allow_once"}
	stale := *request
	stale.TurnID = "turn-1"
	tampered := *request
	tampered.Payload += " "
	for name, bad := range map[string]Request{"finished turn": stale, "changed payload": tampered} {
		if err := s.ValidateResponse(bad, allow); err == nil {
			t.Fatalf("a %s copy of the approval was accepted", name)
		}
	}
	if err := s.Respond(t.Context(), *request, allow); err != nil {
		t.Fatal(err)
	}
	if err := s.Respond(t.Context(), *request, allow); err == nil {
		t.Fatal("an approval was answered twice")
	}
}

// A callback of the finished turn that was never answered blocks the follow-up:
// it could otherwise be answered into the new turn.
func TestContinueTurnRefusesWhileAnApprovalOfTheLastTurnIsOpen(t *testing.T) {
	t.Run("codex", func(t *testing.T) {
		s := fixture(t, "multiturn")
		if err := s.StartTurn(t.Context(), "first"); err != nil {
			t.Fatal(err)
		}
		collectCodex(t, s)
		s.pending["a"] = &pendingRequest{source: Request{ID: "a"}}
		requireRefused(t, s.ContinueTurn(t.Context(), "second"), ErrTurnInFlight)
		s.pending["a"].sent = true // answered: no longer blocks
		if err := s.ContinueTurn(t.Context(), "second"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("claude", func(t *testing.T) {
		s := openFixture(t, "multiturn", "ask")
		if err := s.StartTurn(t.Context(), "first"); err != nil {
			t.Fatal(err)
		}
		collect(t, s)
		s.pending["a"] = &claudePending{source: Request{ID: "a"}, requestID: "a"}
		requireRefused(t, s.ContinueTurn(t.Context(), "second"), ErrTurnInFlight)
		s.pending["a"].sent = true
		answered := s.pending["a"].source
		if err := s.ContinueTurn(t.Context(), "second"); err != nil {
			t.Fatal(err)
		}
		// An answered callback no longer counts against the pending cap, and a
		// late answer to it is stale rather than delivered.
		if len(s.pending) != 0 {
			t.Fatalf("answered callbacks stayed pending: %d", len(s.pending))
		}
		if err := s.Respond(t.Context(), answered, Response{Decision: "allow_once"}); err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("a late answer to a finished turn's callback: %v", err)
		}
	})
}

// Both adapters keep their one-turn StartTurn rule; a follow-up is a different
// call, not a second StartTurn.
func TestStartTurnStillTakesOneTurnPerSession(t *testing.T) {
	c := openFixture(t, "multiturn", "ask")
	if err := c.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartTurn(t.Context(), "second"); err == nil {
		t.Fatal("Claude StartTurn took a second turn")
	}
	x := fixture(t, "multiturn")
	if err := x.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	if err := x.StartTurn(t.Context(), "second"); err == nil {
		t.Fatal("Codex StartTurn took a second turn")
	}
}
