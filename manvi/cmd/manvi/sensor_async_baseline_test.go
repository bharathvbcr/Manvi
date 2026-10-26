package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The sensor records its baseline beside the turn so the first model request
// is not held behind it. These tests pin the two halves of that: the request
// really is not held, and no tool can run until the recording is finished, so
// the baseline is still a photograph of the tree before the turn touched it.

// holdVerifier is a verifier whose baseline capture waits to be let go, and
// then delegates to the verifier it wraps. It honours its context, as the
// real capture does.
type holdVerifier struct {
	pathVerifier
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}

	startOnce, releaseOnce, finishOnce sync.Once
}

func newHoldVerifier(inner pathVerifier) *holdVerifier {
	return &holdVerifier{
		pathVerifier: inner,
		started:      make(chan struct{}),
		release:      make(chan struct{}),
		finished:     make(chan struct{}),
	}
}

// let releases the capture.
func (h *holdVerifier) let() { h.releaseOnce.Do(func() { close(h.release) }) }

func (h *holdVerifier) CaptureBaseline(ctx context.Context) devcouncil.Baseline {
	h.startOnce.Do(func() { close(h.started) })
	defer h.finishOnce.Do(func() { close(h.finished) })
	select {
	case <-h.release:
	case <-ctx.Done():
		return devcouncil.Baseline{Note: "capture cancelled: " + ctx.Err().Error()}
	}
	return h.pathVerifier.CaptureBaseline(ctx)
}

func (h *holdVerifier) isFinished() bool {
	select {
	case <-h.finished:
		return true
	default:
		return false
	}
}

const asyncWait = 20 * time.Second

type runResult struct {
	out agent.Outcome
	err error
}

// runInBackground runs a turn on its own goroutine so a test that expects it
// to be blocked can observe that, and fail instead of hanging when it is.
func runInBackground(loop *agent.Loop) <-chan runResult {
	done := make(chan runResult, 1)
	go func() {
		out, err := loop.Run(context.Background(), llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}},
		})
		done <- runResult{out, err}
	}()
	return done
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(asyncWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// The first model request is issued while the baseline capture is still
// running. On a synchronous capture the turn cannot reach its first request
// until the capture has returned, so this fails by timeout rather than hanging.
func TestFirstModelRequestIsNotHeldBehindTheBaseline(t *testing.T) {
	hold := newHoldVerifier(&fakeVerifier{baseline: devcouncil.Baseline{Tree: "pre-turn"}})
	t.Cleanup(hold.let)

	provider := newTimingProvider(replayText("ok"))
	loopBus := bus.New()
	log := session.NewLog()
	loop := newTimedLoop(t, provider, loopBus, log, tools.NewRegistry(loopBus))
	s := &sensor{native: hold, log: log}
	if err := attachSensor(loopBus, s); err != nil {
		t.Fatal(err)
	}

	done := runInBackground(loop)
	select {
	case <-provider.first:
	case <-time.After(asyncWait):
		t.Fatal("the first model request was not issued while the baseline capture was pending: " +
			"the capture is on the critical path")
	}
	waitFor(t, hold.started, "the capture to start at turn start")
	if hold.isFinished() {
		t.Fatal("the capture finished before the test let it go; the request did not overlap it")
	}

	hold.let()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
	case <-time.After(asyncWait):
		t.Fatal("the turn did not finish once the capture was released")
	}
}

// A turn whose first response is a write: the write must not be in the
// baseline. The capture is held, the model's request is answered with a write
// call, and the write must wait for the capture, which is the only thing
// keeping it out of the photograph of the tree. Remove the gate and the tool
// runs while the capture is still held, which is what this asserts against.
func TestFirstResponseWriteIsNotInTheBaseline(t *testing.T) {
	root := benchGitRepo(t, 30, 2, 3)
	inner, err := devcouncil.NewPathVerifier(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	hold := newHoldVerifier(inner)
	t.Cleanup(hold.let)

	var wrote atomic.Bool
	loopBus := bus.New()
	log := session.NewLog()
	registry := tools.NewRegistry(loopBus)
	if err := registry.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "write", Description: "write a file"},
		Handler: func(_ context.Context, call tools.Call) tools.Result {
			var args struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				return tools.Errorf("bad arguments: %v", err)
			}
			if err := os.WriteFile(filepath.Join(root, args.Path), []byte("package n\n"), 0o600); err != nil {
				return tools.Errorf("write: %v", err)
			}
			wrote.Store(true)
			return tools.Result{Text: "wrote " + args.Path, Wrote: []string{args.Path}}
		},
	}); err != nil {
		t.Fatal(err)
	}

	provider := newTimingProvider(
		replayToolCall("c1", "write", `{"path":"turn_new.go"}`),
		replayText("done"),
	)
	loop := newTimedLoop(t, provider, loopBus, log, registry)
	s := &sensor{native: hold, log: log}
	if err := attachSensor(loopBus, s); err != nil {
		t.Fatal(err)
	}

	done := runInBackground(loop)
	waitFor(t, hold.started, "the capture to start at turn start")
	select {
	case <-provider.first:
	case <-time.After(asyncWait):
		t.Fatal("the first model request was never issued")
	}
	// The model has answered with a write. Give the loop every chance to run it.
	time.Sleep(300 * time.Millisecond)
	if wrote.Load() {
		t.Fatal("the turn's first write ran while the pre-turn baseline was still being recorded; " +
			"the write would be part of the tree it is diffed against")
	}

	hold.let()
	var r runResult
	select {
	case r = <-done:
	case <-time.After(asyncWait):
		t.Fatal("the turn did not finish once the capture was released")
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !wrote.Load() {
		t.Fatal("the write never ran")
	}

	baseline, err := s.await(context.Background(), s.captureTurn)
	if err != nil || !baseline.Captured() {
		t.Fatalf("no baseline: %+v, %v", baseline, err)
	}
	listing := gitIn(t, root, "ls-tree", "-r", "--name-only", baseline.Tree)
	if strings.Contains(listing, "turn_new.go") {
		t.Fatalf("the baseline contains the turn's own write:\n%s", listing)
	}
	for _, want := range []string{"pkg0/file0.go", "new/u0.go"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("the baseline is missing %s, which was in the tree before the turn:\n%s", want, listing)
		}
	}
}

// Cancelling the turn while the capture is running ends the capture: nothing is
// left running, and the check that asks for the baseline afterwards is told it
// is missing rather than handed an empty one.
func TestCancelledTurnEndsItsBaselineCapture(t *testing.T) {
	hold := newHoldVerifier(&fakeVerifier{baseline: devcouncil.Baseline{Tree: "pre-turn"}})
	t.Cleanup(hold.let)
	s, _ := newSensor(t, hold)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.begin(ctx, &agent.TurnStarting{Turn: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, hold.started, "the capture to start")

	cancel()
	waitFor(t, hold.finished, "the capture to stop after the turn was cancelled")
	// finished is closed by the capture itself; the sensor's goroutine is gone
	// once the capture's own done channel is closed.
	waitFor(t, s.capture.done, "the capture goroutine to exit")

	b := s.baselineFor(context.Background(), 1)
	if b.Captured() || b.Note == "" {
		t.Fatalf("a cancelled capture produced baseline %+v, want none with a note", b)
	}
}

// A check that stops waiting does not wait forever and does not pass off the
// missing baseline as an empty one; the recording is left to finish for the
// next look.
func TestBaselineWaitIsBoundedByTheContext(t *testing.T) {
	hold := newHoldVerifier(&fakeVerifier{baseline: devcouncil.Baseline{Tree: "pre-turn"}})
	t.Cleanup(hold.let)
	s, _ := newSensor(t, hold)
	if err := s.begin(context.Background(), &agent.TurnStarting{Turn: 1}); err != nil {
		t.Fatal(err)
	}

	waiting, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	b := s.baselineFor(waiting, 1)
	if b.Captured() || !strings.Contains(b.Note, "still being recorded") {
		t.Fatalf("a bounded wait returned %+v", b)
	}
	if err := s.dispatching(waiting, &agent.ToolsDispatching{Turn: 1}); err == nil {
		t.Fatal("dispatching released a tool over a baseline that is not recorded")
	}

	hold.let()
	got := s.baselineFor(context.Background(), 1)
	if got.Tree != "pre-turn" {
		t.Fatalf("after the capture finished the baseline is %+v", got)
	}
}

// A turn whose start event never arrived has nothing to wait for: its tools
// run, and its check says it has no baseline, as before.
func TestDispatchingWithoutARecordingDoesNotWait(t *testing.T) {
	s, _ := newSensor(t, &fakeVerifier{})
	if err := s.dispatching(context.Background(), &agent.ToolsDispatching{Turn: 7}); err != nil {
		t.Fatal(err)
	}
	if b := s.baselineFor(context.Background(), 7); b.Captured() || b.Note == "" {
		t.Fatalf("a turn that never started has baseline %+v", b)
	}
}

// A newer turn's start ends an older recording nobody waited for, and the older
// recording finishing late cannot overwrite the newer turn's baseline.
func TestOlderRecordingCannotReplaceANewerTurnsBaseline(t *testing.T) {
	older := newHoldVerifier(&fakeVerifier{baseline: devcouncil.Baseline{Tree: "turn1"}})
	t.Cleanup(older.let)
	s, _ := newSensor(t, older)
	if err := s.begin(context.Background(), &agent.TurnStarting{Turn: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, older.started, "turn 1's capture to start")

	s.native = &fakeVerifier{baseline: devcouncil.Baseline{Tree: "turn2"}}
	if err := s.begin(context.Background(), &agent.TurnStarting{Turn: 2}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, older.finished, "turn 1's capture to be cancelled by turn 2's start")

	if got := s.baselineFor(context.Background(), 2); got.Tree != "turn2" {
		t.Fatalf("turn 2's baseline is %+v", got)
	}
	if got := s.baselineFor(context.Background(), 1); got.Captured() {
		t.Fatalf("turn 1 still has a baseline after turn 2 began: %+v", got)
	}
}

// A turn that answered without touching anything never asked for its baseline,
// and must still not end with the capture running: the git children it started
// would be writing into the repository after the caller thinks the turn is over.
func TestCheckWaitsForTheTurnsCaptureBeforeTheTurnEnds(t *testing.T) {
	hold := newHoldVerifier(&fakeVerifier{baseline: devcouncil.Baseline{Tree: "pre-turn"}})
	t.Cleanup(hold.let)
	s, _ := newSensor(t, hold)
	if err := s.begin(context.Background(), &agent.TurnStarting{Turn: 1}); err != nil {
		t.Fatal(err)
	}

	returned := make(chan error, 1)
	go func() {
		returned <- s.check(context.Background(), &agent.TurnStopping{Turn: 1})
	}()
	select {
	case <-returned:
		t.Fatal("the end-of-turn check returned while the baseline capture was still running")
	case <-time.After(150 * time.Millisecond):
	}

	hold.let()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(asyncWait):
		t.Fatal("the check did not return once the capture finished")
	}
	if !hold.isFinished() {
		t.Fatal("the check returned before the capture finished")
	}
}
