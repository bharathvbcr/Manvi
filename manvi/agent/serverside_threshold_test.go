package agent

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The adapter asks the server to clear at the point the loop would have
// compacted. Its arithmetic is a repeat of Loop.Budget's (the adapter cannot
// import this package), so the loop's own Budget is the oracle: where it puts
// the threshold is where the server is told to start, and the gap down to its
// target is the least a clear has to free.
func TestServerSideClearingStartsWhereTheLoopWouldHaveCompacted(t *testing.T) {
	for _, tc := range []struct {
		model     string
		window    int
		maxTokens int
	}{
		{"claude-opus-5-5", 1_000_000, 1024},
		{"claude-opus-5-5", 1_000_000, 64_000},
		{"claude-opus-5-5", 1_000_000, 600_000}, // reservation above half the window
		{"claude-haiku-4-5", 200_000, 150_000},
		{"claude-haiku-4-5", 200_000, 8192},
	} {
		provider := windowed{Provider: anthropic.New("http://unused", adaptertest.Secret("k")), window: tc.window}
		loop, err := NewLoop(Config{
			Provider: provider, Model: tc.model, MaxSteps: 4, MaxTokens: tc.maxTokens,
		}, bus.New(), session.NewLog(), tools.NewRegistry(bus.New()))
		if err != nil {
			t.Fatal(err)
		}
		// The adapter reads the model's catalogue window, so the fixture's window
		// has to be that one or the two are not measuring the same budget.
		if catalogue, _ := provider.Provider.Capability(tc.model); catalogue.ContextWindow != tc.window {
			t.Fatalf("%s: catalogue window is %d, fixture says %d", tc.model, catalogue.ContextWindow, tc.window)
		}
		want := loop.Budget()
		edit := anthropic.ContextEditsFor(tc.model, tc.maxTokens)[0]
		if edit.Trigger.Value != want.Threshold() {
			t.Errorf("%s window=%d max=%d: trigger %d, loop threshold %d", tc.model, tc.window, tc.maxTokens, edit.Trigger.Value, want.Threshold())
		}
		gap := want.Threshold() - want.Target()
		if d := edit.ClearAtLeast.Value - gap; d < -1 || d > 1 {
			t.Errorf("%s window=%d max=%d: clear_at_least %d, loop gap %d", tc.model, tc.window, tc.maxTokens, edit.ClearAtLeast.Value, gap)
		}
	}
}
