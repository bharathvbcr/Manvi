package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
)

// TurnEnded closes the scope TurnStarting opens, on every way out of Run.

// ToolsDispatching is what lets a listener start work at TurnStarting and still
// guarantee it finished before the turn changed anything. Both halves of that
// guarantee are pinned here: the event precedes the tool, and a listener that
// cannot hold the line stops the tool from running.

func TestToolsDispatchingPrecedesEveryResponseThatCarriesCalls(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("c1", "probe", `{}`),
		toolTurn("c2", "probe", `{"again":true}`),
		textTurn("done"),
	}, nil)

	var order []string
	ran := 0
	registerCounter(t, h, "probe", &ran)
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *ToolsDispatching) error {
		order = append(order, "dispatching")
		if e.Calls != 1 {
			t.Errorf("event carried %d calls, want 1", e.Calls)
		}
		if want := len(order); e.Step != want {
			t.Errorf("event step = %d, want %d", e.Step, want)
		}
		if ran != len(order)-1 {
			t.Errorf("tool had run %d time(s) when dispatching %d was published; the event must come first",
				ran, len(order))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("published %d times for a turn with two tool-calling responses and one text response, want 2",
			len(order))
	}
	if ran != 2 {
		t.Fatalf("the tool ran %d times, want 2", ran)
	}
}

func TestToolsDispatchingErrorKeepsTheToolFromRunning(t *testing.T) {
	h := build(t, []replay.Turn{toolTurn("c1", "probe", `{}`), textTurn("done")}, nil)

	ran := 0
	registerCounter(t, h, "probe", &ran)
	refusal := errors.New("the baseline is not recorded")
	if _, err := bus.OnSerial(h.bus, func(context.Context, *ToolsDispatching) error {
		return refusal
	}); err != nil {
		t.Fatal(err)
	}

	_, err := h.loop.Run(context.Background(), userMsg("go"))
	if !errors.Is(err, refusal) {
		t.Fatalf("run error = %v, want the listener's refusal", err)
	}
	if ran != 0 {
		t.Fatalf("the tool ran %d time(s) after the listener refused to let it", ran)
	}
}

func TestTurnEndedFollowsTurnStartingOnEveryExit(t *testing.T) {
	record := func(h *harness) *[]string {
		var seen []string
		if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStarting) error {
			seen = append(seen, "start")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := bus.OnSerial(h.bus, func(ctx context.Context, e *TurnEnded) error {
			if ctx.Err() != nil {
				t.Error("TurnEnded was dispatched under a cancelled context")
			}
			seen = append(seen, "end")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return &seen
	}
	want := []string{"start", "end"}

	t.Run("natural stop", func(t *testing.T) {
		h := build(t, []replay.Turn{textTurn("done")}, nil)
		seen := record(h)
		if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
			t.Fatal(err)
		}
		if !equalStrings(*seen, want) {
			t.Fatalf("events = %v, want %v", *seen, want)
		}
	})
	t.Run("provider error", func(t *testing.T) {
		h := build(t, []replay.Turn{{Err: "down"}}, nil)
		seen := record(h)
		if _, err := h.loop.Run(context.Background(), userMsg("go")); err == nil {
			t.Fatal("expected the provider error")
		}
		if !equalStrings(*seen, want) {
			t.Fatalf("events = %v, want %v", *seen, want)
		}
	})
	t.Run("cancelled before the first request", func(t *testing.T) {
		h := build(t, []replay.Turn{textTurn("done")}, nil)
		seen := record(h)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _ = h.loop.Run(ctx, userMsg("go"))
		if !equalStrings(*seen, want) {
			t.Fatalf("events = %v, want %v", *seen, want)
		}
	})
	t.Run("a start listener refuses", func(t *testing.T) {
		h := build(t, []replay.Turn{textTurn("done")}, nil)
		seen := record(h)
		if _, err := bus.OnSerial(h.bus, func(context.Context, *TurnStarting) error {
			return errors.New("refused")
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.loop.Run(context.Background(), userMsg("go")); err == nil {
			t.Fatal("expected the refusal")
		}
		if !equalStrings(*seen, want) {
			t.Fatalf("events = %v, want %v: a listener that started before the refusal needs its end", *seen, want)
		}
	})
	t.Run("a panic", func(t *testing.T) {
		h := build(t, []replay.Turn{textTurn("done")}, nil)
		seen := record(h)
		if _, err := bus.OnSerial(h.bus, func(context.Context, *TurnStarting) error {
			panic("listener panicked")
		}); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() { _ = recover() }()
			_, _ = h.loop.Run(context.Background(), userMsg("go"))
		}()
		if !equalStrings(*seen, want) {
			t.Fatalf("events = %v, want %v", *seen, want)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
