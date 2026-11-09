package agent

import (
	"context"
	"testing"
)

// l.effort is the tier of the request being assembled and rises inside a turn
// that is circling; Draft sizes the first request of a turn that has not started,
// which carries the configured tier (openingEffort), because a turn resets
// l.effort when it opens. After a turn that escalated, l.effort still holds the
// raised tier, so the two differ: the draft must equal the next request sent, not
// the leftover (ft-9a5710b6 item 2, effort).
func TestDraftCarriesTheTierTheNextTurnsFirstRequestSends(t *testing.T) {
	const searches = 2 + NoProgressLimit
	turns := append(churn("grep", searches), textTurn("done"), textTurn("second"))
	h := buildOn(t, localLadder(), turns, func(c *Config) {
		c.Effort, c.EffortCeiling, c.MaxSteps = "low", "high", 100
	})
	var ran int
	registerScripted(t, h, "grep", true, []string{"no matches found"}, &ran)

	out, err := h.loop.Run(context.Background(), userMessage("remove the unused imports"))
	if err != nil {
		t.Fatal(err)
	}
	if out.EffortRaised != 1 || h.loop.effort != "medium" {
		t.Fatalf("raised %d, loop effort %q; the first turn must leave the escalation behind", out.EffortRaised, h.loop.effort)
	}

	next := userMessage("second task")
	draft := h.loop.Draft(next)
	before := len(h.provider.Requests())
	if _, err := h.loop.Run(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	sent := h.provider.Requests()[before]
	if draft.Effort != sent.Effort || sent.Effort != "low" {
		t.Fatalf("draft effort %q, first request of the next turn %q, want both %q",
			draft.Effort, sent.Effort, "low")
	}
}
