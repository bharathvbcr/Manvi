package main

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

	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
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
	// live counts captures that have started and not yet returned: the seam
	// that says whether a turn left one running.
	live atomic.Int32
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
	h.live.Add(1)
	defer h.live.Add(-1)
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

// failingTurn is a recorded response that makes the provider return an error.
func failingTurn() replay.Turn { return replay.Turn{Err: "the provider is down"} }

// Every way out of a turn ends the baseline capture. The turns here leave
// before the terminal checkpoint, which is the one place a capture was awaited
// before: a provider error at the first step, a cancelled context, and a panic.
// Each is judged by the live-capture count and by whether the capture finished,
// read the instant Run returns, not after waiting for it.
func TestEveryExitFromATurnEndsItsBaselineCapture(t *testing.T) {
	cases := []struct {
		name string
		// arrange sets up the provider and returns the context to run under.
		arrange func(p *timingProvider) (context.Context, context.CancelFunc)
		panics  bool
	}{
		{"provider error at the first step", func(p *timingProvider) (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		}, false},
		{"context cancelled at the first request", func(p *timingProvider) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			p.onFirst = cancel
			return ctx, cancel
		}, false},
		{"panic in the provider", func(p *timingProvider) (context.Context, context.CancelFunc) {
			p.onFirst = func() { panic("the provider panicked") }
			return context.WithCancel(context.Background())
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hold := newHoldVerifier(&fakeVerifier{baseline: devcouncil.Baseline{Tree: "pre-turn"}})
			t.Cleanup(hold.let)

			provider := newTimingProvider(failingTurn(), replayText("never reached"))
			ctx, cancel := tc.arrange(provider)
			defer cancel()
			loopBus := bus.New()
			log := session.NewLog()
			loop := newTimedLoop(t, provider, loopBus, log, tools.NewRegistry(loopBus))
			if err := attachSensor(loopBus, &sensor{native: hold, log: log}); err != nil {
				t.Fatal(err)
			}

			var runErr error
			func() {
				defer func() {
					if r := recover(); r != nil {
						if !tc.panics {
							panic(r)
						}
						runErr = errors.New("panicked")
					}
				}()
				_, runErr = loop.Run(ctx, llm.Message{
					Role:    llm.RoleUser,
					Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}},
				})
			}()
			if runErr == nil {
				t.Fatal("the turn was expected to fail")
			}
			if n := hold.live.Load(); n != 0 || !hold.isFinished() {
				t.Fatalf("Run returned (%v) with %d baseline capture(s) still running, finished=%v",
					runErr, n, hold.isFinished())
			}
		})
	}
}

// The capture's temp directory is gone when Run returns from a turn that failed
// before any check, with the real verifier and real git children behind it.
func TestAFailedTurnLeavesNoBaselineTempDirectory(t *testing.T) {
	root := benchGitRepo(t, 300, 10, 10)
	inner, err := devcouncil.NewPathVerifier(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)

	provider := newTimingProvider(failingTurn())
	loopBus := bus.New()
	log := session.NewLog()
	loop := newTimedLoop(t, provider, loopBus, log, tools.NewRegistry(loopBus))
	if err := attachSensor(loopBus, &sensor{native: inner, log: log}); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}},
	}); err == nil {
		t.Fatal("the turn was expected to fail")
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("%s was left behind by a failed turn", e.Name())
	}
}

// The gate never releases a cancelled turn's tools. A finished capture and a
// cancelled context are both ready here, which is the case a select would
// resolve at random; the answer has to be the same every time.
func TestDispatchingNeverReleasesACancelledTurn(t *testing.T) {
	s, _ := newSensor(t, &fakeVerifier{baseline: devcouncil.Baseline{Tree: "pre-turn"}})
	if err := s.begin(context.Background(), &agent.TurnStarting{Turn: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.await(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 5000; i++ {
		if err := s.dispatching(ctx, &agent.ToolsDispatching{Turn: 1}); err == nil {
			t.Fatalf("a cancelled turn was released to dispatch on attempt %d", i)
		}
	}
}
