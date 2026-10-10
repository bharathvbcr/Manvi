package agents

import "strings"

// Tier names a class of model by what it is for, so a role and a router can
// say where a child belongs without naming a model id.
//
// The ids live in one place — a TierProfile — and everything else speaks in
// tiers. That is what lets a dispatching model ask for "lite" without being
// able to ask for whatever model it happens to know the name of.
type Tier string

// The tiers, cheapest first.
const (
	TierBasic   Tier = "basic"
	TierLite    Tier = "lite"
	TierHeavy   Tier = "heavy"
	TierMain    Tier = "main"
	TierAdvisor Tier = "advisor"
)

// tierOrder is the one ordering every comparison reads.
var tierOrder = []Tier{TierBasic, TierLite, TierHeavy, TierMain, TierAdvisor}

// Tiers returns every tier, cheapest first.
func Tiers() []Tier { return append([]Tier(nil), tierOrder...) }

// ParseTier reads a tier name, ignoring case and surrounding space.
func ParseTier(s string) (Tier, bool) {
	t := Tier(strings.ToLower(strings.TrimSpace(s)))
	return t, t.Rank() >= 0
}

// Rank is a tier's position in the ordering, or -1 for a name that is not a
// tier.
func (t Tier) Rank() int {
	for i, known := range tierOrder {
		if t == known {
			return i
		}
	}
	return -1
}

// String returns the tier's name.
func (t Tier) String() string { return string(t) }

// TierSpec is what one tier resolves to on the session's provider.
type TierSpec struct {
	// Model is a bare model id on the session's provider.
	Model string
	// Effort is the reasoning effort a child on this tier starts at. Empty
	// omits the field.
	Effort string
}

// TierProfile maps each tier to its model. A nil profile, or a tier missing
// from it, means the child inherits the parent's placement.
type TierProfile map[Tier]TierSpec

// DefaultTierProfile is the shipped profile on Anthropic.
func DefaultTierProfile() TierProfile {
	return TierProfile{
		TierAdvisor: {Model: "claude-fable-5-1"},
		TierMain:    {Model: "claude-opus-5-5"},
		TierHeavy:   {Model: "claude-opus-5-5"},
		TierLite:    {Model: "claude-sonnet-5-5"},
		TierBasic:   {Model: "claude-haiku-5-5"},
	}
}
