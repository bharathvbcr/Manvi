package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

var assistConfig = EnhancementConfiguration{OK: true, Provider: "anthropic", Model: "claude-test", ModelSource: "settings", Providers: []string{"anthropic"}}

// assistEnv is a runner over a fake provider that counts what reaches it. It
// has no store: the assist op reads and writes none, which these tests rely on.
type assistEnv struct {
	runner   *EnhancementRunner
	provider *enhancementTestProvider
	resolves atomic.Int32
	streams  atomic.Int32
	mu       sync.Mutex
	requests []llm.Request
}

func assistAnswer(req llm.Request, stop llm.StopReason, text, reasoning string, usage llm.Usage) llm.Stream {
	var content []llm.ContentBlock
	if reasoning != "" {
		content = append(content, llm.ReasoningBlock{Text: reasoning})
	}
	content = append(content, llm.TextBlock{Text: text})
	return &enhancementTestStream{response: llm.Response{StopReason: stop, MaxTokensApplied: req.MaxTokens, Usage: usage,
		Message: llm.Message{Role: llm.RoleAssistant, Content: content}}}
}

func newAssistEnv(t *testing.T, stream func(context.Context, llm.Request) (llm.Stream, error)) *assistEnv {
	t.Helper()
	env := &assistEnv{}
	env.provider = &enhancementTestProvider{name: "anthropic", cap: llm.Capability{Provider: "anthropic", Model: "claude-test", ContextWindow: 200000, MaxOutputTokens: 8000}}
	env.provider.stream = func(ctx context.Context, req llm.Request) (llm.Stream, error) {
		env.streams.Add(1)
		env.mu.Lock()
		env.requests = append(env.requests, req)
		env.mu.Unlock()
		if stream != nil {
			return stream(ctx, req)
		}
		return assistAnswer(req, llm.StopEndTurn, "ok", "", llm.Usage{}), nil
	}
	runner, err := NewEnhancementRunner(&store.Client{}, func(context.Context, string, string) (llm.Provider, error) {
		env.resolves.Add(1)
		return env.provider, nil
	}, func(err error) string { return strings.ReplaceAll(err.Error(), "private-token", "[redacted]") })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	env.runner = runner
	return env
}

func (e *assistEnv) request(i int) llm.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requests[i]
}

func assistParams(system, user string, max int) AssistParams {
	return AssistParams{System: system, User: user, MaxOutputTokens: max}
}

func TestAssistReturnsTextAndUsage(t *testing.T) {
	env := newAssistEnv(t, func(_ context.Context, req llm.Request) (llm.Stream, error) {
		return assistAnswer(req, llm.StopEndTurn, "fix: handle empty diff", "", llm.Usage{InputTokens: 42, OutputTokens: 7}), nil
	})
	got, err := env.runner.Assist(t.Context(), assistParams("write a commit message", "diff --git a b", 512), assistConfig)
	if err != nil {
		t.Fatal(err)
	}
	want := AssistResult{OK: true, Provider: "anthropic", Model: "claude-test", ContextWindow: 200000, MaxOutputTokens: 512,
		Text: "fix: handle empty diff", StopReason: "end_turn", InputTokens: 42, OutputTokens: 7}
	if *got != want {
		t.Fatalf("result = %+v; want %+v", *got, want)
	}
	req := env.request(0)
	if req.Model != "claude-test" || req.System != "write a commit message" || req.MaxTokens != 512 || len(req.Tools) != 0 || len(req.Messages) != 1 {
		t.Fatalf("request = %+v", req)
	}
	if text, _ := req.Messages[0].Content[0].(llm.TextBlock); text.Text != "diff --git a b" || req.Messages[0].Role != llm.RoleUser {
		t.Fatalf("user message = %+v", req.Messages[0])
	}
}

func TestAssistAppliesTheLowerOfRequestedAndModelOutputCap(t *testing.T) {
	env := newAssistEnv(t, nil)
	got, err := env.runner.Assist(t.Context(), assistParams("s", "u", 16384), assistConfig)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxOutputTokens != 8000 || env.request(0).MaxTokens != 8000 {
		t.Fatalf("applied %d, sent %d; the model caps output at 8000", got.MaxOutputTokens, env.request(0).MaxTokens)
	}
}

func TestAssistKeepsReasoningOutOfTheText(t *testing.T) {
	env := newAssistEnv(t, func(_ context.Context, req llm.Request) (llm.Stream, error) {
		return assistAnswer(req, llm.StopEndTurn, "the answer", "private chain of thought", llm.Usage{}), nil
	})
	got, err := env.runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "the answer" || got.Reasoning != "private chain of thought" || strings.Contains(got.Text, "chain of thought") {
		t.Fatalf("text %q, reasoning %q", got.Text, got.Reasoning)
	}
}

func TestAssistReportsTruncationAtTheOutputCap(t *testing.T) {
	env := newAssistEnv(t, func(_ context.Context, req llm.Request) (llm.Stream, error) {
		return assistAnswer(req, llm.StopMaxTokens, "a partial ans", "", llm.Usage{OutputTokens: 100}), nil
	})
	got, err := env.runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig)
	if err != nil {
		t.Fatalf("a truncated answer is a result, not an error: %v", err)
	}
	if !got.Truncated || got.StopReason != "max_tokens" || got.Text != "a partial ans" {
		t.Fatalf("result = %+v", *got)
	}
	complete, err := newAssistEnv(t, nil).runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig)
	if err != nil || complete.Truncated {
		t.Fatalf("an end_turn answer is not truncated: %+v %v", complete, err)
	}
}

func TestAssistRefusesWhatIsNotACompleteOrCappedAnswer(t *testing.T) {
	for _, stop := range []llm.StopReason{llm.StopRefusal, llm.StopToolUse, llm.StopOther} {
		env := newAssistEnv(t, func(_ context.Context, req llm.Request) (llm.Stream, error) {
			return assistAnswer(req, stop, "text", "", llm.Usage{}), nil
		})
		if got, err := env.runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig); err == nil {
			t.Errorf("stop %q returned %+v", stop, got)
		}
	}
}

func TestAssistRefusesAToolRequest(t *testing.T) {
	env := newAssistEnv(t, func(_ context.Context, req llm.Request) (llm.Stream, error) {
		return &enhancementTestStream{next: func() (llm.Chunk, error) { return llm.Chunk{Kind: llm.ChunkToolCallStart, ToolName: "x"}, nil }}, nil
	})
	if _, err := env.runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig); err == nil {
		t.Fatal("a tool call must end a text-only completion")
	}
}

func TestAssistDryRunMakesNoModelCallAndReportsTheWindow(t *testing.T) {
	env := newAssistEnv(t, nil)
	got, err := env.runner.Assist(t.Context(), AssistParams{MaxOutputTokens: 4000, DryRun: true}, assistConfig)
	if err != nil {
		t.Fatal(err)
	}
	want := AssistResult{OK: true, Provider: "anthropic", Model: "claude-test", ContextWindow: 200000, MaxOutputTokens: 4000}
	if *got != want {
		t.Fatalf("dry run = %+v; want %+v", *got, want)
	}
	if env.streams.Load() != 0 {
		t.Fatalf("dry run made %d model calls", env.streams.Load())
	}
	capped, err := env.runner.Assist(t.Context(), AssistParams{MaxOutputTokens: 16384, DryRun: true}, assistConfig)
	if err != nil || capped.MaxOutputTokens != 8000 {
		t.Fatalf("dry run applied cap = %+v %v", capped, err)
	}
}

func TestAssistNamesAModelItHasNoCapabilityFor(t *testing.T) {
	for _, dry := range []bool{false, true} {
		env := newAssistEnv(t, nil)
		selection := assistConfig
		selection.Model = "claude-unknown"
		_, err := env.runner.Assist(t.Context(), AssistParams{System: "s", User: "u", MaxOutputTokens: 100, DryRun: dry}, selection)
		if err == nil || !strings.Contains(err.Error(), "claude-unknown") {
			t.Fatalf("dry=%v: err = %v; want one naming the model", dry, err)
		}
		if env.streams.Load() != 0 {
			t.Fatalf("dry=%v: an unknown model reached the provider", dry)
		}
	}
	// A capability with no declared window is unknown, never a guessed one.
	env := newAssistEnv(t, nil)
	env.provider.cap.ContextWindow = 0
	if _, err := env.runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig); err == nil || env.streams.Load() != 0 {
		t.Fatalf("err = %v, streams = %d", err, env.streams.Load())
	}
}

func TestAssistWithoutASelectionNeverReachesAnyBackend(t *testing.T) {
	for name, selection := range map[string]EnhancementConfiguration{
		"not ok":         {Provider: "anthropic", Model: "claude-test"},
		"empty provider": {OK: true, Model: "claude-test"},
		"empty model":    {OK: true, Provider: "anthropic"},
		"zero":           {},
	} {
		env := newAssistEnv(t, nil)
		for _, dry := range []bool{false, true} {
			if _, err := env.runner.Assist(t.Context(), AssistParams{System: "s", User: "u", MaxOutputTokens: 100, DryRun: dry}, selection); err == nil {
				t.Errorf("%s dry=%v: no error", name, dry)
			}
		}
		if env.resolves.Load() != 0 || env.streams.Load() != 0 {
			t.Errorf("%s: %d provider resolutions and %d model calls without a selection", name, env.resolves.Load(), env.streams.Load())
		}
	}
}

func TestAssistRefusesAProviderThatIsNotTheSelectedOne(t *testing.T) {
	env := newAssistEnv(t, nil)
	env.provider.name = "local"
	if _, err := env.runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig); err == nil || env.streams.Load() != 0 {
		t.Fatalf("a substituted backend was used: %v", err)
	}
}

func TestAssistRefusesAPromptThatCannotFitTheWindow(t *testing.T) {
	env := newAssistEnv(t, nil)
	env.provider.cap.ContextWindow = 1000
	_, err := env.runner.Assist(t.Context(), assistParams("s", strings.Repeat("x", 6000), 100), assistConfig)
	if err == nil || !strings.Contains(err.Error(), "context window") || env.streams.Load() != 0 {
		t.Fatalf("err = %v, streams = %d", err, env.streams.Load())
	}
}

func TestAssistErrorsAreRedacted(t *testing.T) {
	env := newAssistEnv(t, func(context.Context, llm.Request) (llm.Stream, error) {
		return nil, errors.New("401 for key private-token")
	})
	_, err := env.runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig)
	if err == nil || strings.Contains(err.Error(), "private-token") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("err = %v", err)
	}
	// And at the wire, where a host reads it.
	module := assistModule(env)
	response := roundTrip(t, Options{Modules: []Module{module}},
		Request{ID: "a", Op: OpWorkAssistComplete, Params: json.RawMessage(`{"system":"s","user":"u","max_output_tokens":100}`)})[0]
	if response.OK || response.Error == nil || strings.Contains(response.Error.Message, "private-token") || !strings.Contains(response.Error.Message, "[redacted]") {
		t.Fatalf("response = %+v", response)
	}
}

func TestAssistCancellationReachesTheProviderStream(t *testing.T) {
	streams := make(chan *enhancementTestStream, 1)
	env := newAssistEnv(t, func(ctx context.Context, _ llm.Request) (llm.Stream, error) {
		stream := &enhancementTestStream{next: func() (llm.Chunk, error) {
			<-ctx.Done()
			return llm.Chunk{}, ctx.Err()
		}}
		streams <- stream
		return stream, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := env.runner.Assist(ctx, assistParams("s", "u", 100), assistConfig)
		done <- err
	}()
	stream := <-streams
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v; want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop the stream")
	}
	if !stream.closed {
		t.Fatal("the cancelled stream was not closed")
	}
}

// blockedAssists starts n Assist calls that wait inside the provider and
// returns the channel their results arrive on.
func blockedAssists(t *testing.T, env *assistEnv, n int) chan error {
	t.Helper()
	results := make(chan error, 8)
	for range n {
		go func() {
			_, err := env.runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig)
			results <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for int(env.streams.Load()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d assist calls reached the provider", env.streams.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return results
}

func newBlockingAssistEnv(t *testing.T) (*assistEnv, chan struct{}) {
	t.Helper()
	gate := make(chan struct{})
	env := newAssistEnv(t, func(ctx context.Context, req llm.Request) (llm.Stream, error) {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return assistAnswer(req, llm.StopEndTurn, "ok", "", llm.Usage{}), nil
	})
	return env, gate
}

func TestAssistAllowsTwoInFlightAndRefusesAThirdAsBusy(t *testing.T) {
	env, gate := newBlockingAssistEnv(t)
	results := blockedAssists(t, env, 2)
	_, err := env.runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig)
	var refused *Error
	if !errors.As(err, &refused) || refused.Code != ErrBusy || !refused.Retryable {
		t.Fatalf("third call err = %v; want a retryable %s", err, ErrBusy)
	}
	if env.streams.Load() != 2 {
		t.Fatalf("%d model calls; the refused call must not reach the provider", env.streams.Load())
	}
	// A dry run is no model call and is not counted against the bound.
	if _, err := env.runner.Assist(t.Context(), AssistParams{MaxOutputTokens: 100, DryRun: true}, assistConfig); err != nil {
		t.Fatalf("dry run while two are in flight: %v", err)
	}
	close(gate)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig); err != nil {
		t.Fatalf("a slot was not released: %v", err)
	}
}

func TestAssistReleasesItsSlotWhenTheCallFails(t *testing.T) {
	env := newAssistEnv(t, func(context.Context, llm.Request) (llm.Stream, error) { return nil, errors.New("boom") })
	for range 5 {
		if _, err := env.runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig); err == nil || strings.Contains(err.Error(), ErrBusy) {
			t.Fatalf("err = %v; a failed call leaked its slot", err)
		}
	}
}

func TestAssistDoesNotShareTheEnhancementSlot(t *testing.T) {
	env := newAssistEnv(t, nil)
	env.runner.mu.Lock()
	env.runner.active = &enhancementJob{id: "running", done: make(chan struct{}), cancel: func() {}}
	env.runner.mu.Unlock()
	got, err := env.runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig)
	if err != nil || got.Text != "ok" {
		t.Fatalf("assist while an enhancement is in flight: %+v %v", got, err)
	}
	// And the other way: assist calls in flight do not occupy the enhancement slot.
	env2, gate := newBlockingAssistEnv(t)
	blockedAssists(t, env2, 2)
	env2.runner.mu.Lock()
	occupied := env2.runner.active != nil
	env2.runner.mu.Unlock()
	if occupied {
		t.Fatal("in-flight assist calls hold the enhancement slot")
	}
	close(gate)
}

func TestAssistIsCancelledByCloseAndRefusedAfterIt(t *testing.T) {
	env, _ := newBlockingAssistEnv(t)
	results := blockedAssists(t, env, 1)
	if err := env.runner.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-results:
		if err == nil {
			t.Fatal("a call in flight when the runner closed succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the in-flight assist call")
	}
	if _, err := env.runner.Assist(t.Context(), assistParams("s", "u", 100), assistConfig); err == nil {
		t.Fatal("a closed runner accepted an assist call")
	}
	if _, err := env.runner.Assist(t.Context(), AssistParams{MaxOutputTokens: 100, DryRun: true}, assistConfig); err == nil {
		t.Fatal("a closed runner accepted a dry run")
	}
}

func TestAssistRedactsNothingAndSaysSo(t *testing.T) {
	// The enhancement path has no prompt-redaction owner (see docs/WORKBENCH.md,
	// "Assist completion"), so the count is a statement, not a measurement: it
	// is zero, and the prompt reaches the provider exactly as the host sent it.
	env := newAssistEnv(t, nil)
	secret := "token sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" // #nosec G101 -- a made-up key in the real one's shape; the test needs the prompt to carry something key-shaped
	got, err := env.runner.Assist(t.Context(), assistParams(secret, secret, 100), assistConfig)
	if err != nil || got.Redactions != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	if env.request(0).System != secret {
		t.Fatal("the system prompt was altered without being counted")
	}
}

// --- decoding ---

func TestAssistParamsAreDecodedStrictly(t *testing.T) {
	big := func(n int) string { return strings.Repeat("a", n) }
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"valid", `{"system":"s","user":"u","max_output_tokens":1}`, true},
		{"top of range", `{"system":"s","user":"u","max_output_tokens":16384}`, true},
		{"largest system", `{"system":"` + big(32768) + `","user":"u","max_output_tokens":10}`, true},
		{"largest user", `{"system":"s","user":"` + big(2097152) + `","max_output_tokens":10}`, true},
		{"dry run needs neither prompt", `{"max_output_tokens":10,"dry_run":true}`, true},
		{"dry run with empty prompts", `{"system":"","user":"","max_output_tokens":10,"dry_run":true}`, true},
		{"dry_run false is an ordinary call", `{"system":"s","user":"u","max_output_tokens":10,"dry_run":false}`, true},
		{"unknown field", `{"system":"s","user":"u","max_output_tokens":10,"temperature":0}`, false},
		{"case-variant key", `{"System":"s","user":"u","max_output_tokens":10}`, false},
		{"duplicate key", `{"system":"s","system":"t","user":"u","max_output_tokens":10}`, false},
		{"system over cap", `{"system":"` + big(32769) + `","user":"u","max_output_tokens":10}`, false},
		{"user over cap", `{"system":"s","user":"` + big(2097153) + `","max_output_tokens":10}`, false},
		{"zero tokens", `{"system":"s","user":"u","max_output_tokens":0}`, false},
		{"tokens over cap", `{"system":"s","user":"u","max_output_tokens":16385}`, false},
		{"negative tokens", `{"system":"s","user":"u","max_output_tokens":-1}`, false},
		{"missing tokens", `{"system":"s","user":"u"}`, false},
		{"fractional tokens", `{"system":"s","user":"u","max_output_tokens":1.5}`, false},
		{"dry run still bounds tokens", `{"max_output_tokens":0,"dry_run":true}`, false},
		{"empty user without dry run", `{"system":"s","user":"","max_output_tokens":10}`, false},
		{"missing user without dry run", `{"system":"s","max_output_tokens":10}`, false},
		{"empty system without dry run", `{"system":"","user":"u","max_output_tokens":10}`, false},
		{"invalid UTF-8", "{\"system\":\"s\xff\",\"user\":\"u\",\"max_output_tokens\":10}", false},
		{"lone surrogate escape", `{"system":"s\ud800","user":"u","max_output_tokens":10}`, false},
		{"trailing content", `{"system":"s","user":"u","max_output_tokens":10} {}`, false},
		{"not an object", `["s","u",10]`, false},
		{"null params", `null`, false},
		{"empty params", ``, false},
		{"wrong type", `{"system":1,"user":"u","max_output_tokens":10}`, false},
		{"dry_run wrong type", `{"system":"s","user":"u","max_output_tokens":10,"dry_run":"yes"}`, false},
	}
	for _, c := range cases {
		params, refused := decodeAssistParams(json.RawMessage(c.raw))
		switch {
		case c.ok && refused != nil:
			t.Errorf("%s: refused: %v", c.name, refused)
		case !c.ok && (refused == nil || refused.Code != ErrBadRequest):
			t.Errorf("%s: decoded %+v; want %s", c.name, params, ErrBadRequest)
		}
	}
}

func TestAssistWireRefusesBadParamsBeforeAnyProviderWork(t *testing.T) {
	env := newAssistEnv(t, nil)
	responses := roundTrip(t, Options{Modules: []Module{assistModule(env)}},
		Request{ID: "unknown", Op: OpWorkAssistComplete, Params: json.RawMessage(`{"system":"s","user":"u","max_output_tokens":9,"extra":1}`)},
		Request{ID: "empty", Op: OpWorkAssistComplete, Params: json.RawMessage(`{"system":"s","user":"","max_output_tokens":9}`)},
		Request{ID: "none", Op: OpWorkAssistComplete})
	for _, r := range byID(t, responses, "unknown", "empty", "none") {
		if r.OK || r.Error == nil || r.Error.Code != ErrBadRequest {
			t.Errorf("%s = %+v", r.ID, r)
		}
	}
	if env.resolves.Load() != 0 || env.streams.Load() != 0 {
		t.Fatalf("bad params reached the provider: %d resolutions, %d calls", env.resolves.Load(), env.streams.Load())
	}
}

// --- the wire ---

func assistModule(env *assistEnv) WorkbenchModule {
	client := &store.Client{}
	env.runner.client = client
	return WorkbenchModule{Client: client, Runner: env.runner, Configuration: &assistConfig}
}

func TestAssistResultOnTheWireCarriesTheDeclaredFields(t *testing.T) {
	env := newAssistEnv(t, func(_ context.Context, req llm.Request) (llm.Stream, error) {
		return assistAnswer(req, llm.StopEndTurn, "hi", "thought", llm.Usage{InputTokens: 3, OutputTokens: 2}), nil
	})
	responses := roundTrip(t, Options{Modules: []Module{assistModule(env)}},
		Request{ID: "a", Op: OpWorkAssistComplete, Params: json.RawMessage(`{"system":"s","user":"u","max_output_tokens":50}`)},
		Request{ID: "dry", Op: OpWorkAssistComplete, Params: json.RawMessage(`{"max_output_tokens":50,"dry_run":true}`)})
	for _, r := range byID(t, responses, "a", "dry") {
		if !r.OK {
			t.Fatalf("%s: %+v", r.ID, r.Error)
		}
		var fields map[string]any
		if err := json.Unmarshal(r.Result, &fields); err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		want := []string{"context_window", "input_tokens", "max_output_tokens", "model", "ok", "output_tokens", "provider", "reasoning", "redactions", "stop_reason", "text", "truncated"}
		if !slices.Equal(keys, want) {
			t.Fatalf("%s result keys = %v; want %v", r.ID, keys, want)
		}
	}
}

func TestAssistIsAHostOperationOnItsOwnLane(t *testing.T) {
	if got := laneFor(OpWorkAssistComplete); got != "assist" || got == laneFor(OpWorkRefine) || got == laneFor("work.items.list") {
		t.Fatalf("lane = %q", got)
	}
	if got := opDeadline(OpWorkAssistComplete); got != enhancementJobTimeout+30*time.Second {
		t.Fatalf("deadline = %v", got)
	}
	env := newAssistEnv(t, nil)
	resp := roundTrip(t, Options{Modules: []Module{assistModule(env)}}, Request{ID: "h", Op: OpHello})[0]
	var hello HelloResult
	if err := json.Unmarshal(resp.Result, &hello); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(hello.Ops, OpWorkAssistComplete) {
		t.Fatalf("hello does not negotiate %s", OpWorkAssistComplete)
	}
	if got := hello.OpLimits[OpWorkAssistComplete]; got.Lane != "assist" || got.DeadlineMS != (enhancementJobTimeout+30*time.Second).Milliseconds() {
		t.Fatalf("limits = %+v", got)
	}
	// Like refine, it is served only where an enhancement selection is configured.
	bare := roundTrip(t, Options{Modules: []Module{WorkbenchModule{Client: env.runner.client, Runner: env.runner}}}, Request{ID: "h", Op: OpHello})[0]
	if bytes.Contains(bare.Result, []byte(OpWorkAssistComplete)) {
		t.Fatalf("assist advertised without a configuration: %s", bare.Result)
	}
}

func TestAssistAnswersBusyOnTheWireWhileTwoAreInFlightAndNeverQueuesBehindThem(t *testing.T) {
	env, gate := newBlockingAssistEnv(t)
	s := startSession(t, t.Context(), Options{Modules: []Module{assistModule(env)}})
	params := `{"system":"s","user":"u","max_output_tokens":9}`
	s.send("one", OpWorkAssistComplete, params)
	s.send("two", OpWorkAssistComplete, params)
	deadline := time.Now().Add(5 * time.Second)
	for env.streams.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d calls reached the provider: the lane serialises assist calls", env.streams.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.send("three", OpWorkAssistComplete, params)
	third := s.await("three", time.Second)
	if third.OK || third.Error == nil || third.Error.Code != ErrBusy || !third.Error.Retryable {
		t.Fatalf("third = %+v", third)
	}
	// Other lanes are untouched while two assist calls are held.
	s.send("hello", OpHello, "")
	if r := s.await("hello", time.Second); !r.OK {
		t.Fatalf("hello: %+v", r)
	}
	close(gate)
	for _, id := range []string{"one", "two"} {
		if r := s.await(id, 5*time.Second); !r.OK {
			t.Fatalf("%s: %+v", id, r.Error)
		}
	}
}

func TestAssistWireIsIndependentOfAnEnhancementInFlight(t *testing.T) {
	env := newAssistEnv(t, nil)
	env.runner.mu.Lock()
	env.runner.active = &enhancementJob{id: "running", done: make(chan struct{}), cancel: func() {}}
	env.runner.mu.Unlock()
	r := roundTrip(t, Options{Modules: []Module{assistModule(env)}},
		Request{ID: "a", Op: OpWorkAssistComplete, Params: json.RawMessage(`{"system":"s","user":"u","max_output_tokens":9}`)})[0]
	if !r.OK {
		t.Fatalf("assist answered %+v while an enhancement was running", r.Error)
	}
}

// --- the contract ---

func TestServeContractDeclaresAssistComplete(t *testing.T) {
	raw, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	var committed serveContract
	if err := json.Unmarshal(raw, &committed); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(committed.Ops.Optional, OpWorkAssistComplete) || slices.Contains(committed.Ops.Always, OpWorkAssistComplete) {
		t.Fatalf("%s must be under ops.optional only", OpWorkAssistComplete)
	}
	params, result := committed.Types["AssistCompleteParams"], committed.Types["AssistCompleteResult"]
	if params == nil || result == nil {
		t.Fatalf("types: params %v, result %v", params, result)
	}
	if f := params.Fields["max_output_tokens"]; f == nil || !f.Required {
		t.Errorf("max_output_tokens = %+v; it is required, a dry run included", f)
	}
	for _, name := range []string{"system", "user", "dry_run"} {
		if f := params.Fields[name]; f == nil || f.Required {
			t.Errorf("%s = %+v; it may be absent on a dry run", name, f)
		}
	}
	for _, name := range []string{"ok", "provider", "model", "context_window", "max_output_tokens", "text", "reasoning", "stop_reason", "truncated", "input_tokens", "output_tokens", "redactions"} {
		if f := result.Fields[name]; f == nil || !f.Required {
			t.Errorf("result %s = %+v", name, f)
		}
	}
}
