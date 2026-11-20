package agent

import (
	"context"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// A turn that changed something, with a check registered, and ended before the
// terminal checkpoint reached it, was reported with the same Sensor value as a
// turn nobody registered a check for. The first has an unchecked change; the
// second has nothing to check.
func TestAStepCeilingBeforeTheCheckpointIsNotTheNoCheckVerdict(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "write", `{"path":"a.go"}`),
		textTurn("done"),
	}, func(c *Config) { c.MaxSteps = 1 })
	registerWriteTool(t, h)
	asked := 0
	if _, err := OnCheck(h.bus, func(_ context.Context, _ *TurnStopping) error {
		asked++
		return nil
	}); err != nil {
		t.Fatalf("listen: %v", err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.TruncatedBySteps || !out.Mutated || asked != 0 {
		t.Fatalf("setup: truncated=%v mutated=%v checkpoint asked %d times", out.TruncatedBySteps, out.Mutated, asked)
	}
	if out.Sensor == SensorNone {
		t.Fatalf("sensor = %q: an unchecked change reads as no check registered", out.Sensor)
	}
	if out.Sensor != SensorInterrupted {
		t.Fatalf("sensor = %q, want %q", out.Sensor, SensorInterrupted)
	}
}

// The value is owed only where a check exists and something changed.
func TestAnInterruptedTurnOwesNoCheckWithoutAListenerOrAChange(t *testing.T) {
	t.Run("no check registered", func(t *testing.T) {
		h := build(t, []replay.Turn{toolTurn("call-1", "write", `{"path":"a.go"}`), textTurn("done")},
			func(c *Config) { c.MaxSteps = 1 })
		registerWriteTool(t, h)
		out, err := h.loop.Run(context.Background(), userMsg("go"))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if out.Sensor != SensorNone {
			t.Fatalf("sensor = %q, want %q with nothing registered", out.Sensor, SensorNone)
		}
	})
	t.Run("nothing changed", func(t *testing.T) {
		h := build(t, []replay.Turn{toolTurn("call-1", "read", `{"path":"a.go"}`), textTurn("done")},
			func(c *Config) { c.MaxSteps = 1 })
		if err := h.tools.Register(tools.Tool{
			Schema: llm.ToolSchema{Name: "read", Description: "read"}, ReadOnly: true,
			Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "x"} },
		}); err != nil {
			t.Fatalf("register: %v", err)
		}
		if _, err := OnCheck(h.bus, func(_ context.Context, _ *TurnStopping) error { return nil }); err != nil {
			t.Fatalf("listen: %v", err)
		}
		out, err := h.loop.Run(context.Background(), userMsg("go"))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if out.Sensor != SensorNone {
			t.Fatalf("sensor = %q, want %q for a turn that changed nothing", out.Sensor, SensorNone)
		}
	})
}

// Cancelled after the last response and before the checkpoint: the same state.
func TestACancelBeforeTheCheckpointIsNotTheNoCheckVerdict(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "write", `{"path":"a.go"}`),
		textTurn("done"),
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "write", Description: "write"},
		Handler: func(context.Context, tools.Call) tools.Result {
			cancel()
			return tools.Result{Text: "wrote", Wrote: []string{"a.go"}}
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := OnCheck(h.bus, func(_ context.Context, _ *TurnStopping) error { return nil }); err != nil {
		t.Fatalf("listen: %v", err)
	}

	out, err := h.loop.Run(ctx, userMsg("go"))
	if err != nil {
		t.Skipf("the provider refuses a cancelled context before the checkpoint is reached: %v", err)
	}
	if out.Sensor != SensorInterrupted {
		t.Fatalf("sensor = %q, want %q", out.Sensor, SensorInterrupted)
	}
}

// A listener that only watches the checkpoint is not a check: a turn it saw
// being cut off owes nothing, and reads as the no-check value.
func TestAPureCheckpointObserverDoesNotMakeATurnOweACheck(t *testing.T) {
	h := build(t, []replay.Turn{toolTurn("call-1", "write", `{"path":"a.go"}`), textTurn("done")},
		func(c *Config) { c.MaxSteps = 1 })
	registerWriteTool(t, h)
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, _ *TurnStopping) error { return nil }); err != nil {
		t.Fatalf("listen: %v", err)
	}
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Sensor != SensorNone {
		t.Fatalf("sensor = %q, want %q for a turn only observed", out.Sensor, SensorNone)
	}
	for _, ev := range h.log.Events() {
		if ev.Type == session.VerifyReport {
			t.Fatal("an observer produced a verify/report entry")
		}
	}
}
