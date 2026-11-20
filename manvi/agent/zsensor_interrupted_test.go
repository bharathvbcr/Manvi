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

// cancelAfterResponse cancels its context when the nth stream settles into a
// response, so the cancel lands after the response and before the checkpoint.
type cancelAfterResponse struct {
	llm.Provider
	cancel context.CancelFunc
	on     int
	seen   *int
}

func (p cancelAfterResponse) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	s, err := p.Provider.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	*p.seen++
	return cancelingStream{Stream: s, cancel: p.cancel, fire: *p.seen == p.on}, nil
}

type cancelingStream struct {
	llm.Stream
	cancel context.CancelFunc
	fire   bool
}

func (s cancelingStream) Response() (llm.Response, error) {
	r, err := s.Stream.Response()
	if s.fire {
		s.cancel()
	}
	return r, err
}

// Cancelled after the last response and before the checkpoint: the cancel branch
// in closeOrBounce, which ends the turn with no error.
func TestACancelBeforeTheCheckpointIsNotTheNoCheckVerdict(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := 0
	h := build(t, []replay.Turn{
		toolTurn("call-1", "write", `{"path":"a.go"}`),
		textTurn("done"),
	}, func(c *Config) {
		c.Provider = cancelAfterResponse{Provider: c.Provider, cancel: cancel, on: 2, seen: &seen}
	})
	registerWriteTool(t, h)
	asked := 0
	if _, err := OnCheck(h.bus, func(_ context.Context, _ *TurnStopping) error { asked++; return nil }); err != nil {
		t.Fatalf("listen: %v", err)
	}

	out, err := h.loop.Run(ctx, userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if asked != 0 {
		t.Fatalf("the checkpoint ran %d times despite the cancel", asked)
	}
	if out.Sensor != SensorInterrupted {
		t.Fatalf("sensor = %q, want %q", out.Sensor, SensorInterrupted)
	}
	if !hasVerifyReport(h) {
		t.Fatal("no verify/report entry for the interrupted turn")
	}
}

// The ordinary cancel: Ctrl-C while the next model call is streaming, on a turn
// that already wrote. Run returns the cancellation, unchanged, with the verdict
// and the log entry.
func TestACancelMidTurnAfterAWriteIsInterruptedAndKeepsItsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := build(t, []replay.Turn{
		toolTurn("call-1", "write", `{"path":"a.go"}`),
		textTurn("done"),
	}, nil)
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
	if err == nil {
		t.Fatalf("a cancelled turn returned no error (sensor %q)", out.Sensor)
	}
	if out.Sensor != SensorInterrupted || !hasVerifyReport(h) {
		t.Fatalf("sensor = %q, report logged = %v; want interrupted and logged", out.Sensor, hasVerifyReport(h))
	}
}

// With no check registered, or nothing written, a cancel owes and records nothing.
func TestACancelOwesNothingWithoutACheckOrAWrite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		check   bool
		mutates bool
	}{{"no check", false, true}, {"nothing written", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := build(t, []replay.Turn{toolTurn("call-1", "write", `{"path":"a.go"}`), textTurn("done")}, nil)
			if err := h.tools.Register(tools.Tool{
				Schema: llm.ToolSchema{Name: "write", Description: "write"}, ReadOnly: !tc.mutates,
				Handler: func(context.Context, tools.Call) tools.Result {
					cancel()
					return tools.Result{Text: "x"}
				},
			}); err != nil {
				t.Fatalf("register: %v", err)
			}
			if tc.check {
				if _, err := OnCheck(h.bus, func(_ context.Context, _ *TurnStopping) error { return nil }); err != nil {
					t.Fatalf("listen: %v", err)
				}
			}
			out, _ := h.loop.Run(ctx, userMsg("go"))
			if out.Sensor != SensorNone || hasVerifyReport(h) {
				t.Fatalf("sensor = %q, logged = %v; want none and not logged", out.Sensor, hasVerifyReport(h))
			}
		})
	}
}

func hasVerifyReport(h *harness) bool {
	for _, ev := range h.log.Events() {
		if ev.Type == session.VerifyReport {
			return true
		}
	}
	return false
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
