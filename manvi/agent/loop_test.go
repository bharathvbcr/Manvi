package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"manvi/core/bus"
	"manvi/llm"
	"manvi/llm/replay"
	"manvi/session"
	"manvi/tools"
)

func caps(model string) llm.Capability {
	return llm.Capability{
		Provider: "replay", Model: model,
		ContextWindow: 200000, MaxOutputTokens: 8192,
		SupportsTools: true, SupportsStreaming: true,
	}
}

func textTurn(text string) replay.Turn {
	return replay.Turn{
		Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: text}},
		Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: text}}},
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

func toolTurn(id, name, args string) replay.Turn {
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

type harness struct {
	bus      *bus.Bus
	log      *session.Log
	tools    *tools.Registry
	provider *replay.Provider
	loop     *Loop
}

func build(t *testing.T, turns []replay.Turn, cfg func(*Config)) *harness {
	t.Helper()
	return buildOn(t, caps("test-model"), turns, cfg)
}

// buildOn is build with the model's capability spelled out, for the tests that
// turn on what the model declares it can do rather than on the loop's own
// bookkeeping.
func buildOn(t *testing.T, capability llm.Capability, turns []replay.Turn, cfg func(*Config)) *harness {
	t.Helper()
	b := bus.New()
	log := session.NewLog()
	registry := tools.NewRegistry(b)
	provider := replay.New(replay.Fixture{
		Provider:     "replay",
		Capabilities: []llm.Capability{capability},
		Turns:        turns,
	})

	c := Config{
		Provider: provider, Model: "test-model",
		SystemPrompt: "you are a builder", MaxSteps: 8, MaxTokens: 1024,
		AssertInvariant: true,
	}
	if cfg != nil {
		cfg(&c)
	}
	loop, err := NewLoop(c, b, log, registry)
	if err != nil {
		t.Fatalf("new loop: %v", err)
	}
	return &harness{bus: b, log: log, tools: registry, provider: provider, loop: loop}
}

func userMsg(text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: text}}}
}

func TestSingleStepTurn(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("done")}, nil)

	out, err := h.loop.Run(context.Background(), userMsg("hello"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Steps != 1 || out.StopReason != llm.StopEndTurn {
		t.Fatalf("outcome = %+v", out)
	}
	if out.Final.Text() != "done" {
		t.Fatalf("final text = %q", out.Final.Text())
	}
	if h.provider.Remaining() != 0 {
		t.Fatalf("%d recorded turns went unplayed", h.provider.Remaining())
	}
}

// TestMultiStepTurnWithToolCalls is half the Phase 4 gate: a replayed session
// drives a full multi-step turn through the tool pipeline.
func TestMultiStepTurnWithToolCalls(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "read", `{"path":"a.go"}`),
		toolTurn("call-2", "read", `{"path":"b.go"}`),
		textTurn("both read"),
	}, nil)

	var seen []string
	if err := h.tools.Register(tools.Tool{
		Schema:   llm.ToolSchema{Name: "read", Description: "read a file"},
		ReadOnly: true,
		Handler: func(_ context.Context, call tools.Call) tools.Result {
			seen = append(seen, string(call.Arguments))
			return tools.Result{Text: "contents of " + call.Name}
		},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("read both files"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Steps != 3 || out.ToolCalls != 2 {
		t.Fatalf("outcome = %+v, want 3 steps and 2 tool calls", out)
	}
	if len(seen) != 2 {
		t.Fatalf("tool handler saw %d calls", len(seen))
	}
	if out.TruncatedBySteps {
		t.Fatal("turn should have ended naturally, not on the step ceiling")
	}

	// The third request must carry both tool results, projected from the log.
	requests := h.provider.Requests()
	last := requests[len(requests)-1]
	results := 0
	for _, msg := range last.Messages {
		for _, block := range msg.Content {
			if block.Kind() == llm.KindToolResult {
				results++
			}
		}
	}
	if results != 2 {
		t.Fatalf("final request carried %d tool results, want 2", results)
	}
}

// TestPreExecuteDenialShortCircuits is the other half of the gate: denying at
// pre-execute skips the body, and the loop is never told which listener decided.
func TestPreExecuteDenialShortCircuits(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "write", `{"path":"secret.env"}`),
		textTurn("understood"),
	}, nil)

	bodyRan := false
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "write"},
		Handler: func(context.Context, tools.Call) tools.Result {
			bodyRan = true
			return tools.Result{Text: "written"}
		},
	}); err != nil {
		t.Fatal(err)
	}

	// The gate mounts as an ordinary listener. It knows nothing about the loop,
	// and the loop knows nothing about it.
	if _, err := bus.OnWaterfall(h.bus, func(e tools.PreExecute, next bus.Next[tools.PreExecute]) tools.PreExecute {
		if strings.Contains(string(e.Call.Arguments), ".env") {
			e.Decided = &tools.Result{
				Text: "Secret and credential paths are never writable.", IsError: true,
				Rule: "path.secret", Severity: "hard",
			}
			return e // short-circuit: next is never called
		}
		return next(e)
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("write the env file"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if bodyRan {
		t.Fatal("a short-circuited pre-execute must not run the tool body")
	}
	if out.Denied != 1 {
		t.Fatalf("outcome recorded %d denials, want 1", out.Denied)
	}

	// The denial is a first-class session event beside the tool result, so the
	// evidence trail explains the refusal.
	var denials, results int
	for _, event := range h.log.Events() {
		switch event.Type {
		case session.PolicyDenied:
			denials++
			var data session.DenialData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Rule != "path.secret" || data.Severity != "hard" {
				t.Fatalf("denial event = %+v", data)
			}
		case session.ToolResult:
			results++
		}
	}
	if denials != 1 || results != 1 {
		t.Fatalf("log has %d denials and %d results, want 1 and 1", denials, results)
	}
}

func TestStepCeilingIsReportedNotSilent(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("c1", "noop", `{}`),
		toolTurn("c2", "noop", `{}`),
		toolTurn("c3", "noop", `{}`),
	}, func(c *Config) { c.MaxSteps = 2 })

	if err := h.tools.Register(tools.Tool{
		Schema:  llm.ToolSchema{Name: "noop"},
		Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "ok"} },
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Steps != 2 {
		t.Fatalf("steps = %d, want the ceiling of 2", out.Steps)
	}
	if !out.TruncatedBySteps {
		t.Fatal("a turn cut short by the step ceiling must say so, or a caller will read it as complete")
	}
}

func TestUnboundedTurnIsRefusedAtConstruction(t *testing.T) {
	b := bus.New()
	_, err := NewLoop(Config{
		Provider: replay.New(replay.Fixture{}), Model: "m", MaxSteps: 0,
	}, b, session.NewLog(), tools.NewRegistry(b))
	if err == nil {
		t.Fatal("MaxSteps of 0 must be refused")
	}
}

func TestUnknownToolIsAnErrorResultNotACrash(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("c1", "nonexistent", `{}`),
		textTurn("ok then"),
	}, nil)

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Steps != 2 {
		t.Fatalf("the turn should continue after an unknown tool, got %d steps", out.Steps)
	}
}

func TestCapabilityViolationFailsAtAssembly(t *testing.T) {
	registry := llm.NewRegistry()
	provider := replay.New(replay.Fixture{
		Provider: "replay",
		Capabilities: []llm.Capability{{
			Provider: "replay", Model: "test-model",
			SupportsTools: false, // deliberately cannot take tools
		}},
		Turns: []replay.Turn{textTurn("never reached")},
	})
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}

	b := bus.New()
	toolRegistry := tools.NewRegistry(b)
	if err := toolRegistry.Register(tools.Tool{
		Schema:  llm.ToolSchema{Name: "read"},
		Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{} },
	}); err != nil {
		t.Fatal(err)
	}

	loop, err := NewLoop(Config{
		Provider: provider, Registry: registry, Model: "test-model", MaxSteps: 4,
	}, b, session.NewLog(), toolRegistry)
	if err != nil {
		t.Fatal(err)
	}

	_, err = loop.Run(context.Background(), userMsg("go"))
	if err == nil || !strings.Contains(err.Error(), "does not support tools") {
		t.Fatalf("error = %v, want an assembly-time capability failure", err)
	}
	if provider.Remaining() != 1 {
		t.Fatal("the request must not have reached the provider")
	}
}

func TestPreStepCanRejectAStep(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("never")}, nil)
	if _, err := bus.OnWaterfall(h.bus, func(e PreStep, next bus.Next[PreStep]) PreStep {
		e.Reject = errors.New("context budget exceeded")
		return e
	}); err != nil {
		t.Fatal(err)
	}

	_, err := h.loop.Run(context.Background(), userMsg("go"))
	if err == nil || !strings.Contains(err.Error(), "context budget") {
		t.Fatalf("error = %v, want the rejection surfaced", err)
	}
}

func TestTurnStoppingCanKeepTheTurnOpen(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("first"), textTurn("second")}, nil)

	calls := 0
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, e TurnStopping) error {
		calls++
		if calls == 1 {
			return errors.New("verification wants one more step")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Steps != 2 {
		t.Fatalf("steps = %d; the terminal checkpoint should have kept the turn open once", out.Steps)
	}
}

func TestMultiTurnSequenceNumberAndPromptDeduplication(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("turn 1 reply"), textTurn("turn 2 reply")}, nil)

	var turnsSeen []int
	if _, err := bus.OnWaterfall(h.bus, func(e PreStep, next bus.Next[PreStep]) PreStep {
		turnsSeen = append(turnsSeen, e.Turn)
		return next(e)
	}); err != nil {
		t.Fatal(err)
	}

	out1, err := h.loop.Run(context.Background(), userMsg("turn 1"))
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if out1.Final.Text() != "turn 1 reply" {
		t.Fatalf("turn 1 reply = %q", out1.Final.Text())
	}

	out2, err := h.loop.Run(context.Background(), userMsg("turn 2"))
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if out2.Final.Text() != "turn 2 reply" {
		t.Fatalf("turn 2 reply = %q", out2.Final.Text())
	}

	if len(turnsSeen) != 2 || turnsSeen[0] != 1 || turnsSeen[1] != 2 {
		t.Fatalf("turns seen = %v, want [1 2]", turnsSeen)
	}

	// Assert system prompts were not duplicated in the log
	sysCount := 0
	for _, ev := range h.log.Events() {
		if ev.Type == session.SystemPrompt {
			sysCount++
		}
	}
	if sysCount != 1 {
		t.Fatalf("logged %d system prompts across 2 turns, want 1", sysCount)
	}
}

func TestIdenticalToolCallsAreRefusedAfterTheLimit(t *testing.T) {
	const attempts = 6
	var turns []replay.Turn
	for i := 0; i < attempts; i++ {
		turns = append(turns, callTurn("look", `{}`))
	}
	turns = append(turns, textTurn("done"))

	// The budget is in units, not steps, and a refused step is charged
	// StallCost because it consumed a model call and produced nothing. This
	// test is about the repeat ledger rather than the ceiling, so it is given
	// enough budget for every attempt to be made and refused.
	h := build(t, turns, func(c *Config) { c.MaxSteps = (attempts + 1) * StallCost })
	var ran int
	registerCounter(t, h, "look", &ran)

	out, err := h.loop.Run(context.Background(), userMessage("go"))
	if err != nil {
		t.Fatal(err)
	}
	if ran != RepeatLimit {
		t.Fatalf("the handler ran %d times; only the first %d identical calls may reach it", ran, RepeatLimit)
	}
	if want := attempts - RepeatLimit; out.Repeated != want {
		t.Fatalf("Repeated = %d, want %d", out.Repeated, want)
	}
	// A refusal for going in circles is not a policy denial, and reporting it as
	// one would send an operator looking at the gate for a fault in the model.
	if out.Denied != 0 {
		t.Fatalf("Denied = %d; a repeat is not a policy denial", out.Denied)
	}
}

// TestADifferentArgumentIsNotARepeat guards the other side of the bound: the
// ledger keys on the arguments as the model sent them, so a call that genuinely
// varies is never refused. A limiter that caught legitimate work would be worse
// than none, because the model cannot tell the two refusals apart.
func TestADifferentArgumentIsNotARepeat(t *testing.T) {
	const attempts = 6
	var turns []replay.Turn
	for i := 0; i < attempts; i++ {
		turns = append(turns, callTurn("look", fmt.Sprintf(`{"p":"file%d"}`, i)))
	}
	turns = append(turns, textTurn("done"))

	h := build(t, turns, func(c *Config) { c.MaxSteps = attempts + 1 })
	var ran int
	registerCounter(t, h, "look", &ran)

	out, err := h.loop.Run(context.Background(), userMessage("go"))
	if err != nil {
		t.Fatal(err)
	}
	if ran != attempts {
		t.Fatalf("the handler ran %d times, want %d — distinct arguments are not repeats", ran, attempts)
	}
	if out.Repeated != 0 {
		t.Fatalf("Repeated = %d; distinct arguments must never be refused", out.Repeated)
	}
}

func callTurn(name, args string) replay.Turn {
	return replay.Turn{
		StopReason: llm.StopToolUse,
		Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCallBlock{ID: llm.CallID("c"), Name: name, Arguments: []byte(args)},
		}},
	}
}

func userMessage(text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: text}}}
}

func registerCounter(t *testing.T, h *harness, name string, ran *int) {
	t.Helper()
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{
			Name: name, Description: name,
			InputSchema: []byte(`{"type":"object","properties":{}}`),
		},
		Handler: func(context.Context, tools.Call) tools.Result {
			*ran++
			return tools.Result{Text: "same answer every time"}
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticContextCompactionTriggersUnderLoad(t *testing.T) {
	var toolCalls int
	turns := []replay.Turn{
		toolTurn("c1", "huge_output_tool", "{}"),
		textTurn("all done"),
	}

	h := build(t, turns, func(c *Config) {
		c.AssertInvariant = true
		c.MaxTokens = 2048
	})

	// Register huge_output_tool
	_ = h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{
			Name:        "huge_output_tool",
			Description: "returns big data",
			InputSchema: []byte(`{"type":"object","properties":{}}`),
		},
		Handler: func(context.Context, tools.Call) tools.Result {
			toolCalls++
			return tools.Result{Text: strings.Repeat("A very large block of output data\n", 200)}
		},
	})

	outcome, err := h.loop.Run(context.Background(), userMsg("run big query"))
	if err != nil {
		t.Fatalf("turn failed under auto-compaction and invariant assertion: %v", err)
	}
	if outcome.Steps != 2 {
		t.Fatalf("expected 2 steps, got %d", outcome.Steps)
	}
	if toolCalls != 1 {
		t.Fatalf("expected 1 tool call, got %d", toolCalls)
	}
}

// malformedTurn replays a response whose tool call could not be reconstructed,
// which is what a locally served model produces when it runs out of output
// tokens partway through emitting arguments.
func malformedTurn(name, reason string) replay.Turn {
	return replay.Turn{
		Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: ""}},
		Message:    llm.Message{Role: llm.RoleAssistant},
		StopReason: llm.StopMaxTokens,
		Usage:      llm.Usage{InputTokens: 20, OutputTokens: 1024},
		Malformed:  []llm.MalformedCall{{ID: "call_x", Name: name, Reason: reason}},
	}
}

func TestATruncatedToolCallDoesNotKillTheTurn(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("c1", "echo", `{"v":1}`),
		malformedTurn("echo", "the arguments were cut off mid-value"),
		textTurn("recovered and finished"),
	}, nil)
	registerEcho(t, h)

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("a truncated tool call ended the turn: %v", err)
	}
	if out.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1", out.Malformed)
	}
	if !out.TruncatedByTokens {
		t.Error("a max-tokens stop was not reported; it is indistinguishable from finishing")
	}
	if out.Steps != 3 {
		t.Errorf("Steps = %d, want 3 — the turn should have continued after the truncation", out.Steps)
	}
	if got := out.Final.Text(); got != "recovered and finished" {
		t.Errorf("final = %q", got)
	}
}

// The model has to be told what happened, or it will simply repeat the call.
func TestTheModelIsToldWhyItsCallWasUnreadable(t *testing.T) {
	h := build(t, []replay.Turn{
		malformedTurn("devcouncil_write_file", "the arguments were cut off mid-value"),
		textTurn("ok"),
	}, nil)

	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}

	var told bool
	var recorded bool
	for _, e := range h.log.Events() {
		switch e.Type {
		case session.MalformedToolCall:
			recorded = true
		case session.UserMessage:
			var data session.MessageData
			if json.Unmarshal(e.Data, &data) == nil &&
				strings.Contains(data.Message.Text(), "could not read") {
				told = true
				if !strings.Contains(data.Message.Text(), "devcouncil_write_file") {
					t.Error("the correction did not name the tool")
				}
				if !strings.Contains(data.Message.Text(), "output limit") {
					t.Error("the correction did not explain that the response was truncated")
				}
			}
		}
	}
	if !recorded {
		t.Error("the malformed call was not recorded in the evidence trail")
	}
	if !told {
		t.Error("the model was never told its call was unreadable")
	}
}

func TestAdapterCompensationIsRecordedAndReported(t *testing.T) {
	turn := textTurn("done")
	turn.Decoding = llm.DecodingReport{FallbackFormat: "qwen-xml", ReasoningReclassified: true}
	h := build(t, []replay.Turn{turn}, nil)

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Decoding.FallbackFormat != "qwen-xml" {
		t.Errorf("Decoding.FallbackFormat = %q", out.Decoding.FallbackFormat)
	}
	var logged bool
	for _, e := range h.log.Events() {
		if e.Type == session.DecodingCompensated {
			logged = true
		}
	}
	if !logged {
		t.Error("the harness compensated for the server's framing and said nothing")
	}
}

func registerEcho(t *testing.T, h *harness) {
	t.Helper()
	_ = h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{
			Name: "echo", Description: "echo",
			InputSchema: []byte(`{"type":"object","properties":{"v":{"type":"integer"}}}`),
		},
		Handler: func(context.Context, tools.Call) tools.Result {
			return tools.Result{Text: "echoed"}
		},
	})
}

// --- near-duplicate churn, and the budget that is aware of it ---

// registerScripted registers a tool that hands back the next text in outputs
// on each call and repeats the last one once they run out, so a test can say
// exactly what the model is told rather than inferring it.
func registerScripted(t *testing.T, h *harness, name string, readOnly bool, outputs []string, ran *int) {
	t.Helper()
	calls := 0
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{
			Name: name, Description: name,
			InputSchema: []byte(`{"type":"object","properties":{}}`),
		},
		ReadOnly: readOnly,
		Handler: func(context.Context, tools.Call) tools.Result {
			text := outputs[len(outputs)-1]
			if calls < len(outputs) {
				text = outputs[calls]
			}
			calls++
			*ran++
			return tools.Result{Text: text}
		},
	}); err != nil {
		t.Fatal(err)
	}
}

// churn is n calls to one read-only tool, every one with different arguments —
// the shape observed against the live model, where five greps for `sys|time`,
// `time\.`, `time\b`, `time.sleep` and `sys\.` each slipped past the verbatim
// ledger and each returned the same nothing.
func churn(name string, n int) []replay.Turn {
	var turns []replay.Turn
	for i := 0; i < n; i++ {
		turns = append(turns, callTurn(name, fmt.Sprintf(`{"pattern":"variant%d"}`, i)))
	}
	return turns
}

func TestNearDuplicateCallsAreInterruptedWhenNothingChanges(t *testing.T) {
	// One search that tells the model something, NoProgressLimit that tell it
	// nothing, and one more that is refused instead of run.
	const attempts = 2 + NoProgressLimit
	turns := append(churn("grep", attempts), textTurn("done"))
	h := build(t, turns, func(c *Config) { c.MaxSteps = 100 })

	var ran int
	registerScripted(t, h, "grep", true, []string{"no matches found"}, &ran)

	out, err := h.loop.Run(context.Background(), userMessage("remove the unused imports"))
	if err != nil {
		t.Fatal(err)
	}

	// The first search is information; the next NoProgressLimit repeat it. The
	// one after that is refused rather than run.
	if want := 1 + NoProgressLimit; ran != want {
		t.Fatalf("the tool ran %d times, want %d — the churn was not interrupted", ran, want)
	}
	if out.Stalled != 1 {
		t.Fatalf("Stalled = %d, want 1; distinct arguments walked straight around the limiter",
			out.Stalled)
	}
	if out.Repeated != 0 {
		t.Fatalf("Repeated = %d; no two of these calls were byte-identical", out.Repeated)
	}
	if out.NoProgressSteps != NoProgressLimit {
		t.Fatalf("NoProgressSteps = %d, want %d", out.NoProgressSteps, NoProgressLimit)
	}
}

// The refusal must reach the model as something it can act on, and must not
// appear anywhere as a decision the policy gate made.
func TestTheStallRefusalIsNotRecordedAsAPolicyDenial(t *testing.T) {
	turns := append(churn("grep", 5), textTurn("done"))
	h := build(t, turns, func(c *Config) { c.MaxSteps = 100 })

	var ran int
	registerScripted(t, h, "grep", true, []string{"no matches found"}, &ran)

	out, err := h.loop.Run(context.Background(), userMessage("go"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Stalled != 1 {
		t.Fatalf("Stalled = %d, want 1", out.Stalled)
	}
	if out.Denied != 0 {
		t.Fatalf("Denied = %d; the gate never saw these calls", out.Denied)
	}

	var told bool
	for _, e := range h.log.Events() {
		switch e.Type {
		case session.PolicyDenied:
			t.Fatal("a loop-detection refusal was written into the evidence trail as a policy " +
				"denial; the trail would claim a decision no rule made")
		case session.ToolResult:
			var data session.ToolResultData
			if err := json.Unmarshal(e.Data, &data); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(data.Text, "was not run") {
				told = true
				if !data.IsError {
					t.Error("the refusal was handed back as an ordinary success; the model cannot " +
						"tell it apart from the tool returning nothing")
				}
				if !strings.Contains(data.Text, "grep") {
					t.Error("the refusal did not name the call it refused")
				}
			}
		}
	}
	if !told {
		t.Fatal("the call was skipped without telling the model why")
	}
}

// A refusal is information the model has not had before, so it gets a clean run
// at the task afterwards. Holding the streak at the limit would refuse every
// remaining call, and a turn that can no longer call a tool has ended without
// saying so.
func TestTheModelCanKeepWorkingAfterAStallRefusal(t *testing.T) {
	turns := append(churn("grep", 5), callTurn("write", `{"path":"a.go"}`), textTurn("done"))
	h := build(t, turns, func(c *Config) { c.MaxSteps = 100 })

	var greps, writes int
	registerScripted(t, h, "grep", true, []string{"no matches found"}, &greps)
	registerScripted(t, h, "write", false, []string{"written"}, &writes)

	out, err := h.loop.Run(context.Background(), userMessage("go"))
	if err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("the write ran %d times; the refusal must not close the turn to further work", writes)
	}
	if out.Final.Text() != "done" {
		t.Fatalf("final = %q, want the turn to have finished normally", out.Final.Text())
	}
	if out.TruncatedBySteps {
		t.Fatal("the turn was truncated; it should have ended on its own")
	}
}

// --- the limiter must stay off real work ---

// Re-reading a file after writing it is the ordinary edit loop, and the reads
// return the same bytes every time. A limiter that refused this would be worse
// than none, because the model cannot tell that refusal from one it should heed.
func TestTheEditLoopIsNeverRefused(t *testing.T) {
	turns := []replay.Turn{
		callTurn("read", `{"path":"a.go"}`),
		callTurn("write", `{"path":"a.go"}`),
		callTurn("read", `{"path":"a.go"}`),
		callTurn("write", `{"path":"a.go"}`),
		callTurn("read", `{"path":"a.go"}`),
		callTurn("write", `{"path":"a.go"}`),
		textTurn("edited"),
	}
	h := build(t, turns, func(c *Config) { c.MaxSteps = 100 })

	var reads, writes int
	// Every read hands back the same text and every write the same
	// confirmation: the worst case for a detector keyed on results.
	registerScripted(t, h, "read", true, []string{"package main"}, &reads)
	registerScripted(t, h, "write", false, []string{"ok"}, &writes)

	out, err := h.loop.Run(context.Background(), userMessage("edit a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if reads != 3 || writes != 3 {
		t.Fatalf("reads = %d, writes = %d; want 3 and 3 — every call was legitimate work", reads, writes)
	}
	if out.Stalled != 0 {
		t.Fatalf("Stalled = %d; re-reading a file you just wrote is progress", out.Stalled)
	}
	if out.Repeated != 0 {
		t.Fatalf("Repeated = %d; three reads of one path is inside RepeatLimit", out.Repeated)
	}
}

// Re-running a check after a fix looks identical to churn until it passes: the
// failing output repeats verbatim. The mutation between the runs is what makes
// it progress.
func TestRerunningACheckAroundAFixIsProgress(t *testing.T) {
	turns := []replay.Turn{
		callTurn("verify", `{}`),
		callTurn("patch", `{"path":"a.go"}`),
		callTurn("verify", `{}`),
		callTurn("patch", `{"path":"b.go"}`),
		callTurn("verify", `{}`),
		textTurn("green"),
	}
	h := build(t, turns, func(c *Config) { c.MaxSteps = 100 })

	var verifies, patches int
	registerScripted(t, h, "verify", true,
		[]string{"FAIL: 2 gaps", "FAIL: 2 gaps", "PASS"}, &verifies)
	registerScripted(t, h, "patch", false, []string{"patched"}, &patches)

	out, err := h.loop.Run(context.Background(), userMessage("make it pass"))
	if err != nil {
		t.Fatal(err)
	}
	if verifies != 3 || patches != 2 {
		t.Fatalf("verifies = %d, patches = %d, want 3 and 2", verifies, patches)
	}
	if out.Stalled != 0 {
		t.Fatalf("Stalled = %d; a check re-run after a change is not churn", out.Stalled)
	}
}

// Exploration that keeps finding new things is progress however long it runs.
func TestExploringNewFilesIsNeverRefused(t *testing.T) {
	const files = 20
	var turns []replay.Turn
	var outputs []string
	for i := 0; i < files; i++ {
		turns = append(turns, callTurn("read", fmt.Sprintf(`{"path":"f%d.go"}`, i)))
		outputs = append(outputs, fmt.Sprintf("contents of f%d.go", i))
	}
	turns = append(turns, textTurn("mapped"))

	h := build(t, turns, func(c *Config) { c.MaxSteps = 100 })
	var reads int
	registerScripted(t, h, "read", true, outputs, &reads)

	out, err := h.loop.Run(context.Background(), userMessage("map the package"))
	if err != nil {
		t.Fatal(err)
	}
	if reads != files {
		t.Fatalf("read ran %d times, want %d — genuinely new information is progress", reads, files)
	}
	if out.Stalled != 0 || out.NoProgressSteps != 0 {
		t.Fatalf("Stalled = %d, NoProgressSteps = %d; every read returned something new",
			out.Stalled, out.NoProgressSteps)
	}
}

// --- the budget ---

func TestATurnThatFinishesUnderBudgetIsNotTruncated(t *testing.T) {
	h := build(t, []replay.Turn{
		callTurn("read", `{"path":"a.go"}`),
		callTurn("write", `{"path":"a.go"}`),
		textTurn("done"),
	}, func(c *Config) { c.MaxSteps = 500 })

	var reads, writes int
	registerScripted(t, h, "read", true, []string{"package main"}, &reads)
	registerScripted(t, h, "write", false, []string{"ok"}, &writes)

	out, err := h.loop.Run(context.Background(), userMessage("go"))
	if err != nil {
		t.Fatal(err)
	}
	if out.TruncatedBySteps {
		t.Fatal("a three-step turn reported the step ceiling")
	}
	if out.Steps != 3 || out.BudgetSpent != 3 {
		t.Fatalf("Steps = %d, BudgetSpent = %d, want 3 and 3", out.Steps, out.BudgetSpent)
	}
}

// A turn that keeps getting somewhere gets every step the ceiling allows, and
// not one more. This is the hard bound: because the cheapest step costs one
// unit, no turn can run longer than MaxSteps whatever it does.
func TestAProgressingTurnGetsTheWholeCeilingAndNoMore(t *testing.T) {
	for _, ceiling := range []int{1, 2, 7} {
		var turns []replay.Turn
		var outputs []string
		for i := 0; i < ceiling+5; i++ {
			turns = append(turns, callTurn("read", fmt.Sprintf(`{"path":"f%d"}`, i)))
			outputs = append(outputs, fmt.Sprintf("contents %d", i))
		}
		h := build(t, turns, func(c *Config) { c.MaxSteps = ceiling })
		var reads int
		registerScripted(t, h, "read", true, outputs, &reads)

		out, err := h.loop.Run(context.Background(), userMessage("go"))
		if err != nil {
			t.Fatalf("ceiling %d: %v", ceiling, err)
		}
		if out.Steps != ceiling {
			t.Fatalf("ceiling %d: Steps = %d, want exactly the ceiling", ceiling, out.Steps)
		}
		if !out.TruncatedBySteps {
			t.Fatalf("ceiling %d: the turn ran out of steps and did not say so", ceiling)
		}
	}
}

// The point of a progress-aware budget: a step that changed nothing observable
// does not cost the same as one that did, so a turn going in circles reaches
// the end of the ceiling sooner than one doing the work.
func TestASpinningTurnExhaustsTheBudgetSoonerThanAWorkingOne(t *testing.T) {
	const ceiling = 24

	steps := func(readOnly bool, sameAnswer bool) Outcome {
		t.Helper()
		var turns []replay.Turn
		var outputs []string
		for i := 0; i < ceiling+5; i++ {
			turns = append(turns, callTurn("look", fmt.Sprintf(`{"pattern":"v%d"}`, i)))
			if sameAnswer {
				outputs = append(outputs, "no matches found")
			} else {
				outputs = append(outputs, fmt.Sprintf("match %d", i))
			}
		}
		h := build(t, turns, func(c *Config) { c.MaxSteps = ceiling })
		var ran int
		registerScripted(t, h, "look", readOnly, outputs, &ran)
		out, err := h.loop.Run(context.Background(), userMessage("go"))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	working := steps(true, false)
	spinning := steps(true, true)

	if !working.TruncatedBySteps || !spinning.TruncatedBySteps {
		t.Fatalf("both turns should have hit the ceiling: working=%+v spinning=%+v",
			working.TruncatedBySteps, spinning.TruncatedBySteps)
	}
	if working.Steps != ceiling {
		t.Fatalf("the working turn got %d steps, want the full ceiling of %d", working.Steps, ceiling)
	}
	if spinning.Steps >= working.Steps {
		t.Fatalf("spinning turn ran %d steps and working turn %d; a step that changed nothing "+
			"cost the same as one that did", spinning.Steps, working.Steps)
	}
	if spinning.Steps > ceiling {
		t.Fatalf("spinning turn ran %d steps against a ceiling of %d; the ceiling is not hard",
			spinning.Steps, ceiling)
	}
	if spinning.NoProgressSteps == 0 {
		t.Fatal("the spinning turn reported no steps without progress")
	}
}

func TestDynamicToolActivationDuringTurn(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "devcouncil_activate_tools", `{"tools":["specialized"]}`),
		toolTurn("call-2", "specialized_tool", `{"action":"execute"}`),
		textTurn("done"),
	}, nil)

	if err := h.tools.Register(tools.Tool{
		Schema:   llm.ToolSchema{Name: "devcouncil_activate_tools", Description: "activate tools"},
		ReadOnly: true,
		Group:    tools.GroupCore,
		Handler: func(_ context.Context, call tools.Call) tools.Result {
			_, err := h.tools.Activate("specialized")
			if err != nil {
				return tools.Errorf("activation failed: %v", err)
			}
			return tools.Result{Text: "activated"}
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.tools.Register(tools.Tool{
		Schema:   llm.ToolSchema{Name: "specialized_tool", Description: "specialized tool"},
		ReadOnly: true,
		Group:    "specialized",
		Extended: true,
		Handler: func(_ context.Context, call tools.Call) tools.Result {
			return tools.Result{Text: "specialized output"}
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Enable dynamic mode explicitly with only devcouncil_activate_tools
	h.tools.EnableDynamic("devcouncil_activate_tools")

	// Before run: only devcouncil_activate_tools is active
	if len(h.tools.ActiveSchemas()) != 1 {
		t.Fatalf("expected 1 active schema before run, got %d", len(h.tools.ActiveSchemas()))
	}

	out, err := h.loop.Run(context.Background(), userMsg("run workflow"))
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if out.Steps != 3 || out.ToolCalls != 2 {
		t.Fatalf("outcome = %+v, want 3 steps and 2 tool calls", out)
	}

	// After run: specialized_tool was dynamically activated and is in active schemas
	if len(h.tools.ActiveSchemas()) != 2 {
		t.Fatalf("expected 2 active schemas after activation, got %d", len(h.tools.ActiveSchemas()))
	}
}

// TestQualifiedAllowIsNotADenial holds the line between "policy had something
// to say" and "policy refused".
//
// devcouncil.annotate carries the rule that *would* have blocked a write onto
// results the posture or a grant allowed, so the log can say why the allow was
// reached. Classifying on the presence of a Rule alone read those allows as
// refusals: in yolo posture, where nothing is refused by construction, a write
// that plainly landed was counted in Outcome.Denied and written to the evidence
// trail as session.PolicyDenied. The gate's own Report draws this line at
// Blocked(); the loop must draw the same one.
func TestQualifiedAllowIsNotADenial(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "write", `{"path":"hello.txt"}`),
		textTurn("done"),
	}, nil)

	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "write"},
		Handler: func(context.Context, tools.Call) tools.Result {
			// Exactly what annotate() returns for a demoted allow: the write
			// succeeded, and it carries the rule that would have stopped it.
			return tools.Result{
				Text:     "wrote hello.txt (5 bytes)",
				IsError:  false,
				Rule:     "task.absent",
				Severity: "soft",
				Demoted:  "policy.file.mode=off (harness.posture=yolo/override)",
			}
		},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("write hello.txt"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Denied != 0 {
		t.Fatalf("Denied = %d; a write that succeeded was not refused by the gate", out.Denied)
	}
	if out.Qualified != 1 {
		t.Fatalf("Qualified = %d, want 1; a demoted allow must still be counted", out.Qualified)
	}

	var denials, qualified int
	for _, event := range h.log.Events() {
		switch event.Type {
		case session.PolicyDenied:
			denials++
		case session.PolicyQualified:
			qualified++
			var data session.QualificationData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			// The demotion is the whole point of the record: without it a
			// resumed session cannot tell why the write was allowed.
			if data.Rule != "task.absent" || data.Demoted == "" {
				t.Fatalf("qualification event = %+v", data)
			}
		}
	}
	if denials != 0 {
		t.Fatalf("evidence trail recorded %d denials for a successful write", denials)
	}
	if qualified != 1 {
		t.Fatalf("evidence trail recorded %d qualifications, want 1", qualified)
	}
}

// cutOffTurn replays a final answer that ran out of output budget mid-sentence:
// prose, no tool calls, and a max-tokens stop. This is what a locally served
// model returns when the answer is longer than llm.local.max_output_tokens.
func cutOffTurn(text string) replay.Turn {
	return replay.Turn{
		Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: text}},
		Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: text}}},
		StopReason: llm.StopMaxTokens,
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 16384},
	}
}

// TestATruncatedFinalAnswerIsNotAFinishedTurn covers the half of truncation the
// turn cannot recover from.
//
// A mid-turn truncation gets another step to put itself right, and
// TestATruncatedToolCallDoesNotKillTheTurn holds that. A truncated *final*
// answer gets nothing: there are no tool calls, so the loop breaks on its
// natural-stop path and the turn ends. The caller is handed half a sentence
// that is byte-identical in shape to a complete one, which is the whole reason
// the outcome has to say which it was.
func TestATruncatedFinalAnswerIsNotAFinishedTurn(t *testing.T) {
	h := build(t, []replay.Turn{
		cutOffTurn("The fix is to change the loop so that it"),
	}, nil)

	out, err := h.loop.Run(context.Background(), userMsg("explain the fix"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.TruncatedByTokens {
		t.Error("TruncatedByTokens was not set for a max-tokens stop")
	}
	if !out.FinalTruncated {
		t.Fatal("FinalTruncated was not set: a turn that ended mid-sentence " +
			"is indistinguishable from one that finished")
	}
}

// TestRecoveredTruncationDoesNotMarkTheAnswerCutOff is the other side of it.
// The turn hit the cap partway through and then finished properly, so the
// answer the caller receives is whole. Marking that turn as cut off would make
// the signal useless — it would fire on every run that ever hit the cap.
func TestRecoveredTruncationDoesNotMarkTheAnswerCutOff(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("c1", "echo", `{"v":1}`),
		malformedTurn("echo", "the arguments were cut off mid-value"),
		textTurn("recovered and finished"),
	}, nil)
	registerEcho(t, h)

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.TruncatedByTokens {
		t.Error("TruncatedByTokens should still record that the cap was hit")
	}
	if out.FinalTruncated {
		t.Error("FinalTruncated was set on a turn whose final answer was complete")
	}
}

// TestDemotedAllowThatFailedOnItsOwnIsNotADenial is the case the first fix at
// this seam left open.
//
// TestQualifiedAllowIsNotADenial covers a demoted allow that succeeded, and
// narrowing the classifier to "Rule and IsError" was enough to satisfy it. It
// is not enough for a demoted allow that then failed for a reason of its own.
// Under --yolo the gate allows every shell command and annotates it with the
// rule that would have stopped it; when the command itself exits non-zero, the
// result carries a Rule and IsError, and the run reported it as refused by a
// gate that had in fact allowed it.
//
// Observed in a real --yolo turn: `cd search && …` failed on a bad working
// directory and the summary read "1 of 9 tool call(s) were refused by the
// gate". Nothing can be refused in yolo posture — the true count is zero by
// construction — so the line told the operator a gate had contained something
// when nothing was contained.
func TestDemotedAllowThatFailedOnItsOwnIsNotADenial(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "exec", `{"command":"cd search"}`),
		textTurn("done"),
	}, nil)

	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "exec"},
		Handler: func(context.Context, tools.Call) tools.Result {
			// The gate allowed this: Blocked is false, and the demotion says
			// which flag let it through. The command then failed by itself, so
			// IsError is true and the rule that would have blocked it is still
			// carried for the record.
			return tools.Result{
				Text:     "exit code 1\nsh: line 0: cd: search: No such file or directory",
				IsError:  true,
				Rule:     "command.no_lease",
				Severity: "soft",
				Demoted:  "policy.command.mode=off (harness.posture=yolo/override)",
				Degraded: []string{"policy.hard_rules.disabled"},
			}
		},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("run the tests"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Denied != 0 {
		t.Fatalf("Denied = %d, want 0; the gate allowed this command and it failed on its own exit code — "+
			"reporting a refusal that did not happen tells an operator a gate contained something when "+
			"nothing was contained", out.Denied)
	}
	if out.Qualified != 1 {
		t.Fatalf("Qualified = %d, want 1; an allow reached by a demotion must still be counted as qualified", out.Qualified)
	}

	for _, event := range h.log.Events() {
		if event.Type == session.PolicyDenied {
			t.Fatalf("a session.PolicyDenied was written for a call the gate allowed: %+v", event)
		}
	}
}

// TestGateRefusalIsStillCountedWhenOtherChecksDidNotRun guards the mirror of
// the bug above: ordering Qualified ahead of the refusal would have hidden a
// real denial behind an unrelated qualification.
//
// Hard rules can be off while a soft rule still enforces, so a genuine refusal
// can arrive with a non-empty Degraded list. It is still a refusal.
func TestGateRefusalIsStillCountedWhenOtherChecksDidNotRun(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "write", `{"path":"vendor/x.go"}`),
		textTurn("done"),
	}, nil)

	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "write"},
		Handler: func(context.Context, tools.Call) tools.Result {
			return tools.Result{
				Text:     `{"rule":"scope.unplanned","allowed":false}`,
				IsError:  true,
				Blocked:  true,
				Rule:     "scope.unplanned",
				Severity: "soft",
				Degraded: []string{"policy.hard_rules.disabled"},
			}
		},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("write vendor/x.go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Denied != 1 {
		t.Fatalf("Denied = %d, want 1; a refusal that also reports skipped checks is still a refusal", out.Denied)
	}
	if out.Qualified != 0 {
		t.Fatalf("Qualified = %d, want 0; a refused call was counted as an allow", out.Qualified)
	}
}

// reasoningOnlyTurn is the shape a runaway local model produced: a settled
// message carrying nothing but a reasoning block, no tool calls, and a stop
// reason of "end turn" rather than "max tokens" — even though generation had in
// fact run to the output cap.
func reasoningOnlyTurn(text string) replay.Turn {
	return replay.Turn{
		Chunks:     []llm.Chunk{{Kind: llm.ChunkReasoning, Text: text}},
		Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ReasoningBlock{Text: text}}},
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 3927, OutputTokens: 900},
	}
}

// TestATurnThatEndsWithNoAnswerIsNotASuccess.
//
// Observed against a local DFlash2-accelerated server: the model looped inside
// its reasoning block, emitted exactly the 16,384-token output cap over 495
// seconds, and the server reported finish_reason="stop" rather than "length".
// FinalTruncated keys on the stop reason, so it stayed false; the response
// carried no tool calls, so the loop took it as a natural stop. The turn
// changed no files, produced no answer, and `manvi run` exited 0 — which a
// benchmark or a CI step reads as the work having been done.
//
// The harness cannot fix the server's stop reason, but it does not need it:
// whether an answer arrived is something it can see for itself.
func TestATurnThatEndsWithNoAnswerIsNotASuccess(t *testing.T) {
	h := build(t, []replay.Turn{
		reasoningOnlyTurn("tracing the bug: xs = 1, 3, 5, 7, 9, 1, 3, 5, 7, 9, 1, 3, 5, 7, 9,"),
	}, nil)

	out, err := h.loop.Run(context.Background(), userMsg("fix the failing test"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.FinalEmpty {
		t.Fatal("FinalEmpty = false; a turn whose last response carried no text and no tool call " +
			"produced nothing, and reporting it as a clean finish is how a benchmark records " +
			"unfinished work as done")
	}
	// Deliberately nowhere near the budget, so this asserts FinalEmpty on its
	// own rather than riding on the cap detection that also caught the real
	// case. An answerless turn is a dead end whether or not it ran long.
	if out.FinalTruncated {
		t.Error("FinalTruncated = true for a response well under the output budget")
	}
}

// The mirror: an ordinary answer must not trip the new signal.
func TestATurnThatEndsWithAnAnswerIsNotReportedEmpty(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("fixed the bound and the test passes")}, nil)

	out, err := h.loop.Run(context.Background(), userMsg("fix the failing test"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.FinalEmpty {
		t.Fatal("FinalEmpty = true for a turn that ended with a real answer")
	}
}

// Reasoning alongside a real answer is still an answer — the reasoning is
// stripped before the caller sees it, so only the text decides.
func TestReasoningBesideAnAnswerDoesNotCountAsEmpty(t *testing.T) {
	h := build(t, []replay.Turn{{
		Chunks: []llm.Chunk{{Kind: llm.ChunkText, Text: "done"}},
		Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "long deliberation"},
			llm.TextBlock{Text: "done"},
		}},
		StopReason: llm.StopEndTurn,
	}}, nil)

	out, err := h.loop.Run(context.Background(), userMsg("fix it"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.FinalEmpty {
		t.Fatal("FinalEmpty = true for a turn that answered after reasoning")
	}
}

// TestAMislabelledOutputCapIsStillDetected.
//
// A server that generates exactly the requested budget and then reports
// finish_reason="stop" is claiming the model finished. Measured against omlx
// 0.6.2: three responses ran to exactly 16,384 tokens and every one was
// labelled "stop". The harness set the budget and gets the usage back, so it
// can check the claim rather than take it.
func TestAMislabelledOutputCapIsStillDetected(t *testing.T) {
	capped := replay.Turn{
		Chunks:     []llm.Chunk{{Kind: llm.ChunkReasoning, Text: "looping"}},
		Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "looping"}}},
		StopReason: llm.StopEndTurn, // the server's claim, and it is wrong
		Usage:      llm.Usage{InputTokens: 3927, OutputTokens: 16384},
	}
	h := build(t, []replay.Turn{capped}, func(c *Config) { c.MaxTokens = 16384 })

	out, err := h.loop.Run(context.Background(), userMsg("fix the failing test"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.FinalTruncated {
		t.Error("FinalTruncated = false; the response used the whole 16384-token budget, which the harness " +
			"itself set — believing the server's \"stop\" over its own arithmetic is how a runaway " +
			"generation was recorded as a model that had finished")
	}
	if !out.TruncatedByTokens {
		t.Error("TruncatedByTokens = false for a response that consumed the entire output budget")
	}
}

// An ordinary short answer must not be read as capped.
func TestAnAnswerWellUnderTheCapIsNotReportedTruncated(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("done")}, func(c *Config) { c.MaxTokens = 16384 })

	out, err := h.loop.Run(context.Background(), userMsg("fix it"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.FinalTruncated || out.TruncatedByTokens {
		t.Fatalf("FinalTruncated=%v TruncatedByTokens=%v; a short answer is not a capped one",
			out.FinalTruncated, out.TruncatedByTokens)
	}
}

// TestTheOutputCapIsDetectedWhenTheBudgetCameFromTheAdapter.
//
// `manvi run` sets Config.MaxTokens to 0 on purpose: zero means "whatever the
// adapter defaults to", not "no budget". The first version of hitOutputCap
// compared against Config.MaxTokens directly, so on that path — the one the
// benchmark and every headless run take — it could never fire.
//
// Caught by the benchmark rather than by this suite: a turn ran to exactly the
// declared 16,384-token budget in 410 seconds and still exited 0.
func TestTheOutputCapIsDetectedWhenTheBudgetCameFromTheAdapter(t *testing.T) {
	capped := replay.Turn{
		Chunks: []llm.Chunk{{Kind: llm.ChunkText, Text: "looping"}},
		Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.TextBlock{Text: "looping"},
		}},
		StopReason: llm.StopEndTurn, // the server's claim, and it is wrong
		Usage:      llm.Usage{InputTokens: 3678, OutputTokens: 16384},
	}
	// Exactly the headless shape: nothing configured, the budget declared by
	// the model's capability.
	declared := caps("test-model")
	declared.MaxOutputTokens = 16384
	h := buildOn(t, declared, []replay.Turn{capped}, func(c *Config) { c.MaxTokens = 0 })

	out, err := h.loop.Run(context.Background(), userMsg("fix the failing test"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.FinalTruncated {
		t.Error("FinalTruncated = false; the response used the entire declared 16384-token budget. " +
			"Config.MaxTokens is 0 on this path and 0 means the adapter's default, not no limit — " +
			"checking against it compares the response to a number nobody used")
	}
	if !out.TruncatedByTokens {
		t.Error("TruncatedByTokens = false for a response that consumed the whole declared budget")
	}
}

// TestDecodingCompensationsAccumulateAcrossTheTurn.
//
// Outcome.Decoding is documented as reporting compensations "across the turn",
// and the loop assigned it per step. A turn whose first step disproved a
// declared reasoning prefill and whose later step recovered a tool call from
// text ended with the prefill flag erased — so the notice that exists to get
// llm.local.assume_reasoning_prefill turned off did not print, on exactly the
// turns where it was needed, and the condition recurred on the next turn.
func TestDecodingCompensationsAccumulateAcrossTheTurn(t *testing.T) {
	prefillStep := replay.Turn{
		Chunks: []llm.Chunk{{Kind: llm.ChunkToolCallStart, ToolCallID: "c1", ToolName: "noop"}},
		Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCallBlock{ID: "c1", Name: "noop", Arguments: json.RawMessage(`{}`)},
		}},
		StopReason: llm.StopToolUse,
		Decoding:   llm.DecodingReport{PrefillDisproved: true},
	}
	fallbackStep := replay.Turn{
		Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: "done"}},
		Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}}},
		StopReason: llm.StopEndTurn,
		Decoding:   llm.DecodingReport{FallbackFormat: "qwen-xml"},
	}
	h := build(t, []replay.Turn{prefillStep, fallbackStep}, nil)
	if err := h.tools.Register(tools.Tool{
		Schema:  llm.ToolSchema{Name: "noop"},
		Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "ok"} },
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("do the thing"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Decoding.PrefillDisproved {
		t.Error("PrefillDisproved was erased by a later step's report; the operator is never told " +
			"their prefill declaration is wrong, and the next turn loses its answers the same way")
	}
	if out.Decoding.FallbackFormat != "qwen-xml" {
		t.Errorf("FallbackFormat = %q, want %q; the later compensation must survive too",
			out.Decoding.FallbackFormat, "qwen-xml")
	}
}

// The first fallback name is the one kept, so the earliest evidence is the one
// an operator can still find in the log.
func TestTheFirstFallbackFormatIsTheOneReported(t *testing.T) {
	step := func(format string, stop llm.StopReason, blocks ...llm.ContentBlock) replay.Turn {
		return replay.Turn{
			Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: "x"}},
			Message:    llm.Message{Role: llm.RoleAssistant, Content: blocks},
			StopReason: stop,
			Decoding:   llm.DecodingReport{FallbackFormat: format},
		}
	}
	h := build(t, []replay.Turn{
		step("hermes-json", llm.StopToolUse, llm.ToolCallBlock{ID: "c1", Name: "noop", Arguments: json.RawMessage(`{}`)}),
		step("qwen-xml", llm.StopEndTurn, llm.TextBlock{Text: "done"}),
	}, nil)
	if err := h.tools.Register(tools.Tool{
		Schema:  llm.ToolSchema{Name: "noop"},
		Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "ok"} },
	}); err != nil {
		t.Fatal(err)
	}
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Decoding.FallbackFormat != "hermes-json" {
		t.Errorf("FallbackFormat = %q, want %q", out.Decoding.FallbackFormat, "hermes-json")
	}
}

// TestTheAppliedCapIsPreferredOverTheDeclaredCeiling.
//
// Capability.MaxOutputTokens is the model's ceiling; the request carries
// whatever the adapter resolved. On three of the four providers those are
// different numbers — anthropic declares 128000 and sends 8192, xai and gemini
// declare nothing at all — so a check written against the ceiling never fires.
// Only the adapter knows what went on the wire, so it reports it.
func TestTheAppliedCapIsPreferredOverTheDeclaredCeiling(t *testing.T) {
	capped := replay.Turn{
		Chunks: []llm.Chunk{{Kind: llm.ChunkText, Text: "cut off here"}},
		Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.TextBlock{Text: "cut off here"},
		}},
		StopReason:       llm.StopEndTurn, // the server's claim, and it is wrong
		Usage:            llm.Usage{InputTokens: 500, OutputTokens: 8192},
		MaxTokensApplied: 8192, // what the adapter actually sent
	}
	// The declared ceiling is far higher, exactly as anthropic's is.
	declared := caps("test-model")
	declared.MaxOutputTokens = 128000
	h := buildOn(t, declared, []replay.Turn{capped}, func(c *Config) { c.MaxTokens = 0 })

	out, err := h.loop.Run(context.Background(), userMsg("write something long"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.FinalTruncated {
		t.Error("FinalTruncated = false; the response used the whole 8192-token bound the adapter sent. " +
			"Comparing against the declared 128000 ceiling instead means a truncated turn on anthropic, " +
			"xai or gemini reports as a model that finished")
	}
}

// A genuinely unbounded request reports zero, and zero must mean "cannot say"
// rather than "no limit" — otherwise every short answer reads as capped.
func TestAnUnboundedRequestIsNotReportedAsCapped(t *testing.T) {
	unbounded := replay.Turn{
		Chunks:           []llm.Chunk{{Kind: llm.ChunkText, Text: "done"}},
		Message:          llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}}},
		StopReason:       llm.StopEndTurn,
		Usage:            llm.Usage{InputTokens: 10, OutputTokens: 4},
		MaxTokensApplied: 0,
	}
	noCeiling := caps("test-model")
	noCeiling.MaxOutputTokens = 0
	h := buildOn(t, noCeiling, []replay.Turn{unbounded}, func(c *Config) { c.MaxTokens = 0 })

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.FinalTruncated || out.TruncatedByTokens {
		t.Fatalf("FinalTruncated=%v TruncatedByTokens=%v; an unbounded request has no cap to hit",
			out.FinalTruncated, out.TruncatedByTokens)
	}
}

// TestAPipelinePanicIsCountedAsARefusalNotAnAllow.
//
// stagePanic sets Degraded, which makes Result.Qualified() true — deliberately,
// so the session log records the outcome as not cleanly reached. But at the
// outcome level Qualified renders as "allowed but not on the rules alone", for
// a call whose own text says it "was refused because a stage that could not
// finish is not a stage that approved". Under --quiet that line was the only
// trace, and it said the opposite of what happened.
func TestAPipelinePanicIsCountedAsARefusalNotAnAllow(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "boom", `{}`),
		textTurn("done"),
	}, nil)
	if err := h.tools.Register(tools.Tool{
		Schema:  llm.ToolSchema{Name: "boom"},
		Handler: func(context.Context, tools.Call) tools.Result { panic("kaboom") },
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("run it"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Panicked != 1 {
		t.Errorf("Panicked = %d, want 1; a stage that could not finish refused the call", out.Panicked)
	}
	if out.Qualified != 0 {
		t.Errorf("Qualified = %d, want 0; a panicked call was not allowed, and reporting it as "+
			"\"allowed but not on the rules alone\" is the opposite of what happened", out.Qualified)
	}
	if out.Denied != 0 {
		t.Errorf("Denied = %d, want 0; the gate did not refuse this — the harness broke", out.Denied)
	}
}

// TestAnOverflowThatCompactionCannotFixIsReportedOnTheOutcome.
//
// reportOverflow's comment says history exceeding the budget after everything
// compactable has been compacted "must not happen silently". Two audits found
// that it did: the event it appends has no case in ui.Project, which is the
// only bridge from the session log to either face, and there was no Outcome
// field for it.
//
// The notice was added and tested against a hand-built Outcome — which left the
// wiring from the actual overflow condition to that field completely untested.
// Coverage put reportOverflow at 0.0%. This drives the real path.
func TestAnOverflowThatCompactionCannotFixIsReportedOnTheOutcome(t *testing.T) {
	// A window far too small for the prompt below, so compaction runs out of
	// anything to shorten. ProtectedTail shields the last six messages, so a
	// short turn has no eligible result at all — the case the comment calls
	// "easy to reach and previously silent".
	tiny := caps("test-model")
	tiny.ContextWindow = 2048
	tiny.MaxOutputTokens = 256

	h := buildOn(t, tiny, []replay.Turn{textTurn("done")}, func(c *Config) {
		c.MaxTokens = 0
		c.SystemPrompt = strings.Repeat("system instructions that will not fit. ", 400)
	})

	out, err := h.loop.Run(context.Background(),
		userMsg(strings.Repeat("please consider this very long request. ", 400)))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.ContextOverflowed {
		t.Fatal("ContextOverflowed = false for a turn whose history could not be made to fit. " +
			"The turn still ran, and the server's own truncation is what the operator would " +
			"have had to explain")
	}
}

// The ordinary case must stay quiet, or the signal is noise.
func TestATurnThatFitsDoesNotReportAnOverflow(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("done")}, nil)
	out, err := h.loop.Run(context.Background(), userMsg("short request"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.ContextOverflowed {
		t.Fatal("ContextOverflowed = true for a turn that fits comfortably")
	}
}

// TestAPipelinePanicStaysInTheSessionLog.
//
// stagePanic sets Degraded, which made Result.Qualified() true, which appended
// a session.PolicyQualified carrying the panic. tools.stagePanic's own comment
// asserts that: "Degraded is set in every case, so Result.Qualified() is true
// and the session log records that this outcome was not reached by the rules
// running cleanly."
//
// Giving the panic its own switch case took that entry away with it, leaving
// Outcome.Panicked in memory and a stack on stderr — both gone from a resumed
// or replayed session. The counter was the point of the case; losing the record
// was not.
func TestAPipelinePanicStaysInTheSessionLog(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "boom", `{}`),
		textTurn("done"),
	}, nil)
	if err := h.tools.Register(tools.Tool{
		Schema:  llm.ToolSchema{Name: "boom"},
		Handler: func(context.Context, tools.Call) tools.Result { panic("kaboom") },
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.loop.Run(context.Background(), userMsg("run it")); err != nil {
		t.Fatalf("run: %v", err)
	}

	var recorded bool
	for _, e := range h.log.Events() {
		if e.Type != session.PolicyQualified {
			continue
		}
		var d session.QualificationData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("qualification payload: %v", err)
		}
		for _, g := range d.Degraded {
			if strings.Contains(g, "panicked") {
				recorded = true
			}
		}
	}
	if !recorded {
		t.Fatal("a tool pipeline panic left no durable record; Outcome.Panicked is in memory and " +
			"the stack is on stderr, so a resumed session shows a turn where nothing went wrong")
	}
}

// TestATerminalFlagDoesNotSurviveIntoALaterStep.
//
// FinalTruncated and FinalEmpty describe the response that ended the turn. Both
// are set in the no-tool-calls branch, and that branch is not always where the
// turn ends: the terminal checkpoint can keep it open, and the turn can then
// end at the step ceiling instead — which breaks out at the top of the loop
// without passing through the branch again. The earlier step's value survived
// and described a response that was never the last one.
//
// A first version of this test ended the turn through the branch a second
// time, which reassigns the flag and so passed with or without the fix. It had
// to end on the ceiling to exercise the bug at all.
func TestATerminalFlagDoesNotSurviveIntoALaterStep(t *testing.T) {
	// Step 1: no text, no tool calls — sets FinalEmpty. The checkpoint reopens
	// the turn, and every later step calls a tool, so the turn ends on the
	// budget rather than on another pass through that branch.
	empty := replay.Turn{
		Chunks:     []llm.Chunk{{Kind: llm.ChunkReasoning, Text: "thinking"}},
		Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "thinking"}}},
		StopReason: llm.StopEndTurn,
	}
	turns := []replay.Turn{empty}
	for i := 0; i < 12; i++ {
		turns = append(turns, toolTurn(fmt.Sprintf("c%d", i), "noop", `{}`))
	}
	h := build(t, turns, func(c *Config) { c.MaxSteps = 4 })
	if err := h.tools.Register(tools.Tool{
		Schema:  llm.ToolSchema{Name: "noop"},
		Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "ok"} },
	}); err != nil {
		t.Fatal(err)
	}
	reopened := false
	if _, err := bus.OnSerial(h.bus, func(_ context.Context, _ TurnStopping) error {
		if reopened {
			return nil
		}
		reopened = true
		return errors.New("keep the turn open for one more step")
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.loop.Run(context.Background(), userMsg("do the work"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !reopened {
		t.Fatal("the checkpoint never reopened the turn, so this test proves nothing")
	}
	if !out.TruncatedBySteps {
		t.Fatalf("the turn did not end on the step ceiling (steps=%d); this test proves nothing", out.Steps)
	}
	if out.FinalEmpty {
		t.Error("FinalEmpty survived from step 1 into a turn that ended at the step ceiling — " +
			"the summary would say the last response carried nothing, about a response that made a tool call")
	}
}
