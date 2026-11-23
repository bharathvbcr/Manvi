package agent

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
)

func pausedTurn(text string) replay.Turn {
	turn := answerTurn(text)
	turn.StopReason = llm.StopPause
	turn.Usage.WebSearchRequests = 1
	return turn
}

// A turn the provider paused inside a hosted tool is not over: the same history
// goes back and the provider continues it. Each continuation is its own step.
func TestAPausedHostedToolTurnIsContinuedNotEnded(t *testing.T) {
	loop, _ := loopOver(t, []replay.Turn{pausedTurn("searching"), pausedTurn("still searching"), answerTurn("the answer")})
	out := run(t, loop)
	if out.Steps != 3 || out.Final.Text() != "the answer" || out.StopReason != llm.StopEndTurn {
		t.Fatalf("a paused turn ended the run: steps=%d stop=%q final=%q", out.Steps, out.StopReason, out.Final.Text())
	}
	if out.Usage.WebSearchRequests != 2 {
		t.Errorf("billed searches in the outcome = %d, want 2", out.Usage.WebSearchRequests)
	}
}

// A provider that pauses forever must not hold the turn open.
func TestPausesAreBounded(t *testing.T) {
	turns := make([]replay.Turn, 0, maxPauses+3)
	for i := 0; i < maxPauses+3; i++ {
		turns = append(turns, pausedTurn("again"))
	}
	loop, _ := loopOver(t, turns)
	out := run(t, loop)
	if out.StopReason != llm.StopPause || out.Steps != maxPauses+1 {
		t.Fatalf("unbounded continuation: steps=%d stop=%q (bound %d)", out.Steps, out.StopReason, maxPauses)
	}
}
