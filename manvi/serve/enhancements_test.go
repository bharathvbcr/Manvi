package serve

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/internal/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func enhancementFixture(t *testing.T) *store.Client {
	t.Helper()
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(client.Close)
	enhancementCall(t, client, "repositories.put", `{"request_id":"r","id":"r","expected_revision":0,"name":"repo","identity_key":"r"}`)
	enhancementCall(t, client, "items.put", `{"request_id":"t","id":"t","expected_revision":0,"title":"fix E42","description":"Keep the original task evidence.","repository_ids":["r"],"primary_repository_id":"r"}`)
	enhancementCall(t, client, "enhancements.create", `{"request_id":"e","id":"e","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"test-model"}`)
	return client
}

func enhancementCall(t *testing.T, client *store.Client, method, input string) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := client.Workbench(ctx, method, json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return result
}

func enhancementWait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not acknowledge completion")
	}
}

func enhancementTestRunner(t *testing.T, client *store.Client, provider *enhancementTestProvider) *EnhancementRunner {
	t.Helper()
	runner, err := NewEnhancementRunner(client, func(context.Context, string, string) (llm.Provider, error) { return provider, nil }, func(err error) string { return strings.ReplaceAll(err.Error(), "private-token", "[redacted]") })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runner.Close(); err != nil {
			t.Error(err)
		}
	})
	return runner
}

func TestEnhancementGenerationIsAsyncDurableAndDoesNotRepeatModelCalls(t *testing.T) {
	client := enhancementFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	provider := &enhancementTestProvider{name: "local", cap: llm.Capability{Model: "test-model", ContextWindow: 100000, MaxOutputTokens: 8192}}
	provider.stream = func(ctx context.Context, req llm.Request) (llm.Stream, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &enhancementTestStream{response: llm.Response{StopReason: llm.StopEndTurn, MaxTokensApplied: req.MaxTokens, Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: `{"title":"Resolve E42"}`}}}}}, nil
	}
	runner := enhancementTestRunner(t, client, provider)
	responses := roundTrip(t, Options{Modules: []Module{WorkbenchModule{Client: client, Runner: runner}}},
		Request{ID: "start", Op: "work.enhancements.generate", Params: json.RawMessage(`{"id":"e","request_id":"start","expected_revision":1}`)},
		Request{ID: "board", Op: "work.items.list"}, Request{ID: "hello", Op: OpHello})
	for _, response := range responses {
		if !response.OK {
			t.Fatalf("control operation failed: %+v", response)
		}
	}
	if len(responses) != 3 || !strings.Contains(string(responses[0].Result), `"state":"running"`) || !strings.Contains(string(responses[2].Result), `"work.enhancements.generate"`) {
		t.Fatalf("missing host generation negotiation/claim: %+v", responses)
	}
	enhancementWait(t, started)
	runner.mu.Lock()
	job := runner.active
	runner.mu.Unlock()
	if job == nil {
		t.Fatal("worker ended while provider was blocked")
	}
	// A second host shares the database, but cannot start a second model call.
	otherClient := store.New(client.Binary, client.DB)
	t.Cleanup(otherClient.Close)
	otherRunner := enhancementTestRunner(t, otherClient, provider)
	for _, host := range []*EnhancementRunner{runner, otherRunner} {
		if _, err := host.Generate(t.Context(), json.RawMessage(`{"id":"e","request_id":"start","expected_revision":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate generation started")
	}
	close(release)
	enhancementWait(t, job.done)
	ready := enhancementCall(t, client, "enhancements.get", `{"id":"e"}`)
	if !strings.Contains(string(ready), `"state":"ready"`) || !strings.Contains(string(ready), `Resolve E42`) {
		t.Fatalf("result was not persisted: %s", ready)
	}
	if _, err := runner.Generate(t.Context(), json.RawMessage(`{"id":"e","request_id":"start","expected_revision":1}`)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("ready proposal triggered a new call")
	}
	task := enhancementCall(t, client, "items.get", `{"id":"t"}`)
	if !strings.Contains(string(task), `"title":"fix E42"`) || !strings.Contains(string(task), `"revision":1`) {
		t.Fatalf("generation modified the task without acceptance: %s", task)
	}
	client.Close()
	reopened := store.New(client.Binary, client.DB)
	t.Cleanup(reopened.Close)
	if got := enhancementCall(t, reopened, "enhancements.get", `{"id":"e"}`); string(got) != string(ready) {
		t.Fatal("restart lost the generated proposal")
	}
}

func TestEnhancementDismissAndShutdownWaitForActualProviderReturn(t *testing.T) {
	for _, mode := range []string{"dismiss", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			client := enhancementFixture(t)
			started := make(chan struct{})
			provider := &enhancementTestProvider{name: "local", cap: llm.Capability{Model: "test-model", ContextWindow: 100000, MaxOutputTokens: 8192}, stream: func(ctx context.Context, _ llm.Request) (llm.Stream, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			runner := enhancementTestRunner(t, client, provider)
			if _, err := runner.Generate(t.Context(), json.RawMessage(`{"id":"e","request_id":"start","expected_revision":1}`)); err != nil {
				t.Fatal(err)
			}
			enhancementWait(t, started)
			runner.mu.Lock()
			job := runner.active
			runner.mu.Unlock()
			want := "failed"
			if mode == "dismiss" {
				want = "cancelled"
				pending := enhancementCall(t, client, "enhancements.dismiss", `{"id":"e","request_id":"cancel","expected_revision":2}`)
				if !strings.Contains(string(pending), `"state":"cancel_requested"`) {
					t.Fatalf("cancellation was prematurely final: %s", pending)
				}
			} else if err := runner.Close(); err != nil {
				t.Fatal(err)
			}
			enhancementWait(t, job.done)
			got := enhancementCall(t, client, "enhancements.get", `{"id":"e"}`)
			if !strings.Contains(string(got), `"state":"`+want+`"`) {
				t.Fatalf("wrong acknowledged outcome: %s", got)
			}
			if mode == "shutdown" && !strings.Contains(string(got), "generation cancelled") {
				t.Fatalf("shutdown cancel was not labeled cancelled: %s", got)
			}
		})
	}
}

func TestEnhancementGenerationRejectsMalformedCancelledAndStaleStarts(t *testing.T) {
	client := enhancementFixture(t)
	var calls atomic.Int32
	provider := &enhancementTestProvider{name: "local", cap: llm.Capability{Model: "test-model", ContextWindow: 100000}, stream: func(context.Context, llm.Request) (llm.Stream, error) {
		calls.Add(1)
		return nil, errors.New("must not run")
	}}
	runner := enhancementTestRunner(t, client, provider)
	for _, input := range []string{`{}`, `{"id":"e","id":"e","request_id":"s","expected_revision":1}`, `{"id":"e","request_id":"s","expected_revision":1,"skip_permissions":true}`, `{"id":"e","request_id":"s","expected_revision":2}`} {
		if _, err := runner.Generate(t.Context(), json.RawMessage(input)); err == nil {
			t.Fatalf("invalid start accepted: %s", input)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := runner.Generate(ctx, json.RawMessage(`{"id":"e","request_id":"s","expected_revision":1}`)); err == nil {
		t.Fatal("cancelled generation accepted")
	}
	enhancementCall(t, client, "items.put", `{"request_id":"edit","id":"t","expected_revision":1,"title":"new task E42","repository_ids":["r"],"primary_repository_id":"r"}`)
	if _, err := runner.Generate(t.Context(), json.RawMessage(`{"id":"e","request_id":"s","expected_revision":1}`)); err == nil {
		t.Fatal("stale source generation accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid start reached the provider")
	}
}

func TestEnhancementProviderFailureIsRedactedAndKeepsTheTaskIntact(t *testing.T) {
	client := enhancementFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	provider := &enhancementTestProvider{name: "local", cap: llm.Capability{Model: "test-model", ContextWindow: 100000}, stream: func(ctx context.Context, _ llm.Request) (llm.Stream, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, errors.New("upstream private-token unavailable")
	}}
	runner := enhancementTestRunner(t, client, provider)
	if _, err := runner.Generate(t.Context(), json.RawMessage(`{"id":"e","request_id":"start","expected_revision":1}`)); err != nil {
		t.Fatal(err)
	}
	enhancementWait(t, started)
	runner.mu.Lock()
	job := runner.active
	runner.mu.Unlock()
	close(release)
	enhancementWait(t, job.done)
	got := enhancementCall(t, client, "enhancements.get", `{"id":"e"}`)
	if !strings.Contains(string(got), `"state":"failed"`) || !strings.Contains(string(got), "[redacted]") || strings.Contains(string(got), "private-token") {
		t.Fatalf("invalid durable error: %s", got)
	}
	task := enhancementCall(t, client, "items.get", `{"id":"t"}`)
	if !strings.Contains(string(task), `"title":"fix E42"`) || !strings.Contains(string(task), `"revision":1`) {
		t.Fatalf("failed inference modified task: %s", task)
	}
}

func TestEnhancementSettleFailureDistinguishesTimeoutFromCancel(t *testing.T) {
	if enhancementJobTimeout != 150*time.Second {
		t.Fatalf("job timeout drifted: %s", enhancementJobTimeout)
	}
	cases := []struct {
		state string
		err   error
		want  string
	}{
		{"cancel_requested", context.DeadlineExceeded, "generation cancelled by the user"},
		{"running", context.DeadlineExceeded, "generation timed out"},
		{"running", context.Canceled, "generation cancelled"},
		{"running", errors.New("upstream boom"), "upstream boom"},
		{"running", nil, ""},
	}
	for _, tc := range cases {
		got := enhancementSettleFailure(tc.state, tc.err)
		if tc.want == "" {
			if got != nil {
				t.Fatalf("state=%s err=%v: got %v, want nil", tc.state, tc.err, got)
			}
			continue
		}
		if got == nil || got.Error() != tc.want {
			t.Fatalf("state=%s err=%v: got %v, want %q", tc.state, tc.err, got, tc.want)
		}
	}
}
