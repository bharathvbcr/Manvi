package devcouncil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// recCall is one call the adapter made to the board.
type recCall struct {
	method string
	input  json.RawMessage
}

// recBoard sits between the adapter and the real store. It records every board
// call and can fail or redirect chosen ones; everything it does not touch goes
// to the real dcstore binary, so a test is still about the real store's answer.
type recBoard struct {
	inner workBoard

	mu    sync.Mutex
	calls []recCall
	// failMethod, when it returns an error for a method, replaces the call.
	failMethod func(method string, n int) error
	// validFail, when it returns an error for the nth Valid call (1-based),
	// replaces that answer with the error.
	validFail func(n int) error
	validN    int
	repoErr   error
}

func (b *recBoard) Workbench(ctx context.Context, method string, input json.RawMessage) (json.RawMessage, error) {
	b.mu.Lock()
	b.calls = append(b.calls, recCall{method: method, input: append(json.RawMessage(nil), input...)})
	n := 0
	for _, c := range b.calls {
		if c.method == method {
			n++
		}
	}
	fail := b.failMethod
	repoErr := b.repoErr
	b.mu.Unlock()
	if method == "repositories.get" && repoErr != nil {
		return nil, repoErr
	}
	if fail != nil {
		if err := fail(method, n); err != nil {
			return nil, err
		}
	}
	return b.inner.Workbench(ctx, method, input)
}

func (b *recBoard) Valid(ctx context.Context, taskID, token string) (bool, error) {
	b.mu.Lock()
	b.validN++
	n := b.validN
	fail := b.validFail
	b.mu.Unlock()
	if fail != nil {
		if err := fail(n); err != nil {
			return false, err
		}
	}
	return b.inner.Valid(ctx, taskID, token)
}

func (b *recBoard) called(method string) []recCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []recCall
	for _, c := range b.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func (b *recBoard) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = nil
}

// workEnv is the work_* tool surface over a real repository and a real store,
// with the adapter's board wrapped by a recorder.
type workEnv struct {
	t     *testing.T
	f     *fixture
	reg   *Registry
	api   *workAPI
	pipe  *tools.Registry
	board *recBoard
	store *store.Client
}

// newWorkEnv builds a second Registry over the fixture's own store and gate with
// a Work binding, the way a host that opted in would.
func newWorkEnv(t *testing.T, runID string) *workEnv {
	t.Helper()
	f := newFixture(t)
	return f.workEnvFor(t, runID)
}

func (f *fixture) workEnvFor(t *testing.T, runID string) *workEnv {
	t.Helper()
	deps := f.reg.deps
	deps.Work = &WorkBinding{RunID: runID}
	reg, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	pipe := tools.NewRegistry(bus.New())
	if err := reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	api := reg.workAPIFor()
	if api == nil {
		t.Fatal("a Work binding produced no adapter")
	}
	rec := &recBoard{inner: deps.Store}
	api.board = rec
	return &workEnv{t: t, f: f, reg: reg, api: api, pipe: pipe, board: rec, store: deps.Store}
}

// restart is a new host process over the same store: a new Registry, a new
// adapter holding no memory of the old one, the same run id.
func (e *workEnv) restart(runID string) *workEnv {
	e.t.Helper()
	return e.f.workEnvFor(e.t, runID)
}

func (e *workEnv) call(name string, args any) tools.Result {
	e.t.Helper()
	return e.callCtx(context.Background(), name, args)
}

func (e *workEnv) callCtx(ctx context.Context, name string, args any) tools.Result {
	e.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.pipe.Run(ctx, tools.Call{ID: "w1", Name: name, Arguments: raw})
}

func (e *workEnv) callRaw(name, raw string) tools.Result {
	e.t.Helper()
	return e.pipe.Run(context.Background(), tools.Call{ID: "w1", Name: name, Arguments: json.RawMessage(raw)})
}

// checkout takes the lease on a task through the real tool, as the agent does.
func (e *workEnv) checkout(taskID string) {
	e.t.Helper()
	e.checkoutCtx(context.Background(), taskID)
}

// checkoutCtx is checkout under the caller's context, for a hook that already
// holds one.
func (e *workEnv) checkoutCtx(ctx context.Context, taskID string) {
	e.t.Helper()
	res := e.callCtx(ctx, "devcouncil_checkout_task", map[string]string{"task_id": taskID})
	if res.IsError || !strings.Contains(res.Text, `"acquired":true`) {
		e.t.Fatalf("checkout %s: %s", taskID, res.Text)
	}
}

// workbench runs a board method directly against the real store, as a host or
// a person would.
func (e *workEnv) workbench(method, input string) json.RawMessage {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := e.store.Workbench(ctx, method, json.RawMessage(input))
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, input, err)
	}
	return out
}

func (e *workEnv) workbenchErr(method, input string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := e.store.Workbench(ctx, method, json.RawMessage(input))
	return err
}

// item reads a board item straight from the store.
func (e *workEnv) item(id string) boardItem {
	e.t.Helper()
	raw := e.workbench("items.get", `{"id":"`+id+`"}`)
	var got struct {
		Item boardItem `json:"item"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		e.t.Fatal(err)
	}
	return got.Item
}

// rawItem is the item's stored JSON object, for fields boardItem does not carry.
func (e *workEnv) rawItem(id string) map[string]json.RawMessage {
	e.t.Helper()
	raw := e.workbench("items.get", `{"id":"`+id+`"}`)
	var got struct {
		Item map[string]json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		e.t.Fatal(err)
	}
	return got.Item
}

// seed puts an item with extra JSON fields merged over a minimal body, linked to
// this-repo unless the body says otherwise.
func (e *workEnv) seed(id, extra string) {
	e.t.Helper()
	body := `{"request_id":"seed-` + id + `","id":"` + id + `","expected_revision":0,"title":"` + id + `","status":"ready",` +
		`"repository_ids":["this-repo"],"primary_repository_id":"this-repo"` + extra + `}`
	e.workbench("items.put", body)
}

func (e *workEnv) addRepository(id string) {
	e.t.Helper()
	e.workbench("repositories.put", `{"request_id":"repo-`+id+`","id":"`+id+`","expected_revision":0,"name":"`+id+`","identity_key":"`+id+`"}`)
}

type refusalPayload struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Cap     string `json:"cap"`
	Field   string `json:"field"`
	Limit   int    `json:"limit"`
	Got     int    `json:"got"`
}

func (e *workEnv) refusal(res tools.Result) refusalPayload {
	e.t.Helper()
	if !res.IsError {
		e.t.Fatalf("expected a refusal, got success: %s", res.Text)
	}
	var p refusalPayload
	if err := json.Unmarshal([]byte(res.Text), &p); err != nil {
		e.t.Fatalf("refusal is not typed JSON: %v (%q)", err, res.Text)
	}
	if p.Error == "" {
		e.t.Fatalf("refusal carries no code: %q", res.Text)
	}
	if res.Rule != "work."+p.Error {
		e.t.Fatalf("refusal rule %q does not match its code %q", res.Rule, p.Error)
	}
	return p
}

func (e *workEnv) success(res tools.Result) map[string]any {
	e.t.Helper()
	if res.IsError {
		e.t.Fatalf("expected success, got: %s", res.Text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		e.t.Fatalf("result is not JSON: %v (%q)", err, res.Text)
	}
	return out
}

// unfence returns the JSON inside an agent-facing read and fails unless the
// standing header and a fence are present.
func unfence(t *testing.T, text string) string {
	t.Helper()
	body, err := unframeBoard(text)
	if err != nil {
		t.Fatalf("%v: %q", err, text[:min(len(text), 160)])
	}
	return body
}

// unframeBoard splits a board read into its body, failing unless the text is
// exactly one frame: a BEGIN line carrying a nonce, the standing note, the
// body, and an END line carrying the same nonce as the last line.
func unframeBoard(text string) (string, error) {
	lines := strings.Split(text, "\n")
	if len(lines) < 4 {
		return "", fmt.Errorf("a board read has %d lines, want a frame", len(lines))
	}
	begin := untrustedBeginPrefix + boardFrameLabel + " "
	if !strings.HasPrefix(lines[0], begin) || !strings.HasSuffix(lines[0], " ---") {
		return "", fmt.Errorf("a read does not open with the board's BEGIN marker: %q", lines[0])
	}
	nonce := strings.TrimSuffix(strings.TrimPrefix(lines[0], begin), " ---")
	if nonce == "" {
		return "", errors.New("the BEGIN marker carries no nonce")
	}
	if lines[1] != boardFrameNote || lines[2] != "" {
		return "", fmt.Errorf("the standing note is missing or altered: %q", lines[1])
	}
	end := untrustedEndPrefix + boardFrameLabel + " " + nonce + " ---"
	if lines[len(lines)-1] != end {
		return "", fmt.Errorf("the last line is not the END marker for this nonce: %q", lines[len(lines)-1])
	}
	body := strings.Join(lines[3:len(lines)-1], "\n")
	if strings.Contains(body, nonce) {
		return "", errors.New("the nonce occurs inside the body, so the frame could be closed from within")
	}
	return body, nil
}

func (e *workEnv) putCalls() int { return len(e.board.called("items.put")) }
