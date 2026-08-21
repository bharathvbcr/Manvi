package llm_test

import (
	"testing"

	"manvi/llm"
	"manvi/llm/anthropic"
	"manvi/llm/gemini"
	"manvi/llm/local"
	"manvi/llm/xai"
)

// rank is the harness's effort vocabulary, least reasoning first.
//
// It is written here rather than derived from any adapter because it is the
// claim being checked: llm.Capability documents EffortLevels as ordered from
// least to most, and agent.EffortPlan climbs that order one rung at a time. An
// adapter that listed its levels in any other order would make "one rung more
// thinking" mean less thinking, silently and only on the turns that were
// already going badly.
var rank = map[string]int{
	"none": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5,
}

func checkOrdered(t *testing.T, what string, levels []string) {
	t.Helper()
	last := -1
	for _, level := range levels {
		at, known := rank[level]
		if !known {
			t.Fatalf("%s declares effort level %q, which is outside the harness's vocabulary %v; "+
				"a level nothing can order cannot be escalated to", what, level, rank)
		}
		if at <= last {
			t.Fatalf("%s declares levels %v, which are not ordered from least reasoning to most", what, levels)
		}
		last = at
	}
}

// TestEveryAdapterOrdersItsEffortLevelsLeastFirst is the guard on a contract
// that is easy to break by accident — the lists are hand-written per model —
// and whose breakage is invisible until a stuck turn escalates downwards.
func TestEveryAdapterOrdersItsEffortLevelsLeastFirst(t *testing.T) {
	catalogued := map[string]func(string) (llm.Capability, bool){
		"anthropic": anthropic.Capability,
		"gemini":    gemini.Capability,
		"xai":       xai.Capability,
	}
	models := map[string][]string{
		"anthropic": anthropic.Models(),
		"gemini":    gemini.Models(),
		"xai":       xai.Models(),
	}

	reasoning := 0
	for provider, capability := range catalogued {
		for _, model := range models[provider] {
			c, ok := capability(model)
			if !ok {
				t.Fatalf("%s lists model %q and then does not serve it", provider, model)
			}
			if len(c.EffortLevels) == 0 {
				continue
			}
			reasoning++
			checkOrdered(t, provider+"/"+model, c.EffortLevels)
		}
	}
	if reasoning == 0 {
		t.Fatal("no catalogued model declared any effort levels; this test checked nothing")
	}

	// The local adapter has no catalogue — the operator declares the model —
	// so its single shared list is the thing to check.
	checkOrdered(t, "local", local.ReasoningEfforts)
}
