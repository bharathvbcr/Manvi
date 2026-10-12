package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/tools"
	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// A turn cancelled while a tool is running leaves the log balanced and usable:
// the next Run on the same log (the TUI's very next prompt) must succeed and
// stay balanced, whichever await point the cancel landed on.
func TestACancelDuringToolDispatchLeavesTheLogBalancedAndReusable(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "slow", `{}`),
		textTurn("second turn works"),
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "slow"},
		Handler: func(c context.Context, _ tools.Call) tools.Result {
			close(started)
			<-c.Done()
			return tools.Result{Text: "interrupted", IsError: true}
		},
	}); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-started
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		_, err := h.loop.Run(ctx, userMsg("go"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			// A tool that reports its own interruption may let the turn end
			// without an error; what must hold either way is the balance below.
			t.Logf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the cancel")
	}
	requireBalanced(t, h.log)
	if _, err := h.loop.Run(context.Background(), userMsg("again")); err != nil {
		t.Fatalf("the turn after a mid-dispatch cancel failed: %v", err)
	}
	requireBalanced(t, h.log)
}

// hangingProvider streams one chunk and then blocks in Next until its context
// ends, so a cancel lands while the stream is live rather than before it.
type hangingProvider struct {
	llm.Provider
	streaming chan struct{}
}

type hangingStream struct {
	ctx       context.Context
	streaming chan struct{}
	sent      bool
}

func (p *hangingProvider) Stream(ctx context.Context, _ llm.Request) (llm.Stream, error) {
	return &hangingStream{ctx: ctx, streaming: p.streaming}, nil
}

func (s *hangingStream) Next() (llm.Chunk, error) {
	if !s.sent {
		s.sent = true
		return llm.Chunk{Kind: llm.ChunkText, Text: "partial"}, nil
	}
	close(s.streaming)
	<-s.ctx.Done()
	return llm.Chunk{}, s.ctx.Err()
}
func (s *hangingStream) Response() (llm.Response, error) { return llm.Response{}, s.ctx.Err() }
func (s *hangingStream) Close() error                    { return nil }

func TestACancelMidStreamLeavesTheLogBalancedAndReusable(t *testing.T) {
	h := build(t, []replay.Turn{textTurn("second turn works")}, nil)
	hp := &hangingProvider{Provider: h.provider, streaming: make(chan struct{})}
	h.loop.cfg.Provider = hp
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-hp.streaming
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		_, err := h.loop.Run(ctx, userMsg("go"))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run must return the cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after a mid-stream cancel")
	}
	requireBalanced(t, h.log)
	h.loop.cfg.Provider = h.provider
	if _, err := h.loop.Run(context.Background(), userMsg("again")); err != nil {
		t.Fatalf("the turn after a mid-stream cancel failed: %v", err)
	}
	requireBalanced(t, h.log)
}

// The projected stream is what NDJSON consumers delimit turns by: after a
// failed turn and a following good one, turn.start and turn.end counts agree.
func TestAFailedTurnProjectsEqualTurnStartAndEndCounts(t *testing.T) {
	loop, log := loopOver(t, []replay.Turn{{Err: "boom"}, answerTurn("second turn works")})
	if _, err := loop.Run(context.Background(), goPrompt()); err == nil {
		t.Fatal("the provider error was swallowed")
	}
	if _, err := loop.Run(context.Background(), goPrompt()); err != nil {
		t.Fatal(err)
	}
	starts, ends := 0, 0
	for _, e := range log.Events() {
		for _, ev := range ui.Project(e) {
			switch ev.Kind {
			case ui.KindTurnStart:
				starts++
			case ui.KindTurnEnd:
				ends++
			}
		}
	}
	if starts != 2 || ends != 2 {
		t.Fatalf("projected turn.start/turn.end = %d/%d, want 2/2", starts, ends)
	}
}
