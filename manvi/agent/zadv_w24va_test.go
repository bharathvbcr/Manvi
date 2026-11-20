package agent

import (
	"context"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

func zadvListen(t *testing.T, h *harness, fn func(context.Context, *TurnStopping) error) *int {
	t.Helper()
	n := new(int)
	if _, err := OnCheck(h.bus, func(ctx context.Context, e *TurnStopping) error {
		*n++
		return fn(ctx, e)
	}); err != nil {
		t.Fatalf("listen: %v", err)
	}
	return n
}

func zadvWriteThenText(t *testing.T, maxSteps int) *harness {
	t.Helper()
	h := build(t, []replay.Turn{
		toolTurn("call-1", "write", `{"path":"a.go"}`),
		textTurn("done"), textTurn("done again"), textTurn("and again"),
	}, func(c *Config) { c.MaxSteps = maxSteps })
	registerWriteTool(t, h)
	return h
}

// The ceiling lands exactly on the step that ends the turn: the checkpoint is
// reached, so the verdict is the check's, not "interrupted".
func TestZadv_w24va_ACeilingExactlyAtTheCheckpointStepStillRunsTheCheck(t *testing.T) {
	h := zadvWriteThenText(t, 2)
	asked := zadvListen(t, h, func(_ context.Context, e *TurnStopping) error {
		e.Verdict = SensorPassed
		return nil
	})
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if *asked != 1 || out.Sensor != SensorPassed || out.TruncatedBySteps {
		t.Fatalf("asked=%d sensor=%q truncated=%v", *asked, out.Sensor, out.TruncatedBySteps)
	}
}

// A bounce that spends the last step leaves the check's own verdict standing.
func TestZadv_w24va_ACeilingAfterABounceKeepsTheFailedVerdict(t *testing.T) {
	h := zadvWriteThenText(t, 2)
	zadvListen(t, h, func(_ context.Context, e *TurnStopping) error {
		e.Verdict, e.Inject = SensorFailed, "fix it"
		return nil
	})
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.TruncatedBySteps || out.Sensor != SensorFailed {
		t.Fatalf("truncated=%v sensor=%q, want the failed verdict to stand", out.TruncatedBySteps, out.Sensor)
	}
}

// Cancelled inside the check: what the check concluded is not overwritten by
// the cancellation, and a check that gave up is degraded, not interrupted.
func TestZadv_w24va_CancelDuringTheCheckKeepsWhatTheCheckConcluded(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(cancel context.CancelFunc, e *TurnStopping) error
		want SensorVerdict
	}{
		{"concluded passed then cancelled", func(cancel context.CancelFunc, e *TurnStopping) error {
			e.Verdict = SensorPassed
			cancel()
			return nil
		}, SensorPassed},
		{"passed with an inject, then cancelled", func(cancel context.CancelFunc, e *TurnStopping) error {
			e.Verdict, e.Inject = SensorPassed, "one more"
			cancel()
			return nil
		}, SensorPassed},
		{"gave up with the context error", func(cancel context.CancelFunc, e *TurnStopping) error {
			cancel()
			return context.Canceled
		}, SensorDegraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := zadvWriteThenText(t, 8)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			zadvListen(t, h, func(_ context.Context, e *TurnStopping) error { return tc.fn(cancel, e) })
			out, err := h.loop.Run(ctx, userMsg("go"))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if out.Sensor != tc.want {
				t.Fatalf("sensor = %q, want %q", out.Sensor, tc.want)
			}
		})
	}
}

// A listener that panics must not leave the outcome reading as a pass or as no
// check: either the panic is contained and the turn is degraded, or it reaches
// the caller. It must never come back as a quiet success.
func TestZadv_w24va_APanickingCheckIsNeverAQuietSuccess(t *testing.T) {
	h := zadvWriteThenText(t, 8)
	zadvListen(t, h, func(context.Context, *TurnStopping) error { panic("sensor blew up") })
	var out Outcome
	var runErr error
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		out, runErr = h.loop.Run(context.Background(), userMsg("go"))
	}()
	if panicked == nil && runErr == nil && (out.Sensor == SensorNone || out.Sensor == SensorPassed) {
		t.Fatalf("a panicking check came back as sensor %q with no error", out.Sensor)
	}
}

// The log is the only record a reader who was not there has. A degraded
// checkpoint writes a verify/report; an interrupted one wrote nothing, so the
// unchecked change is visible only on the in-memory Outcome.
func TestZadv_w24va_AnInterruptedTurnLeavesAVerifyReportInTheLog(t *testing.T) {
	h := zadvWriteThenText(t, 1)
	zadvListen(t, h, func(context.Context, *TurnStopping) error { return nil })
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Sensor != SensorInterrupted {
		t.Fatalf("setup: sensor = %q", out.Sensor)
	}
	for _, ev := range h.log.Events() {
		if ev.Type == session.VerifyReport {
			return
		}
	}
	t.Fatal("the turn ended with an unchecked change and no verify/report in the log")
}
