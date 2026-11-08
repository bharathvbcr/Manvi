package main

import (
	"testing"
)

// Adversarial review of w13-thinking, item 3. The wire frame delivers a changed
// system prompt as a full-prompt notice, so a prompt that differs on every turn
// of one conversation would grow the context by the whole prompt each turn.
// This drives the real assembly path (the one prepareTurn and run.go use) with
// identical inputs the way two consecutive turns would, and compares bytes.
func TestAdvAssembledPromptIsByteStableAcrossTurnsOnAnthropic(t *testing.T) {
	reg := registryWith(t, nil)
	opts := PromptOptions{
		Provider:           "anthropic",
		TaskToolsOffered:   true,
		ActiveGroups:       []string{"core", "navigation"},
		CodeMapAvailable:   true,
		DocLookupAvailable: true,
	}
	first, _, _ := assemblePromptReported(reg, opts)
	if first == "" {
		t.Fatal("the assembly path produced an empty prompt")
	}
	for turn := 2; turn <= 25; turn++ {
		next, _, _ := assemblePromptReported(reg, opts)
		if next != first {
			t.Fatalf("turn %d assembled a different prompt from turn 1 with identical inputs "+
				"(%d bytes vs %d); every turn would ship a full-prompt notice", turn, len(next), len(first))
		}
	}
}
