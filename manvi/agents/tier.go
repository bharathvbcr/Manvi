package agents

import (
	"fmt"
	"slices"
)

// Tier names a rung of the model ladder a sub-agent can be placed on.
//
// This file only defines the rungs and what each one runs by default. It does
// not choose one for a task: routing belongs to the planner, and it is
// structural — what a role is allowed to do — never a classifier over the
// prompt. It is pure for the same reason ParsePlacement is: no provider is
// imported, so a tier is a name and a pair of strings until the composition
// root resolves it against a catalogue.
type Tier string

const (
	// TierAdvisor is the strongest model, consulted rather than run as a worker.
	TierAdvisor Tier = "advisor"
	// TierMain drives the session itself.
	TierMain Tier = "main"
	// TierHeavy runs sub-agents that write.
	TierHeavy Tier = "heavy"
	// TierLite runs read-only sub-agents.
	TierLite Tier = "lite"
	// TierBasic runs wide, simple fan-out.
	TierBasic Tier = "basic"
)

// tierOrder is weakest to strongest. Heavy sits above main because it is the
// rung a writer escalates to; both are Opus 5.5 by default, so the order is the
// order of responsibility, not of price.
var tierOrder = []Tier{TierBasic, TierLite, TierMain, TierHeavy, TierAdvisor}

// Tiers lists every tier, weakest first. The slice is a copy.
func Tiers() []Tier { return slices.Clone(tierOrder) }

// Valid reports whether t is one of the five tiers.
func (t Tier) Valid() bool { return t.Rank() >= 0 }

// Rank is t's position in the ordering, 0 for the weakest, or -1 for a name
// that is not a tier.
func (t Tier) Rank() int { return slices.Index(tierOrder, t) }

// Less reports whether t is strictly below other. An unknown tier is neither
// below nor above anything: a comparison that guessed a position for a name
// nobody defined would be a routing decision made by a typo.
func (t Tier) Less(other Tier) bool {
	a, b := t.Rank(), other.Rank()
	return a >= 0 && b >= 0 && a < b
}

// ParseTier accepts exactly the five lower-case names.
func ParseTier(raw string) (Tier, error) {
	if t := Tier(raw); t.Valid() {
		return t, nil
	}
	return "", fmt.Errorf("%q is not a tier; the tiers are %v", raw, tierOrder)
}

// TierKeyPrefix is the configuration namespace every tier setting lives in.
const TierKeyPrefix = "llm.tier."

// ModelKey is the setting that names the tier's model.
func (t Tier) ModelKey() string { return TierKeyPrefix + string(t) }

// EffortKey is the setting that names the tier's reasoning effort.
func (t Tier) EffortKey() string { return t.ModelKey() + ".effort" }

// TierProfile is what a tier runs: a model id and an effort. An empty Effort
// omits the field, leaving the provider's default in force.
type TierProfile struct {
	Model  string
	Effort string
}

// DefaultTierProfile is the shipped profile for t, or false for a name that is
// not a tier.
func DefaultTierProfile(t Tier) (TierProfile, bool) {
	switch t {
	case TierAdvisor:
		return TierProfile{Model: "claude-fable-5-1"}, true
	case TierMain, TierHeavy:
		return TierProfile{Model: "claude-opus-5-5"}, true
	case TierLite:
		return TierProfile{Model: "claude-sonnet-5-5"}, true
	case TierBasic:
		return TierProfile{Model: "claude-haiku-5-5"}, true
	}
	return TierProfile{}, false
}
