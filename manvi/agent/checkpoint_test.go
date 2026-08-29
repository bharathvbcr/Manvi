package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"manvi/core/bus"
	"manvi/llm"
	"manvi/llm/replay"
	"manvi/session"
	"manvi/tools"
)

// The terminal checkpoint is the seam that decides whether a turn that *looks*
// finished actually is. These tests fix its contract, and every one of them
// fails against the shape it replaced — a listener returning an error, and the
// loop responding by re-asking the model with a history it had already seen.

// TestCheckpointInjectIsVisibleToTheModel is the whole reason the contract
// changed. A listener that wants one more step has to give the model something
// it did not have, and the loop is what puts it in the log: a listener holding
// the log would be able to append something the model never saw, which is the
// one invariant this harness will not trade.
func TestCheckpointInjectIsVisibleToTheModel(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("first"), textTurn("second")}, nil)

	asked := 0
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
		asked++
		if asked == 1 {
			e.Inject = "the verifier has not run"
		}
		return nil
	}); err != nil {
		t.Fatalf("listen: %v", err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if asked != 2 {
		t.Fatalf("checkpoint consulted %d times, want 2", asked)
	}
	if out.Final.Text() != "second" {
		t.Fatalf("final = %q, want the step after the inject", out.Final.Text())
	}

	// Model-visible means logged: the injected text must be in the projection
	// the next request is built from, not held in a listener's memory.
	var found bool
	for _, e := range h.log.Events() {
		if e.Type != session.UserMessage {
			continue
		}
		var data session.MessageData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if strings.Contains(data.Message.Text(), "the verifier has not run") {
			found = true
			if data.Origin != session.OriginHarness {
				t.Fatalf("inject origin = %q, want %q — an inject the faces cannot tell "+
					"from an operator message is a lie in the transcript",
					data.Origin, session.OriginHarness)
			}
		}
	}
	if !found {
		t.Fatal("the injected text never reached the log")
	}
}

// TestCheckpointBouncesAreBounded is the replacement for riding MaxSteps. A
// listener that keeps asking is answered twice and then the turn closes: a live
// model that has already said it is done will keep saying so, and spending 500
// steps discovering that helps nobody.
func TestCheckpointBouncesAreBounded(t *testing.T) {
	turns := make([]replay.Turn, 0, 6)
	for range 6 {
		turns = append(turns, textTurn("done"))
	}
	h := build(t, turns, nil)

	asked := 0
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
		asked++
		e.Inject = "still not verified"
		return nil
	}); err != nil {
		t.Fatalf("listen: %v", err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Bounces != MaxCheckpointBounces {
		t.Fatalf("bounces = %d, want %d", out.Bounces, MaxCheckpointBounces)
	}
	if out.Steps != MaxCheckpointBounces+1 {
		t.Fatalf("steps = %d, want %d — the turn rode past the bounce cap",
			out.Steps, MaxCheckpointBounces+1)
	}
	if !out.BouncesExhausted {
		t.Fatal("a turn closed over a listener still asking for another step must say so")
	}
}

// TestCheckpointCounterIsPerTurn guards the defect that made the bounce counter
// worth putting on the loop rather than in a listener closure: a second turn on
// the same loop must get its own budget.
func TestCheckpointCounterIsPerTurn(t *testing.T) {
	turns := make([]replay.Turn, 0, 8)
	for range 8 {
		turns = append(turns, textTurn("done"))
	}
	h := build(t, turns, nil)

	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
		e.Inject = "again"
		return nil
	}); err != nil {
		t.Fatalf("listen: %v", err)
	}

	for turn := 1; turn <= 2; turn++ {
		out, err := h.loop.Run(context.Background(), userMsg("go"))
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		if out.Bounces != MaxCheckpointBounces {
			t.Fatalf("turn %d bounces = %d, want %d — the budget did not reset",
				turn, out.Bounces, MaxCheckpointBounces)
		}
	}
}

// TestCheckpointErrorClosesRatherThanSpins is the falsified behaviour, stated
// as a rule. A listener that fails has not asked for another step, and re-
// sending an identical history is not a recovery — it is the same request with
// the same answer, charged again.
func TestCheckpointErrorClosesRatherThanSpins(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("done")}, nil)

	if _, err := bus.OnSerial(h.bus, func(_ context.Context, _ *TurnStopping) error {
		return errors.New("the sensor could not run")
	}); err != nil {
		t.Fatalf("listen: %v", err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Steps != 1 {
		t.Fatalf("steps = %d, want 1 — the turn re-asked the model after a listener error", out.Steps)
	}
	if out.Sensor != SensorDegraded {
		t.Fatalf("sensor = %q, want %q", out.Sensor, SensorDegraded)
	}
	if h.provider.Remaining() != 0 {
		t.Fatalf("%d turns unplayed", h.provider.Remaining())
	}
}

// TestCheckpointSeesMutationAndTruncation gives the listener the two facts it
// cannot otherwise learn: whether this turn changed anything, and whether the
// answer it is about to certify was cut off mid-sentence.
func TestCheckpointSeesMutationAndTruncation(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "write", `{"path":"a.go"}`),
		textTurn("done"),
	}, nil)
	registerWriteTool(t, h)

	var seen *TurnStopping
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
		copied := *e
		seen = &copied
		return nil
	}); err != nil {
		t.Fatalf("listen: %v", err)
	}

	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if seen == nil {
		t.Fatal("checkpoint never fired")
	}
	if !seen.Mutated {
		t.Fatal("a turn that ran a successful write reported Mutated = false")
	}
	if got := seen.Wrote; len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("wrote = %v, want [a.go]", got)
	}
	if seen.Truncated {
		t.Fatal("a turn that ended on a clean stop reported Truncated = true")
	}
}

// TestCheckpointSkipsAQuestion is the other half: a turn that only read must
// not be handed to a sensor, and must not be bounced.
func TestCheckpointSkipsAQuestion(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "read", `{"path":"a.go"}`),
		textTurn("it reads a file"),
	}, nil)
	if err := h.tools.Register(tools.Tool{
		Schema:   llm.ToolSchema{Name: "read", Description: "read"},
		ReadOnly: true,
		Handler: func(context.Context, tools.Call) tools.Result {
			return tools.Result{Text: "package main"}
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	var seen *TurnStopping
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
		copied := *e
		seen = &copied
		return nil
	}); err != nil {
		t.Fatalf("listen: %v", err)
	}

	if _, err := h.loop.Run(context.Background(), userMsg("what does a.go do")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if seen == nil {
		t.Fatal("checkpoint never fired")
	}
	if seen.Mutated {
		t.Fatal("a read-only turn reported Mutated = true")
	}
	if len(seen.Wrote) != 0 {
		t.Fatalf("wrote = %v, want none", seen.Wrote)
	}
}

// TestCheckpointDoesNotBounceACancelledTurn: cancellation is the operator
// saying stop. A bounce fired on the way out would spend another model call on
// a turn nobody is waiting for.
func TestCheckpointDoesNotBounceACancelledTurn(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("done"), textTurn("second")}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
		cancel()
		e.Inject = "one more"
		return nil
	}); err != nil {
		t.Fatalf("listen: %v", err)
	}

	out, err := h.loop.Run(ctx, userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Bounces != 0 {
		t.Fatalf("bounces = %d on a cancelled turn, want 0", out.Bounces)
	}
	if out.Steps != 1 {
		t.Fatalf("steps = %d, want 1", out.Steps)
	}
}

// registerWriteTool registers a mutating tool that reports the path it wrote,
// which is the signal the sensor keys on.
func registerWriteTool(t *testing.T, h *harness) {
	t.Helper()
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "write", Description: "write"},
		Handler: func(_ context.Context, call tools.Call) tools.Result {
			var args struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(call.Arguments, &args)
			return tools.Result{Text: "wrote " + args.Path, Wrote: []string{args.Path}}
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
}
