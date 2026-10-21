package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// overflowMessages is a history with one tool result whose body is resultChars
// long. With protected false the result sits outside ProtectedTail, so
// compaction may shorten it; with protected true it sits inside, so compaction
// has nothing eligible.
func overflowMessages(resultChars int, protected bool) []llm.Message {
	text := func(role llm.Role, s string) llm.Message {
		return llm.Message{Role: role, Content: []llm.ContentBlock{llm.TextBlock{Text: s}}}
	}
	body := strings.Repeat("a matching line of source\n", resultChars/26+1)[:resultChars]
	msgs := []llm.Message{
		text(llm.RoleUser, "start"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCallBlock{ID: "c1", Name: "Grep", Arguments: []byte(`{}`)},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultBlock{
			ToolCallID: "c1",
			Content:    []llm.ContentBlock{llm.TextBlock{Text: body}},
		}}},
	}
	if protected {
		return msgs
	}
	for i := 0; i < ProtectedTail+1; i++ {
		role := llm.RoleAssistant
		if i%2 == 1 {
			role = llm.RoleUser
		}
		msgs = append(msgs, text(role, "ok"))
	}
	return msgs
}

// overflowLoop builds a loop on a window small enough that the threshold is the
// minimum, so sysChars of system prompt decides whether compaction can win.
func overflowLoop(t *testing.T, sysChars int, turns []replay.Turn) *harness {
	t.Helper()
	c := caps("test-model")
	c.ContextWindow = 8192
	c.MaxOutputTokens = 256
	return buildOn(t, c, turns, func(cfg *Config) {
		cfg.MaxTokens = 0
		cfg.SystemPrompt = strings.Repeat("system instructions. ", sysChars/21+1)
	})
}

func countEvents(h *harness, typ session.Type) int {
	n := 0
	for _, e := range h.log.Events() {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// hugeSystem alone exceeds the minimum threshold, so nothing compaction does
// can make the history fit.
const hugeSystem = 60000

// An insufficient compaction sets the flag whether or not the plan has steps.
// The flag and the context/overflow event describe the same fact, so they must
// agree; before the fix only the empty-plan branch set the flag.
func TestInsufficientCompactionSetsTheFlag(t *testing.T) {
	t.Run("empty plan", func(t *testing.T) {
		h := overflowLoop(t, hugeSystem, nil)
		if _, err := h.loop.compact(overflowMessages(40000, true)); err != nil {
			t.Fatal(err)
		}
		if countEvents(h, session.ToolResultCompacted) != 0 {
			t.Fatal("fixture error: the plan was expected to be empty")
		}
		if countEvents(h, session.ContextOverflow) != 1 || !h.loop.overflowed {
			t.Fatalf("events=%d flag=%v, want 1 event and the flag set",
				countEvents(h, session.ContextOverflow), h.loop.overflowed)
		}
	})

	t.Run("non-empty plan", func(t *testing.T) {
		h := overflowLoop(t, hugeSystem, nil)
		if _, err := h.loop.compact(overflowMessages(40000, false)); err != nil {
			t.Fatal(err)
		}
		if countEvents(h, session.ToolResultCompacted) == 0 {
			t.Fatal("fixture error: the plan was expected to have steps")
		}
		if countEvents(h, session.ContextOverflow) != 1 {
			t.Fatalf("overflow events = %d, want 1", countEvents(h, session.ContextOverflow))
		}
		if !h.loop.overflowed {
			t.Fatal("overflow was reported in the log but the flag stayed false: " +
				"the turn did not fit and callers treat it as one that did")
		}
	})

	t.Run("non-empty plan that is sufficient stays quiet", func(t *testing.T) {
		h := overflowLoop(t, 100, nil)
		if _, err := h.loop.compact(overflowMessages(80000, false)); err != nil {
			t.Fatal(err)
		}
		if countEvents(h, session.ToolResultCompacted) == 0 {
			t.Fatal("fixture error: the plan was expected to have steps")
		}
		if countEvents(h, session.ContextOverflow) != 0 || h.loop.overflowed {
			t.Fatalf("events=%d flag=%v, want a quiet turn once compaction made it fit",
				countEvents(h, session.ContextOverflow), h.loop.overflowed)
		}
	})

	t.Run("non-empty plan that cannot shorten anything is the empty case", func(t *testing.T) {
		// A result below every floor is skipped, so the plan has no steps even
		// though a candidate existed.
		h := overflowLoop(t, hugeSystem, nil)
		if _, err := h.loop.compact(overflowMessages(100, false)); err != nil {
			t.Fatal(err)
		}
		if countEvents(h, session.ToolResultCompacted) != 0 {
			t.Fatal("fixture error: nothing should have been compacted")
		}
		if !h.loop.overflowed || countEvents(h, session.ContextOverflow) != 1 {
			t.Fatalf("flag=%v events=%d, want both set", h.loop.overflowed,
				countEvents(h, session.ContextOverflow))
		}
	})
}

// A turn flagged on an early step stays flagged. The flag means "history did
// not fit at some point in this turn" -- which is what the outcome notice says
// and what the context/overflow event already recorded -- so a later step that
// fits does not unsay it. Only a new turn clears it.
func TestFlagSurvivesALaterStepThatFitsButNotALaterTurn(t *testing.T) {
	h := overflowLoop(t, 100, []replay.Turn{textTurn("done")})

	if _, err := h.loop.compact(overflowMessages(200000, true)); err != nil { // does not fit
		t.Fatal(err)
	}
	if !h.loop.overflowed {
		t.Fatal("fixture error: step 1 should have overflowed")
	}
	if _, err := h.loop.compact(overflowMessages(100, true)); err != nil { // fits
		t.Fatal(err)
	}
	if !h.loop.overflowed {
		t.Fatal("a later step that fit cleared the flag; the turn still overflowed earlier")
	}

	out, err := h.loop.Run(context.Background(), userMsg("short"))
	if err != nil {
		t.Fatal(err)
	}
	if out.ContextOverflowed {
		t.Fatal("a later turn that fits inherited the flag: openTurn must clear it")
	}
}

// Every step of a bounced turn re-runs compaction; each overflowing step
// appends an event and the outcome stays flagged, so the outcome and the log
// cannot disagree about whether this turn overflowed.
func TestBouncedTurnReportsOverflowOnOutcomeAndLog(t *testing.T) {
	h := overflowLoop(t, hugeSystem, []replay.Turn{textTurn("first"), textTurn("second")})
	asked := 0
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
		asked++
		if asked == 1 {
			e.Inject = "keep going"
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Steps != 2 {
		t.Fatalf("steps = %d, want 2 (one bounce)", out.Steps)
	}
	if !out.ContextOverflowed || countEvents(h, session.ContextOverflow) != 2 {
		t.Fatalf("flag=%v events=%d, want flag set and one event per overflowing step",
			out.ContextOverflowed, countEvents(h, session.ContextOverflow))
	}
}

// A cancelled turn must not leave the flag behind for the next one.
func TestCancelledOverflowingTurnDoesNotPoisonTheNext(t *testing.T) {
	h := overflowLoop(t, 100, []replay.Turn{textTurn("done")})
	if _, err := h.loop.compact(overflowMessages(200000, true)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = h.loop.Run(ctx, userMsg("cancelled"))

	out, err := h.loop.Run(context.Background(), userMsg("short"))
	if err != nil {
		t.Fatal(err)
	}
	if out.ContextOverflowed {
		t.Fatal("flag survived a cancelled turn into the next one")
	}
}
