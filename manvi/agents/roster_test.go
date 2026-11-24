package agents

import (
	"strings"
	"testing"
)

func TestARosterEntryNamesATierAProviderAndAModel(t *testing.T) {
	r, err := ParseRoster("fable=advisor:anthropic/claude-fable-5-1, scout = basic:Gemini/gemini-4-hydrogen@low")
	if err != nil {
		t.Fatal(err)
	}
	if r.Len() != 2 {
		t.Fatalf("parsed %d members, want 2", r.Len())
	}
	scout, ok := r.Member("scout")
	if !ok {
		t.Fatal("member scout is missing")
	}
	want := Member{Name: "scout", Tier: TierBasic, Spec: TierSpec{Provider: "gemini", Model: "gemini-4-hydrogen", Effort: "low"}}
	if scout != want {
		t.Fatalf("scout = %+v, want %+v", scout, want)
	}
	if got := scout.Placement(); got != "gemini/gemini-4-hydrogen" {
		t.Errorf("placement = %q", got)
	}
	if got := r.Providers(); strings.Join(got, ",") != "anthropic,gemini" {
		t.Errorf("providers = %v", got)
	}
}

func TestAnEmptyRosterIsNoTeam(t *testing.T) {
	for _, raw := range []string{"", "  ", ",;,"} {
		r, err := ParseRoster(raw)
		if err != nil || r.Len() != 0 {
			t.Errorf("ParseRoster(%q) = %d members, %v; want an empty roster", raw, r.Len(), err)
		}
	}
}

// Every malformed entry is reported, and none is applied: a team missing one
// member because of a typo would look configured.
func TestAMalformedRosterIsRefusedWhole(t *testing.T) {
	cases := map[string]string{
		"no tier":           "a=anthropic/claude-opus-5-5",
		"bare model":        "a=lite:claude-opus-5-5",
		"empty model":       "a=lite:anthropic/",
		"unknown tier":      "a=cheap:anthropic/claude-haiku-5-5",
		"main tier":         "a=main:anthropic/claude-opus-5-5",
		"tier as a name":    "basic=lite:anthropic/claude-haiku-5-5",
		"inherit as a name": "inherit=lite:anthropic/claude-haiku-5-5",
		"upper-case name":   "Scout=lite:anthropic/claude-haiku-5-5",
		"duplicate":         "a=lite:anthropic/x, a=basic:gemini/y",
		"no equals":         "lite:anthropic/x",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if r, err := ParseRoster("good=lite:anthropic/claude-haiku-5-5," + raw); err == nil {
				t.Fatalf("accepted %q as %d members", raw, r.Len())
			}
		})
	}
	_, err := ParseRoster("a=cheap:x/y, b=lite:nope")
	if err == nil || !strings.Contains(err.Error(), "cheap") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("both bad entries must be named, got %v", err)
	}
}

func TestARosterWiderThanItsBoundIsRefused(t *testing.T) {
	var entries []string
	for i := range MaxRosterMembers + 1 {
		entries = append(entries, "m"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26))+"=lite:anthropic/m")
	}
	if _, err := ParseRoster(strings.Join(entries, ",")); err == nil {
		t.Fatalf("a roster of %d members was accepted", MaxRosterMembers+1)
	}
}

// A member is admitted by its own tier, held to the role's ceiling exactly as
// a tier is: naming a member can never buy more than naming its tier could.
func TestAMemberIsHeldToTheRolesCeiling(t *testing.T) {
	r, err := ParseRoster("fable=advisor:anthropic/claude-fable-5-1,scout=basic:gemini/gemini-4-hydrogen")
	if err != nil {
		t.Fatal(err)
	}
	tier, m, err := r.MemberPlacement("scout", TierLite)
	if err != nil || tier != TierBasic || m == nil || m.Name != "scout" {
		t.Fatalf("scout under a lite ceiling = %v, %+v, %v", tier, m, err)
	}
	if _, _, err := r.MemberPlacement("fable", TierLite); err == nil ||
		!strings.Contains(err.Error(), "ceiling") || !strings.Contains(err.Error(), "scout") {
		t.Fatalf("an advisor member under a lite ceiling = %v; want a refusal naming the members allowed", err)
	}
	tier, m, err = r.MemberPlacement("basic", TierLite)
	if err != nil || tier != TierBasic || m != nil {
		t.Fatalf("a tier name must still mean the tier: %v, %+v, %v", tier, m, err)
	}
	if _, _, err := r.MemberPlacement("anthropic/claude-opus-5-5", TierHeavy); err == nil {
		t.Fatal("a raw placement was accepted from a dispatching model")
	}
}

func TestWithoutATeamAPlacementIsATierAsBefore(t *testing.T) {
	var none Roster
	tier, m, err := none.MemberPlacement("lite", TierHeavy)
	if err != nil || tier != TierLite || m != nil {
		t.Fatalf("= %v, %+v, %v", tier, m, err)
	}
	_, _, err = none.MemberPlacement("scout", TierHeavy)
	want, _ := ModelPlacement("scout", TierHeavy)
	_, wantErr := ModelPlacement("scout", TierHeavy)
	if err == nil || wantErr == nil || err.Error() != wantErr.Error() || want != "" {
		t.Fatalf("without a team the refusal must be ModelPlacement's own: %v vs %v", err, wantErr)
	}
}

func TestTwoSpecsAreTheSameOnlyOnTheSameProvider(t *testing.T) {
	a := TierSpec{Model: "m"}
	if !a.Same(TierSpec{Model: " m "}) {
		t.Error("the same bare model is the same spec")
	}
	if a.Same(TierSpec{Provider: "gemini", Model: "m"}) {
		t.Error("the same id on another provider is another model")
	}
	if got := (TierSpec{Provider: "gemini", Model: "m"}).Placement(); got != "gemini/m" {
		t.Errorf("placement = %q", got)
	}
	if got := (TierSpec{Provider: "gemini"}).Placement(); got != "" {
		t.Errorf("a spec with no model has no placement, got %q", got)
	}
}
