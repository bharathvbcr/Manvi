package serve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func automaticFixture(t *testing.T) *store.Client {
	t.Helper()
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(client.Close)
	enhancementCall(t, client, "repositories.put", `{"request_id":"r","id":"r","expected_revision":0,"name":"repo","identity_key":"r"}`)
	enhancementCall(t, client, "items.put", `{"request_id":"t","id":"t","expected_revision":0,"title":"Investigate E42","description":"Keep the original task evidence.","repository_ids":["r"],"primary_repository_id":"r"}`)
	enhancementCall(t, client, "items.put", `{"id":"t","request_id":"locks","expected_revision":1,"title":"fix E42","description":"Keep the original task evidence.","locked_fields":["description"],"repository_ids":["r"],"primary_repository_id":"r"}`)
	return client
}

func automaticRequest(t *testing.T, client *store.Client, runner *EnhancementRunner, op, input, model string) Response {
	t.Helper()
	responses := roundTrip(t, Options{Modules: []Module{WorkbenchModule{Client: client, Runner: runner, Configuration: &EnhancementConfiguration{OK: true, Provider: "local", Model: model, Providers: []string{"local"}}}}}, Request{ID: "automatic", Op: "work.enhancements." + op, Params: json.RawMessage(input)})
	if len(responses) != 1 {
		t.Fatalf("missing automatic response: %+v", responses)
	}
	return responses[0]
}

func automaticState(t *testing.T, client *store.Client, runner *EnhancementRunner, want string) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for {
		response := automaticRequest(t, client, runner, "worker", "{}", "test-model")
		var result struct {
			State string `json:"state"`
		}
		if !response.OK || json.Unmarshal(response.Result, &result) != nil {
			t.Fatalf("worker state unavailable: %+v", response)
		}
		if result.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker state %q; wanted %q (%s)", result.State, want, response.Result)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func automaticProvider(calls *atomic.Int32) *enhancementTestProvider {
	provider := &enhancementTestProvider{name: "local", cap: llm.Capability{Model: "test-model", ContextWindow: 100000, MaxOutputTokens: 8192}}
	provider.stream = func(_ context.Context, request llm.Request) (llm.Stream, error) {
		calls.Add(1)
		return &enhancementTestStream{response: llm.Response{StopReason: llm.StopEndTurn, MaxTokensApplied: request.MaxTokens, Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: `{"title":"Resolve E42"}`}}}}}, nil
	}
	return provider
}

func waitAutomaticDue(t *testing.T, client *store.Client) {
	t.Helper()
	var queue struct {
		Items []struct {
			Due int64 `json:"not_before_ms"`
		} `json:"items"`
	}
	if err := json.Unmarshal(enhancementCall(t, client, "automation.list", `{"task_id":"t"}`), &queue); err != nil || len(queue.Items) != 1 {
		t.Fatalf("queue missing: %+v %v", queue, err)
	}
	if wait := time.Until(time.UnixMilli(queue.Items[0].Due)); wait > 0 {
		if wait > 2*time.Second {
			t.Fatal("unexpected debounce")
		}
		time.Sleep(wait)
	}
}

func TestAutomaticWakeProcessesSavedTextOnceAndReturnsToIdle(t *testing.T) {
	client := automaticFixture(t)
	var calls atomic.Int32
	runner := enhancementTestRunner(t, client, automaticProvider(&calls))
	response := automaticRequest(t, client, runner, "worker", "{}", "test-model")
	if !response.OK || !strings.Contains(string(response.Result), `"state":"not_started"`) {
		t.Fatalf("read started or hid the worker: %+v", response)
	}
	if calls.Load() != 0 {
		t.Fatal("reading status started a model")
	}
	for i := 0; i < 3; i++ {
		if response := automaticRequest(t, client, runner, "wake", "{}", "test-model"); !response.OK {
			t.Fatalf("wake failed: %+v", response)
		}
	}
	automaticState(t, client, runner, "idle")
	if calls.Load() != 1 {
		t.Fatalf("model calls=%d", calls.Load())
	}
	ready := enhancementCall(t, client, "enhancements.list", `{"automatic":true,"states":["ready"],"limit":1}`)
	if !strings.Contains(string(ready), `"total":1`) {
		t.Fatalf("automatic proposal not ready: %s", ready)
	}
	task := enhancementCall(t, client, "items.get", `{"id":"t"}`)
	if !strings.Contains(string(task), `"title":"fix E42"`) || !strings.Contains(string(task), `"revision":2`) {
		t.Fatal("automatic generation accepted its own changes")
	}
	time.Sleep(40 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("idle worker repeated inference")
	}
}

func TestHostTextSaveWakesAutomaticallyButMovesDoNotRepeatInference(t *testing.T) {
	client := automaticFixture(t)
	var calls atomic.Int32
	runner := enhancementTestRunner(t, client, automaticProvider(&calls))
	options := Options{Modules: []Module{WorkbenchModule{Client: client, Runner: runner, Configuration: &EnhancementConfiguration{OK: true, Provider: "local", Model: "test-model"}}}}
	input := `{"id":"t","request_id":"new-text","expected_revision":2,"title":"Investigate E42","description":"Keep the original task evidence.","locked_fields":["description"],"repository_ids":["r"],"primary_repository_id":"r"}`
	response := roundTrip(t, options, Request{ID: "save", Op: "work.items.put", Params: json.RawMessage(input)})
	if len(response) != 1 || !response[0].OK {
		t.Fatalf("save failed: %+v", response)
	}
	automaticState(t, client, runner, "idle")
	if calls.Load() != 1 {
		t.Fatalf("saved text did not produce one suggestion: %d", calls.Load())
	}
	move := strings.ReplaceAll(strings.ReplaceAll(input, `"new-text"`, `"move"`), `"expected_revision":2`, `"expected_revision":3,"status":"review"`)
	response = roundTrip(t, options, Request{ID: "move", Op: "work.items.put", Params: json.RawMessage(move)})
	if len(response) != 1 || !response[0].OK || !strings.Contains(string(response[0].Result), `"automatic_enhancement_queued":false`) {
		t.Fatalf("move failed or queued generation: %+v", response)
	}
	if calls.Load() != 1 {
		t.Fatal("a move repeated inference")
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	last := strings.ReplaceAll(strings.ReplaceAll(input, `"new-text"`, `"after-close"`), `"expected_revision":2`, `"expected_revision":4`)
	last = strings.ReplaceAll(last, "Investigate E42", "Investigate E42 after restart")
	response = roundTrip(t, options, Request{ID: "closed", Op: "work.items.put", Params: json.RawMessage(last)})
	if len(response) != 1 || !response[0].OK {
		t.Fatalf("worker shutdown turned a committed save into a failure: %+v", response)
	}
	if queued := enhancementCall(t, client, "automation.list", `{"task_id":"t"}`); !strings.Contains(string(queued), `"total":1`) {
		t.Fatal("shutdown lost durable saved work")
	}
}

func TestAutomaticRestartResumesPreparedWorkButDoesNotRepeatAClaimedAttempt(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "claimed"}[claimed], func(t *testing.T) {
			client := automaticFixture(t)
			waitAutomaticDue(t, client)
			enhancementCall(t, client, "automation.prepare", `{"id":"prepared","request_id":"prepare","expected_revision":0,"task_id":"t","expected_task_revision":2,"expected_settings_revision":1,"provider":"local","model":"test-model"}`)
			if claimed {
				enhancementCall(t, client, "enhancements.claim", `{"id":"prepared","request_id":"claim","expected_revision":1,"worker_id":"previous-host"}`)
			}
			client.Close()
			reopened := store.New(client.Binary, client.DB)
			t.Cleanup(reopened.Close)
			var calls atomic.Int32
			runner := enhancementTestRunner(t, reopened, automaticProvider(&calls))
			if response := automaticRequest(t, reopened, runner, "wake", "{}", "test-model"); !response.OK {
				t.Fatalf("wake failed: %+v", response)
			}
			if claimed {
				automaticState(t, reopened, runner, "waiting")
				if calls.Load() != 0 {
					t.Fatal("claimed provider outcome was replayed")
				}
				record := enhancementCall(t, reopened, "enhancements.get", `{"id":"prepared"}`)
				if !strings.Contains(string(record), `"worker_id":"previous-host"`) {
					t.Fatal("old ownership replaced")
				}
			} else {
				automaticState(t, reopened, runner, "idle")
				if calls.Load() != 1 {
					t.Fatal("prepared work did not resume exactly once")
				}
			}
		})
	}
}

func TestAutomaticMissingSelectionPausesWithoutConsumingQueuedWork(t *testing.T) {
	client := automaticFixture(t)
	var calls atomic.Int32
	runner := enhancementTestRunner(t, client, automaticProvider(&calls))
	if response := automaticRequest(t, client, runner, "wake", "{}", ""); !response.OK {
		t.Fatalf("wake failed: %+v", response)
	}
	automaticState(t, client, runner, "paused")
	if calls.Load() != 0 {
		t.Fatal("missing model caused fallback inference")
	}
	if got := enhancementCall(t, client, "automation.list", "{}"); !strings.Contains(string(got), `"total":1`) {
		t.Fatal("missing configuration consumed queued work")
	}
}

func TestAutomaticProviderFailureIsDurableAndNeverAutomaticallyRetried(t *testing.T) {
	client := automaticFixture(t)
	var calls atomic.Int32
	provider := automaticProvider(&calls)
	provider.stream = func(context.Context, llm.Request) (llm.Stream, error) {
		calls.Add(1)
		return nil, errors.New("fixture provider unavailable")
	}
	runner := enhancementTestRunner(t, client, provider)
	if response := automaticRequest(t, client, runner, "wake", "{}", "test-model"); !response.OK {
		t.Fatalf("wake failed: %+v", response)
	}
	automaticState(t, client, runner, "idle")
	for i := 0; i < 2; i++ {
		automaticRequest(t, client, runner, "wake", "{}", "test-model")
		automaticState(t, client, runner, "idle")
	}
	if calls.Load() != 1 {
		t.Fatalf("failure was retried: %d", calls.Load())
	}
	if failed := enhancementCall(t, client, "enhancements.list", `{"automatic":true,"states":["failed"]}`); !strings.Contains(string(failed), `"total":1`) {
		t.Fatalf("failure disappeared: %s", failed)
	}
}

func TestAutomaticResumesAMaximumLengthProposalIdentity(t *testing.T) {
	client := automaticFixture(t)
	waitAutomaticDue(t, client)
	id := strings.Repeat("x", 128)
	enhancementCall(t, client, "automation.prepare", `{"id":"`+id+`","request_id":"prepare","expected_revision":0,"task_id":"t","expected_task_revision":2,"expected_settings_revision":1,"provider":"local","model":"test-model"}`)
	var calls atomic.Int32
	runner := enhancementTestRunner(t, client, automaticProvider(&calls))
	automaticRequest(t, client, runner, "wake", "{}", "test-model")
	automaticState(t, client, runner, "idle")
	if calls.Load() != 1 {
		t.Fatal("valid long proposal identity did not execute")
	}
}

func TestAutomaticCloseDuringDebouncePreservesQueueAndRefusesReopening(t *testing.T) {
	client := automaticFixture(t)
	var calls atomic.Int32
	runner := enhancementTestRunner(t, client, automaticProvider(&calls))
	automaticRequest(t, client, runner, "wake", "{}", "test-model")
	automaticState(t, client, runner, "waiting")
	started := time.Now()
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 2*time.Second || calls.Load() != 0 {
		t.Fatal("closing a waiting worker started inference or waited for its timer")
	}
	if queue := enhancementCall(t, client, "automation.list", "{}"); !strings.Contains(string(queue), `"total":1`) {
		t.Fatal("shutdown lost queued text")
	}
	response := automaticRequest(t, client, runner, "wake", "{}", "test-model")
	if response.OK || response.Error == nil || response.Error.Code != "closed" {
		t.Fatalf("closed worker wake was not refused precisely: %+v", response)
	}
}

func TestAutomaticDisableDuringInferenceWaitsForProviderAcknowledgement(t *testing.T) {
	client := automaticFixture(t)
	var calls atomic.Int32
	started, returned := make(chan struct{}), make(chan struct{})
	provider := automaticProvider(&calls)
	provider.stream = func(ctx context.Context, _ llm.Request) (llm.Stream, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		close(returned)
		return nil, ctx.Err()
	}
	runner := enhancementTestRunner(t, client, provider)
	automaticRequest(t, client, runner, "wake", "{}", "test-model")
	enhancementWait(t, started)
	enhancementCall(t, client, "automation.put", `{"id":"profile","request_id":"disable","expected_revision":1,"enabled":false}`)
	automaticRequest(t, client, runner, "wake", "{}", "test-model")
	enhancementWait(t, returned)
	automaticState(t, client, runner, "disabled")
	cancelled := enhancementCall(t, client, "enhancements.list", `{"automatic":true,"states":["cancelled"]}`)
	if !strings.Contains(string(cancelled), `"total":1`) || calls.Load() != 1 {
		t.Fatalf("cancelled work not acknowledged once: %s", cancelled)
	}
}

func TestTwoAutomaticHostsShareOneDurableProviderSlot(t *testing.T) {
	client := automaticFixture(t)
	other := store.New(client.Binary, client.DB)
	t.Cleanup(other.Close)
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	provider := automaticProvider(&calls)
	provider.stream = func(ctx context.Context, request llm.Request) (llm.Stream, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &enhancementTestStream{response: llm.Response{StopReason: llm.StopEndTurn, MaxTokensApplied: request.MaxTokens, Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: `{"title":"Resolve E42"}`}}}}}, nil
	}
	runner := enhancementTestRunner(t, client, provider)
	second := enhancementTestRunner(t, other, provider)
	automaticRequest(t, client, runner, "wake", "{}", "test-model")
	automaticRequest(t, other, second, "wake", "{}", "test-model")
	enhancementWait(t, started)
	if calls.Load() != 1 {
		t.Fatalf("two model calls entered the shared slot: %d", calls.Load())
	}
	releaseOnce.Do(func() { close(release) })
	automaticState(t, client, runner, "idle")
	automaticState(t, other, second, "idle")
	if calls.Load() != 1 {
		t.Fatalf("competing host repeated inference: %d", calls.Load())
	}
	if ready := enhancementCall(t, client, "enhancements.list", `{"automatic":true,"states":["ready"]}`); !strings.Contains(string(ready), `"total":1`) {
		t.Fatalf("unexpected proposal count: %s", ready)
	}
}

func TestAutomaticControlsRejectPayloadsWithoutOpeningStorage(t *testing.T) {
	client := store.New("must-not-run", t.TempDir()+"/absent.sqlite")
	t.Cleanup(client.Close)
	var calls atomic.Int32
	runner := enhancementTestRunner(t, client, automaticProvider(&calls))
	for _, op := range []string{"wake", "worker"} {
		for _, raw := range []string{`[]`, `{"enabled":true}`, `{"x":1,"\u0078":2}`} {
			response := automaticRequest(t, client, runner, op, raw, "test-model")
			if response.OK || response.Error == nil || response.Error.Code != "invalid_input" {
				t.Fatalf("invalid control crossed the boundary: %+v", response)
			}
		}
	}
	response := automaticRequest(t, client, runner, "worker", "{}", "test-model")
	if !response.OK || calls.Load() != 0 || !strings.Contains(string(response.Result), `"state":"not_started"`) {
		t.Fatalf("read-only worker status started work: %+v", response)
	}
	if _, err := os.Stat(client.DB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("controls opened storage: %v", err)
	}
}
