package agents

import (
	"slices"
	"testing"
)

func TestTiersAreOrderedWeakestToStrongest(t *testing.T) {
	want := []Tier{TierBasic, TierLite, TierMain, TierHeavy, TierAdvisor}
	if got := Tiers(); !slices.Equal(got, want) {
		t.Fatalf("Tiers() = %v, want %v", got, want)
	}
	for i, tier := range want {
		if tier.Rank() != i {
			t.Errorf("%s.Rank() = %d, want %d", tier, tier.Rank(), i)
		}
		if i > 0 && !want[i-1].Less(tier) {
			t.Errorf("%s should be below %s", want[i-1], tier)
		}
		if tier.Less(tier) {
			t.Errorf("%s is below itself", tier)
		}
	}
}

func TestTiersReturnsACopy(t *testing.T) {
	got := Tiers()
	got[0] = "mutated"
	if Tiers()[0] != TierBasic {
		t.Fatal("mutating the returned slice changed the package's ordering")
	}
}

func TestParseTierRefusesWhatIsNotATier(t *testing.T) {
	for _, tier := range Tiers() {
		got, err := ParseTier(string(tier))
		if err != nil || got != tier {
			t.Errorf("ParseTier(%q) = %q, %v", tier, got, err)
		}
	}
	for _, raw := range []string{"", " ", "Main", "MAIN", "main ", "opus", "tier.main", "judge"} {
		if got, err := ParseTier(raw); err == nil {
			t.Errorf("ParseTier(%q) = %q, want a refusal", raw, got)
		}
	}
	if Tier("nope").Valid() || Tier("nope").Rank() != -1 {
		t.Error("an unknown tier must be invalid and rank -1")
	}
	if Tier("nope").Less(TierBasic) || TierBasic.Less("nope") {
		t.Error("an unknown tier must compare as neither above nor below anything")
	}
}

func TestTierKeysAreDerivedFromOneNamespace(t *testing.T) {
	if got := TierLite.ModelKey(); got != "llm.tier.lite" {
		t.Errorf("ModelKey = %q", got)
	}
	if got := TierLite.EffortKey(); got != "llm.tier.lite.effort" {
		t.Errorf("EffortKey = %q", got)
	}
}

func TestDefaultProfileMatchesTheDecidedTiering(t *testing.T) {
	want := map[Tier]string{
		TierAdvisor: "claude-fable-5-1",
		TierMain:    "claude-opus-5-5",
		TierHeavy:   "claude-opus-5-5",
		TierLite:    "claude-sonnet-5-5",
		TierBasic:   "claude-haiku-5-5",
	}
	for tier, model := range want {
		got, ok := DefaultTierProfile(tier)
		if !ok || got.Model != model || got.Effort != "" {
			t.Errorf("DefaultTierProfile(%s) = %+v, %v; want model %s and no effort", tier, got, ok, model)
		}
	}
	if _, ok := DefaultTierProfile("nope"); ok {
		t.Error("an unknown tier has no default profile")
	}
}
