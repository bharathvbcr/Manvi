package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The owner's requirement: a turn that changed files is verified to a verdict,
// whatever ended it. These pin the exits that used to skip the check.

func checkedLoop(t *testing.T, turns []replay.Turn, maxSteps int) *harness {
	t.Helper()
	h := build(t, turns, func(c *Config) { c.MaxSteps = maxSteps; c.EndOfTurnCheck = true })
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "write"},
		Handler: func(_ context.Context, c tools.Call) tools.Result {
			return tools.Result{Text: "wrote", Wrote: []string{"a.go"}}
		},
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func verifyReports(t *testing.T, h *harness) []session.VerifyReportData {
	t.Helper()
	var got []session.VerifyReportData
	for _, e := range h.log.Events() {
		if e.Type != session.VerifyReport {
			continue
		}
		var d session.VerifyReportData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		got = append(got, d)
	}
	return got
}

func passing(t *testing.T, h *harness, seen *[]TurnStopping) {
	t.Helper()
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
		*seen = append(*seen, *e)
		e.Verdict = SensorPassed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The step ceiling used to end a mutating turn with no checkpoint at all:
// Sensor empty, nothing in the log, indistinguishable from a quiet turn.
func TestStepCeilingRunsTheCheckOnTheFinalTree(t *testing.T) {
	h := checkedLoop(t, []replay.Turn{
		toolTurn("c1", "write", `{"path":"a.go"}`),
		toolTurn("c2", "write", `{"path":"b.go"}`),
		toolTurn("c3", "write", `{"path":"c.go"}`),
	}, 2)
	var seen []TurnStopping
	passing(t, h, &seen)

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if !out.TruncatedBySteps {
		t.Fatal("fixture: the turn was meant to end on the ceiling")
	}
	if len(seen) != 1 || !seen[0].Final || seen[0].Bounce != 0 {
		t.Fatalf("checkpoints = %+v, want exactly one, Final", seen)
	}
	if out.Sensor != SensorPassed {
		t.Fatalf("sensor = %q, want the verdict of the final check", out.Sensor)
	}
	if len(verifyReports(t, h)) != 0 {
		// The recording belongs to the listener, which here recorded nothing.
		t.Fatal("the loop wrote a report of its own beside a listener's verdict")
	}
}

// A failed check, a bounce, more edits, then the ceiling: the standing verdict
// describes an earlier tree, so the check runs again rather than reporting it.
func TestCeilingAfterABounceRechecksTheLaterEdits(t *testing.T) {
	h := checkedLoop(t, []replay.Turn{
		toolTurn("c1", "write", `{"path":"a.go"}`),
		textTurn("done"),
		toolTurn("c2", "write", `{"path":"b.go"}`),
		toolTurn("c3", "write", `{"path":"c.go"}`),
	}, 3)
	var seen []TurnStopping
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
		seen = append(seen, *e)
		if e.Bounce == 0 && !e.Final {
			e.Verdict, e.Inject = SensorFailed, "fix it"
			return nil
		}
		e.Verdict = SensorPassed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || !seen[1].Final || seen[1].Bounce != 1 {
		t.Fatalf("checkpoints = %+v, want the bounce and then a Final one", seen)
	}
	if out.Sensor != SensorPassed {
		t.Fatalf("sensor = %q: the stale failed verdict was kept over a later check", out.Sensor)
	}
}

// A ceiling with nothing changed since the last verdict does not pay for a
// second identical check.
func TestCeilingDoesNotRecheckAnUnchangedTree(t *testing.T) {
	h := checkedLoop(t, []replay.Turn{
		toolTurn("c1", "write", `{"path":"a.go"}`),
		textTurn("done"),
		toolTurn("c2", "noop", `{}`),
	}, 3)
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "noop"}, ReadOnly: true,
		Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "ok"} },
	}); err != nil {
		t.Fatal(err)
	}
	var seen []TurnStopping
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
		seen = append(seen, *e)
		e.Verdict, e.Inject = SensorFailed, "fix it"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("checkpoints = %d, want 1: nothing changed after the failed verdict", len(seen))
	}
	if out.Sensor != SensorFailed {
		t.Fatalf("sensor = %q, want the standing failed verdict", out.Sensor)
	}
}

// An error that ends a turn that changed files is a check that did not run.
func TestFailedTurnThatChangedFilesIsDegradedNotUnchecked(t *testing.T) {
	// One write, then the provider has nothing left to say: the turn errors.
	h := checkedLoop(t, []replay.Turn{toolTurn("c1", "write", `{"path":"a.go"}`)}, 8)
	var seen []TurnStopping
	passing(t, h, &seen)

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err == nil {
		t.Fatal("fixture: the turn was meant to fail")
	}
	if out.Sensor != SensorDegraded {
		t.Fatalf("sensor = %q, want degraded: files changed and nothing checked them", out.Sensor)
	}
	reports := verifyReports(t, h)
	if len(reports) != 1 || reports[0].Verdict != string(SensorDegraded) || len(reports[0].Degraded) == 0 {
		t.Fatalf("reports = %+v, want one degraded report naming what did not run", reports)
	}
	if len(seen) != 0 {
		t.Fatal("the check ran on a turn that errored before its checkpoint")
	}
}

// A cancellation after a write is the same shape and must not pass as a quiet turn.
func TestCancelledTurnThatChangedFilesIsDegraded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := build(t, []replay.Turn{
		toolTurn("c1", "write", `{"path":"a.go"}`),
		textTurn("done"),
	}, func(c *Config) { c.MaxSteps = 8; c.EndOfTurnCheck = true })
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "write"},
		Handler: func(context.Context, tools.Call) tools.Result {
			cancel()
			return tools.Result{Text: "wrote", Wrote: []string{"a.go"}}
		},
	}); err != nil {
		t.Fatal(err)
	}
	var seen []TurnStopping
	passing(t, h, &seen)

	out, _ := h.loop.Run(ctx, userMsg("go"))
	if out.Sensor != SensorDegraded {
		t.Fatalf("sensor = %q, want degraded: the turn was cancelled with unchecked changes", out.Sensor)
	}
	if reports := verifyReports(t, h); len(reports) != 1 || reports[0].Verdict != string(SensorDegraded) {
		t.Fatalf("reports = %+v, want one degraded report", reports)
	}
	if len(seen) != 0 {
		t.Fatal("a verifier was started for a cancelled turn")
	}
}

// Without the declaration — a sub-agent's loop — nothing is added.
func TestUncheckedLoopRecordsNothingExtra(t *testing.T) {
	h := build(t, []replay.Turn{toolTurn("c1", "write", `{"path":"a.go"}`)}, func(c *Config) { c.MaxSteps = 8 })
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "write"},
		Handler: func(context.Context, tools.Call) tools.Result {
			return tools.Result{Text: "w", Wrote: []string{"a.go"}}
		},
	}); err != nil {
		t.Fatal(err)
	}
	out, _ := h.loop.Run(context.Background(), userMsg("go"))
	if out.Sensor != SensorNone || len(verifyReports(t, h)) != 0 {
		t.Fatalf("sensor=%q reports=%d: a loop that declared no check was given one", out.Sensor, len(verifyReports(t, h)))
	}
}

// The cut-off-answer verdict is reached before any file is read. A turn that
// then ends some other way has still not had its changes examined, and must not
// finish on a "failed" that reads as though they had been.
func TestUnexaminedVerdictDoesNotDischargeTheCheck(t *testing.T) {
	h := checkedLoop(t, []replay.Turn{
		toolTurn("c1", "write", `{"path":"a.go"}`),
		textTurn("cut off"),
	}, 8)
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
		e.Verdict, e.Unexamined, e.Inject = SensorFailed, true, "finish it"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The bounce is answered by a provider that has run dry: the turn errors.
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err == nil {
		t.Fatal("fixture: the turn was meant to fail after the bounce")
	}
	if out.Sensor != SensorDegraded {
		t.Fatalf("sensor = %q, want degraded: no file was ever examined", out.Sensor)
	}
}
