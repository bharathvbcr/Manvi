package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"manvi/core/bus"
	"manvi/llm"
	"manvi/llm/replay"
	"manvi/session"
	"manvi/tools"
)

// The checkpoint decides whether work is finished, so the inputs it has to
// survive are the hostile ones: a tool that claims a thousand paths, a tool
// that claims the same path a thousand times, paths with newlines in them, a
// listener that panics, and two turns racing each other.

// A turn that writes more paths than the loop tracks must carry the
// incompleteness with the list. A truncated set passed on as complete is how a
// verifier comes to certify files nothing looked at.
func TestWrittenPathsAreCappedAndSayTheyWereCapped(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "writemany", `{"n":400}`),
		textTurn("done"),
	}, func(c *Config) { c.MaxSteps = 8 })

	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "writemany"},
		Handler: func(_ context.Context, call tools.Call) tools.Result {
			var args struct {
				N int `json:"n"`
			}
			_ = json.Unmarshal(call.Arguments, &args)
			paths := make([]string, 0, args.N)
			for i := range args.N {
				paths = append(paths, fmt.Sprintf("pkg/f%04d.go", i))
			}
			return tools.Result{Text: "wrote many", Wrote: paths}
		},
	}); err != nil {
		t.Fatal(err)
	}

	var seen *TurnStopping
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
		copied := *e
		seen = &copied
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Wrote) != MaxTrackedWrites {
		t.Fatalf("tracked %d paths, want the cap of %d", len(out.Wrote), MaxTrackedWrites)
	}
	if !out.WroteTruncated {
		t.Fatal("the list was cut and nothing says so — a capped sample must never be " +
			"presented as complete coverage")
	}
	if seen == nil || !seen.WroteTruncated {
		t.Fatal("the checkpoint was handed a truncated list without being told")
	}
}

// The same path written repeatedly is one file to verify, and must not consume
// the cap.
func TestWrittenPathsAreDeduplicated(t *testing.T) {
	have, truncated := []string(nil), false
	for range 1000 {
		have, truncated = trackWrites(have, truncated, []string{"a.go", "b.go"})
	}
	if len(have) != 2 {
		t.Fatalf("tracked %d paths for two distinct files", len(have))
	}
	if truncated {
		t.Fatal("de-duplication reported a truncation that did not happen")
	}
}

// Junk from a handler must not enter the set. A path that is empty or is
// nothing but whitespace names no file, and carrying it would produce a check
// against "".
func TestWrittenPathsRejectEmptyEntries(t *testing.T) {
	have, truncated := trackWrites(nil, false, []string{"", "   ", "\t\n", "real.go"})
	if len(have) != 1 || have[0] != "real.go" {
		t.Fatalf("tracked %v, want only the real path", have)
	}
	if truncated {
		t.Fatal("dropping blanks was reported as a truncation")
	}
}

// Once truncated, always truncated: a later call that happens to fit must not
// clear the flag and make the set look complete again.
func TestTruncationIsSticky(t *testing.T) {
	var have []string
	truncated := false
	for i := range MaxTrackedWrites + 5 {
		have, truncated = trackWrites(have, truncated, []string{fmt.Sprintf("f%d.go", i)})
	}
	if !truncated {
		t.Fatal("the cap was hit without being recorded")
	}
	have, truncated = trackWrites(have, truncated, []string{"a.go"})
	if !truncated {
		t.Fatal("a later call cleared the truncation flag, so an incomplete set now claims to " +
			"be complete")
	}
	_ = have
}

// A listener that panics is a defect in the harness, and the loop's contract is
// that a defect must not be reported as a verified turn.
func TestAPanickingCheckpointListenerDoesNotCertifyTheTurn(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("done")}, nil)

	if _, err := bus.OnSerial(h.bus, func(_ context.Context, _ *TurnStopping) error {
		panic("listener defect")
	}); err != nil {
		t.Fatal(err)
	}

	defer func() {
		// The bus does not contain a serial panic today, so this test states
		// what must remain true rather than asserting a recovery that does not
		// exist: if it ever starts containing one, the turn must not come back
		// reporting a pass.
		if r := recover(); r == nil {
			t.Fatal("the panic was contained; assert here that the outcome is not a pass")
		}
	}()
	_, _ = h.loop.Run(context.Background(), userMsg("go"))
}

// Two listeners, and the second still runs when the first sets an inject. A
// checkpoint that stopped at the first opinion would silently drop a sensor
// registered after a pattern listener.
func TestEveryCheckpointListenerIsConsulted(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("first"), textTurn("second")}, nil)

	var order []string
	for _, name := range []string{"a", "b"} {
		if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
			order = append(order, name)
			if name == "a" && e.Bounce == 0 {
				e.Inject = "from a"
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	if len(order) != 4 {
		t.Fatalf("listeners ran %v, want both on each of the two checkpoints", order)
	}
}

// A later listener overriding an earlier one's inject is ordinary composition,
// and the loop must inject exactly one message per bounce rather than
// concatenating whatever each listener said.
func TestOneInjectPerBounce(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("first"), textTurn("second")}, nil)

	for _, text := range []string{"first opinion", "second opinion"} {
		if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
			if e.Bounce == 0 {
				e.Inject = text
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}

	var harnessMessages int
	for _, e := range h.log.Events() {
		if e.Type != session.UserMessage {
			continue
		}
		var d session.MessageData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Origin == session.OriginHarness {
			harnessMessages++
			if !strings.Contains(d.Message.Text(), "second opinion") {
				t.Fatalf("injected %q, want the last listener's text", d.Message.Text())
			}
		}
	}
	if harnessMessages != 1 {
		t.Fatalf("%d harness messages for one bounce, want 1", harnessMessages)
	}
}

// An inject that is nothing but whitespace is not a reason to keep the turn
// open. Treating it as one would spend a model call on an empty message.
func TestBlankInjectDoesNotBounce(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("done")}, nil)

	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
		e.Inject = "   \n\t "
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Bounces != 0 {
		t.Fatalf("bounces = %d on a blank inject, want 0", out.Bounces)
	}
}

// Loops are independent. Two running at once must not share checkpoint state,
// which is the property that makes the per-turn bounce counter safe.
func TestConcurrentLoopsDoNotShareCheckpointState(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			turns := make([]replay.Turn, 0, 4)
			for range 4 {
				turns = append(turns, textTurn("done"))
			}
			h := buildConcurrent(t, turns)
			if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
				e.Inject = fmt.Sprintf("loop %d wants more", i)
				return nil
			}); err != nil {
				t.Error(err)
				return
			}
			out, err := h.loop.Run(context.Background(), userMsg("go"))
			if err != nil {
				t.Error(err)
				return
			}
			if out.Bounces != MaxCheckpointBounces {
				t.Errorf("loop %d bounced %d times, want %d", i, out.Bounces, MaxCheckpointBounces)
			}
		}()
	}
	wg.Wait()
}

// buildConcurrent is build without the *testing.T helper calls that are not
// safe to make from a non-test goroutine.
func buildConcurrent(t *testing.T, turns []replay.Turn) *harness {
	b := bus.New()
	log := session.NewLog()
	registry := tools.NewRegistry(b)
	provider := replay.New(replay.Fixture{
		Provider:     "replay",
		Capabilities: []llm.Capability{caps("test-model")},
		Turns:        turns,
	})
	loop, err := NewLoop(Config{
		Provider: provider, Model: "test-model",
		SystemPrompt: "you are a builder", MaxSteps: 8, MaxTokens: 1024,
		AssertInvariant: true,
	}, b, log, registry)
	if err != nil {
		t.Error(err)
		return nil
	}
	return &harness{bus: b, log: log, tools: registry, provider: provider, loop: loop}
}
