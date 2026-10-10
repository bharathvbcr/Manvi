package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

func boundaryCounts(log *session.Log) (turnStart, turnEnd, stepStart, stepEnd int) {
	for _, e := range log.Events() {
		switch e.Type {
		case session.TurnStart:
			turnStart++
		case session.TurnEnd:
			turnEnd++
		case session.StepStart:
			stepStart++
		case session.StepEnd:
			stepEnd++
		}
	}
	return
}

func requireBalanced(t *testing.T, log *session.Log) {
	t.Helper()
	ts, te, ss, se := boundaryCounts(log)
	if ts != te || ss != se {
		t.Fatalf("unbalanced log: turn start/end %d/%d, step start/end %d/%d", ts, te, ss, se)
	}
}

func goPrompt() llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}}
}

func TestAProviderErrorStillClosesTheTurnAndStep(t *testing.T) {
	loop, log := loopOver(t, []replay.Turn{{Err: "boom"}, answerTurn("second turn works")})
	_, err := loop.Run(context.Background(), goPrompt())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Run must return the original provider error, got %v", err)
	}
	requireBalanced(t, log)

	// The same log is usable afterwards, and stays balanced.
	if _, err := loop.Run(context.Background(), goPrompt()); err != nil {
		t.Fatalf("second Run on the same log: %v", err)
	}
	requireBalanced(t, log)
}

func TestACancelledTurnStillClosesTheTurnAndStep(t *testing.T) {
	loop, log := loopOver(t, []replay.Turn{answerTurn("never delivered")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := loop.Run(ctx, goPrompt())
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run must return the cancellation, got %v", err)
	}
	requireBalanced(t, log)
}

func TestAPreStepRejectClosesTheTurnWithoutAStepEnd(t *testing.T) {
	loop, log := loopOver(t, []replay.Turn{answerTurn("x")})
	if _, err := bus.OnWaterfall(loop.bus, func(e PreStep, next bus.Next[PreStep]) PreStep {
		e.Reject = errors.New("no")
		return next(e)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), goPrompt()); err == nil || !strings.Contains(err.Error(), "no") {
		t.Fatalf("want the reject, got %v", err)
	}
	ts, te, ss, se := boundaryCounts(log)
	if ts != 1 || te != 1 || ss != 0 || se != 0 {
		t.Fatalf("reject before any step opened: turn %d/%d step %d/%d", ts, te, ss, se)
	}
}

func TestNewLoopRefusesANilBus(t *testing.T) {
	loop, _ := loopOver(t, nil)
	if _, err := NewLoop(loop.cfg, nil, session.NewLog(), loop.tools); err == nil {
		t.Fatal("NewLoop accepted a nil bus, which Run would panic on")
	}
}
