package agents

import "testing"

// TestTiersAreOrderedCheapestFirst pins the order. Something that escalates by
// moving one position along Tiers would, on a reordered list, move a stuck
// child to a cheaper model while reporting that it had bought more.
func TestTiersAreOrderedCheapestFirst(t *testing.T) {
	want := []Tier{TierBasic, TierLite, TierHeavy, TierMain, TierAdvisor}
	got := Tiers()
	if len(got) != len(want) {
		t.Fatalf("Tiers() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Tiers() = %v, want %v", got, want)
		}
		if got[i].Rank() != i {
			t.Errorf("%s.Rank() = %d, want %d", got[i], got[i].Rank(), i)
		}
	}
	// A caller that edits what it was handed must not reorder everyone's.
	got[0] = TierAdvisor
	if Tiers()[0] != TierBasic {
		t.Fatal("Tiers() returned its own backing array; a caller rewrote the shared order")
	}
}

func TestParseTierIsStrict(t *testing.T) {
	for raw, want := range map[string]Tier{
		"basic": TierBasic, " Lite ": TierLite, "HEAVY": TierHeavy, "main": TierMain, "advisor": TierAdvisor,
	} {
		got, ok := ParseTier(raw)
		if !ok || got != want {
			t.Errorf("ParseTier(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "opus", "advisors", "main.effort", "medium"} {
		if got, ok := ParseTier(raw); ok {
			t.Errorf("ParseTier(%q) = %q, want a refusal", raw, got)
		}
	}
	if Tier("opus").Rank() != -1 {
		t.Error("an unknown tier ranked as a known one")
	}
}

// TestDefaultTierProfileCoversEveryTier. A tier with no default model is a tier
// a routed child cannot be placed on, and the gap would surface only when a
// role first asked for it.
func TestDefaultTierProfileCoversEveryTier(t *testing.T) {
	profile := DefaultTierProfile()
	want := map[Tier]string{
		TierAdvisor: "claude-fable-5-1",
		TierMain:    "claude-opus-5-5",
		TierHeavy:   "claude-opus-5-5",
		TierLite:    "claude-sonnet-5-5",
		TierBasic:   "claude-haiku-5-5",
	}
	if len(profile) != len(Tiers()) {
		t.Fatalf("profile has %d tiers, want %d", len(profile), len(Tiers()))
	}
	for tier, model := range want {
		spec, ok := profile[tier]
		if !ok || spec.Model != model || spec.Effort != "" {
			t.Errorf("%s = %+v, want model %s with no effort", tier, spec, model)
		}
	}
}
