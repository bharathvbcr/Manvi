package serve

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// The agent-facing work_* tools live in manvi/devcouncil (see work_tools.go
// there) and are the in-process agent's, not this host connection's. These tests
// pin the two ways this package could quietly take part in them.

// Criterion: Status moves. The wake in workbench.go stays conditional on
// automatic_enhancement_queued: a save that changes neither title nor
// description, which is every move an agent's own-task update makes, wakes
// nothing, and a save that does change text still wakes. The control is what
// lets the first half fail.
func TestStatusOnlySaveWakesNothingAndATextSaveStillDoes(t *testing.T) {
	client := enhancementFixture(t)
	var calls atomic.Int32
	runner := enhancementTestRunner(t, client, automaticProvider(&calls))
	module := WorkbenchModule{Client: client, Runner: runner, Configuration: &EnhancementConfiguration{OK: true, Provider: "local", Model: "test-model", Providers: []string{"local"}}}

	put := func(requestID string, revision int, title, status string) Response {
		t.Helper()
		params := `{"id":"t","request_id":"` + requestID + `","expected_revision":` + strconv.Itoa(revision) +
			`,"title":` + strconv.Quote(title) + `,"description":"Keep the original task evidence.","status":"` + status +
			`","repository_ids":["r"],"primary_repository_id":"r"}`
		responses := roundTrip(t, Options{Modules: []Module{module}}, Request{ID: requestID, Op: "work.items.put", Params: json.RawMessage(params)})
		if len(responses) != 1 || !responses[0].OK {
			t.Fatalf("items.put %s failed: %+v", requestID, responses)
		}
		return responses[0]
	}

	if state := runner.AutomaticStatus().State; state != "not_started" {
		t.Fatalf("the worker is %q before any host save; the test needs it not started", state)
	}
	moved := put("status-move", 1, "fix E42", "in_progress")
	if strings.Contains(string(moved.Result), `"automatic_enhancement_queued":true`) {
		t.Fatalf("a status-only save queued an enhancement: %s", moved.Result)
	}
	if state := runner.AutomaticStatus().State; state != "not_started" {
		t.Fatalf("a status-only save woke the automatic worker (state %q)", state)
	}

	edited := put("text-edit", 2, "fix E42 again", "in_progress")
	if !strings.Contains(string(edited.Result), `"automatic_enhancement_queued":true`) {
		t.Fatalf("control failed: a text save did not queue an enhancement, so the wake path is not what this test exercises: %s", edited.Result)
	}
	if state := runner.AutomaticStatus().State; state == "not_started" {
		t.Fatal("a text save that queued an enhancement did not wake the worker")
	}
}

// requestRecorder is a provider that records every request it is sent.
type requestRecorder struct {
	mu       sync.Mutex
	requests []llm.Request
}

func (r *requestRecorder) provider() *enhancementTestProvider {
	p := &enhancementTestProvider{name: "local", cap: llm.Capability{Provider: "local", Model: "test-model", ContextWindow: 100000, MaxOutputTokens: 8192}}
	p.stream = func(_ context.Context, req llm.Request) (llm.Stream, error) {
		r.mu.Lock()
		r.requests = append(r.requests, req)
		r.mu.Unlock()
		return &enhancementTestStream{response: llm.Response{StopReason: llm.StopEndTurn, MaxTokensApplied: req.MaxTokens,
			Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: `{"title":"Resolve E42"}`}}}}}, nil
	}
	return p
}

// Criterion: Invariant. An enhancement run holds no work_* tool. The run is one
// completion with no tool surface, and the files that build its request do not import
// the package that defines the tools.
func TestEnhancementRunHoldsNoWorkTool(t *testing.T) {
	client := enhancementFixture(t)
	recorder := &requestRecorder{}
	runner := enhancementTestRunner(t, client, recorder.provider())
	started, err := runner.Generate(t.Context(), json.RawMessage(`{"id":"e","request_id":"start","expected_revision":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(started), `"state":"running"`) && !strings.Contains(string(started), `"state":"ready"`) {
		t.Fatalf("generation did not start: %s", started)
	}
	runner.mu.Lock()
	job := runner.active
	runner.mu.Unlock()
	if job != nil {
		enhancementWait(t, job.done)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.requests) == 0 {
		t.Fatal("no provider request was made, so there is no tool surface to inspect")
	}
	for _, req := range recorder.requests {
		if len(req.Tools) != 0 {
			t.Errorf("an enhancement request carries %d tool(s): %+v", len(req.Tools), req.Tools)
		}
		encoded, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "work_") {
			t.Error("an enhancement request names a work_ tool")
		}
	}

	// Structural: the files that build an enhancement request do not import the
	// package that defines the agent tools, so no path through them can put one
	// in a request. (Other files here, the chat harness for one, do import it.)
	files := []string{"enhancements.go", "enhancement_output.go", "automation.go", "refine.go"}
	for _, file := range files {
		file = filepath.Clean(file)
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			if strings.HasSuffix(strings.Trim(imp.Path.Value, `"`), "/manvi/devcouncil") {
				t.Errorf("%s imports the agent tool package", file)
			}
		}
	}
}

// Criterion: Seam. hello.ops for the host connection carries no agent tool and
// no operation this change added: the set is the store's methods plus the host's
// own ops, and no name is spelled like a work_ tool.
func TestHostOperationsCarryNoAgentTool(t *testing.T) {
	client := enhancementFixture(t)
	responses := roundTrip(t, Options{Modules: []Module{WorkbenchModule{Client: client}}}, Request{ID: "hello", Op: OpHello})
	if len(responses) != 1 || !responses[0].OK {
		t.Fatalf("hello failed: %+v", responses)
	}
	var hello struct {
		Ops []string `json:"ops"`
	}
	if err := json.Unmarshal(responses[0].Result, &hello); err != nil {
		t.Fatal(err)
	}
	if len(hello.Ops) == 0 {
		t.Fatal("hello lists no operations")
	}
	for _, op := range hello.Ops {
		if strings.Contains(op, "work_") {
			t.Errorf("hello.ops carries an agent tool name: %s", op)
		}
	}
}
