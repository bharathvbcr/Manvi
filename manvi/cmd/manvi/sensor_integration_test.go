package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"manvi/agent"
	"manvi/core/bus"
	"manvi/devcouncil"
	"manvi/llm"
	"manvi/llm/replay"
	"manvi/session"
	"manvi/tools"
)

// Every other test in this package exercises the check in isolation. This one
// runs a real agent loop with the real listener attached, because a green build
// is not evidence that the pieces meet: the checkpoint could fire on a bus
// nothing listens to, the verdict could stop at the event and never reach the
// outcome, and each half would still pass its own tests.

func replayLoop(t *testing.T, turns []replay.Turn) (*agent.Loop, *bus.Bus, *session.Log, *tools.Registry) {
	t.Helper()
	b := bus.New()
	log := session.NewLog()
	registry := tools.NewRegistry(b)
	provider := replay.New(replay.Fixture{
		Provider: "replay",
		Capabilities: []llm.Capability{{
			Provider: "replay", Model: "m",
			ContextWindow: 200000, MaxOutputTokens: 8192,
			SupportsTools: true, SupportsStreaming: true,
		}},
		Turns: turns,
	})
	loop, err := agent.NewLoop(agent.Config{
		Provider: provider, Model: "m",
		SystemPrompt: "you are a builder", MaxSteps: 12, MaxTokens: 1024,
		AssertInvariant: true,
	}, b, log, registry)
	if err != nil {
		t.Fatal(err)
	}
	return loop, b, log, registry
}

func replayText(text string) replay.Turn {
	return replay.Turn{
		Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: text}},
		Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: text}}},
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

func replayToolCall(id, name, args string) replay.Turn {
	return replay.Turn{
		Chunks: []llm.Chunk{{Kind: llm.ChunkToolCallStart, ToolCallID: llm.CallID(id), ToolName: name}},
		Message: llm.Message{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{
				llm.ToolCallBlock{ID: llm.CallID(id), Name: name, Arguments: json.RawMessage(args)},
			},
		},
		StopReason: llm.StopToolUse,
		Usage:      llm.Usage{InputTokens: 20, OutputTokens: 8},
	}
}

func registerFakeWrite(t *testing.T, registry *tools.Registry) {
	t.Helper()
	if err := registry.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "write", Description: "write a file"},
		Handler: func(_ context.Context, call tools.Call) tools.Result {
			var args struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(call.Arguments, &args)
			return tools.Result{Text: "wrote " + args.Path, Wrote: []string{args.Path}}
		},
	}); err != nil {
		t.Fatal(err)
	}
}

// A failing check bounces the turn, the model gets the findings as a harness
// message, and the verdict reaches the outcome — where the run summary reads it.
func TestSensorEndToEndBouncesAndReportsThroughTheLoop(t *testing.T) {
	loop, b, log, registry := replayLoop(t, []replay.Turn{
		replayToolCall("c1", "write", `{"path":"a.go"}`),
		replayText("done"),
		replayText("fixed it"),
	})
	registerFakeWrite(t, registry)

	// Fails the first look and passes the second: the shape the bounce exists
	// for. A check that always failed would test the cap, which is
	// TestSensorEndToEndStopsAtTheBounceCap's job.
	verifier := &fakeVerifier{
		queue: []devcouncil.PathReport{
			{
				Verdict:  devcouncil.VerdictFailed,
				Findings: []string{"a.go: the error is discarded"},
				Examined: []string{"a.go"},
			},
			{Verdict: devcouncil.VerdictPassed, Examined: []string{"a.go"}},
		},
	}
	if err := attachSensor(b, &sensor{native: verifier, log: log}); err != nil {
		t.Fatal(err)
	}

	out, err := loop.Run(context.Background(), llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Text: "fix a.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The last verdict wins: the turn was sent back, the model fixed it, and
	// the second look passed.
	if out.Sensor != agent.SensorPassed {
		t.Fatalf("Outcome.Sensor = %q, want passed after the fix", out.Sensor)
	}
	if out.Bounces == 0 {
		t.Fatal("the failing check did not send the turn back")
	}
	if len(out.Wrote) != 1 || out.Wrote[0] != "a.go" {
		t.Fatalf("Outcome.Wrote = %v, want the written path", out.Wrote)
	}
	if len(verifier.gotPaths) != 1 || verifier.gotPaths[0] != "a.go" {
		t.Fatalf("the verifier was handed %v, want the path the turn wrote", verifier.gotPaths)
	}

	// The finding reached the model, marked as the harness speaking.
	var injected bool
	for _, e := range log.Events() {
		if e.Type != session.UserMessage {
			continue
		}
		var d session.MessageData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Origin == session.OriginHarness &&
			strings.Contains(d.Message.Text(), "the error is discarded") {
			injected = true
		}
	}
	if !injected {
		t.Fatal("the finding never reached the model as a harness message")
	}

	// And the run summary accounts for the extra work: a turn the operator
	// asked one question of and paid two model calls for says why.
	var said bool
	for _, n := range outcomeNotices(out, 500) {
		if strings.Contains(n.Text, "sent this turn back") {
			said = true
		}
	}
	if !said {
		t.Fatal("the run summary did not mention that the turn was sent back")
	}
}

// A passing check is silent and costs the turn nothing: no bounce, no notice.
func TestSensorEndToEndIsSilentOnAPass(t *testing.T) {
	loop, b, log, registry := replayLoop(t, []replay.Turn{
		replayToolCall("c1", "write", `{"path":"a.go"}`),
		replayText("done"),
	})
	registerFakeWrite(t, registry)

	if err := attachSensor(b, &sensor{
		native: &fakeVerifier{report: devcouncil.PathReport{Verdict: devcouncil.VerdictPassed}},
		log:    log,
	}); err != nil {
		t.Fatal(err)
	}

	out, err := loop.Run(context.Background(), llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Text: "fix a.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Sensor != agent.SensorPassed {
		t.Fatalf("Outcome.Sensor = %q, want passed", out.Sensor)
	}
	if out.Bounces != 0 {
		t.Fatalf("a passing check bounced the turn %d time(s)", out.Bounces)
	}
	for _, n := range outcomeNotices(out, 500) {
		if strings.Contains(n.Text, "end-of-turn check") {
			t.Fatalf("a clean turn was annotated: %q", n.Text)
		}
	}
}

// A question is not verified and not bounced, and the log says the check was
// not owed rather than leaving a reader to assume it ran.
func TestSensorEndToEndSkipsAQuestion(t *testing.T) {
	loop, b, log, _ := replayLoop(t, []replay.Turn{replayText("it reads a file")})

	if err := attachSensor(b, &sensor{native: &fakeVerifier{}, log: log}); err != nil {
		t.Fatal(err)
	}

	out, err := loop.Run(context.Background(), llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Text: "what does a.go do"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Sensor != agent.SensorSkipped {
		t.Fatalf("Outcome.Sensor = %q, want skipped", out.Sensor)
	}
	if out.Bounces != 0 {
		t.Fatal("a read-only turn was bounced")
	}
	if got := reports(t, log); len(got) != 1 || got[0].Verdict != "skipped" {
		t.Fatalf("reports = %+v, want one record saying the check was not owed", got)
	}
}

// The bounce cap holds end to end: a check that never passes ends the turn with
// the objection standing, said out loud, rather than riding the step ceiling.
func TestSensorEndToEndStopsAtTheBounceCap(t *testing.T) {
	turns := []replay.Turn{replayToolCall("c1", "write", `{"path":"a.go"}`)}
	for range 6 {
		turns = append(turns, replayText("done"))
	}
	loop, b, log, registry := replayLoop(t, turns)
	registerFakeWrite(t, registry)

	if err := attachSensor(b, &sensor{
		native: &fakeVerifier{report: devcouncil.PathReport{
			Verdict: devcouncil.VerdictFailed, Findings: []string{"still broken"},
		}},
		log: log,
	}); err != nil {
		t.Fatal(err)
	}

	out, err := loop.Run(context.Background(), llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Text: "fix a.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Bounces != agent.MaxCheckpointBounces {
		t.Fatalf("bounces = %d, want the cap of %d", out.Bounces, agent.MaxCheckpointBounces)
	}
	if !out.BouncesExhausted {
		t.Fatal("the turn ended with the check still failing and did not say so")
	}
	var said bool
	for _, n := range outcomeNotices(out, 500) {
		if strings.Contains(n.Text, "still not satisfied") {
			said = true
		}
	}
	if !said {
		t.Fatal("the run summary did not report that the check was never satisfied")
	}
}
