package anthropic

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// The adapter and the loop must agree on where clearing starts, or the server
// clears at a point the harness did not plan around. agent.Budget cannot be
// imported here, so its arithmetic is repeated and this holds the repeat to the
// numbers the loop's own tests pin: window minus the reserved output (at most
// half the window) minus 2048, floored at 4096, and a clear that must free the
// gap down to seven tenths of that.
func TestContextEditThresholdsFollowTheLoopsBudget(t *testing.T) {
	for _, tc := range []struct {
		model            string
		maxTokens        int
		wantTrigger      int
		wantClearAtLeast int
	}{
		{"claude-opus-5-5", 4096, 1_000_000 - 4096 - 2048, (1_000_000 - 4096 - 2048) * 3 / 10},
		// A reservation above half the window is held to half.
		{"claude-haiku-4-5", 190_000, 100_000 - 2048, (100_000 - 2048) * 3 / 10},
	} {
		w, ok := Capability(tc.model)
		if !ok {
			t.Fatalf("%s is not in the catalogue", tc.model)
		}
		wantWindow := map[string]int{"claude-opus-5-5": 1_000_000, "claude-haiku-4-5": 200_000}[tc.model]
		if w.ContextWindow != wantWindow {
			t.Fatalf("%s window = %d, want %d; the fixture's arithmetic is stale", tc.model, w.ContextWindow, wantWindow)
		}
		e := ContextEditsFor(tc.model, tc.maxTokens)[0]
		if e.Trigger.Value != tc.wantTrigger {
			t.Errorf("%s trigger = %d, want %d", tc.model, e.Trigger.Value, tc.wantTrigger)
		}
		got := e.ClearAtLeast.Value
		if got < tc.wantClearAtLeast-1 || got > tc.wantClearAtLeast+1 {
			t.Errorf("%s clear_at_least = %d, want about %d", tc.model, got, tc.wantClearAtLeast)
		}
	}
	// A model the catalogue does not hold gets the server's thresholds.
	e := ContextEditsFor("claude-not-a-model", 4096)[0]
	if e.Trigger != nil || e.ClearAtLeast != nil || e.Keep == nil {
		t.Errorf("unknown model: %+v", e)
	}
}

// llm cannot import this package, so it repeats the adapter's name. The two
// must not drift, or the harness would stop treating Anthropic as the provider
// that binds thinking to history.
func TestTheNeutralLayerRecognisesThisAdapterAsBindingThinking(t *testing.T) {
	if !llm.BindsThinkingToHistory(Name) {
		t.Fatalf("llm.BindsThinkingToHistory(%q) is false; the neutral layer's copy of the name has drifted", Name)
	}
	for _, other := range []string{"openai", "gemini", "xai", "local", ""} {
		if llm.BindsThinkingToHistory(other) {
			t.Errorf("%q is not a provider that binds thinking to history", other)
		}
	}
}
