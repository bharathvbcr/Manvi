package agents

import "strings"

// Tier names a class of work by what it is allowed to cost, not by which model
// does it.
//
// A role asks for a tier; the operator's profile says which model serves it.
// Keeping the two apart is what lets a profile be retuned — a cheaper lite
// model, a dearer advisor — without touching a single role definition, and what
// keeps this package free of provider knowledge: the ids in DefaultTierProfile
// are data, and whether a provider serves them is decided where the provider is
// built, not here.
type Tier string

const (
	// TierBasic is wide, simple fan-out: many narrow children each answering a
	// small question.
	TierBasic Tier = "basic"
	// TierLite is read-only delegated work — searching, reading, reporting.
	TierLite Tier = "lite"
	// TierHeavy is delegated work that writes.
	TierHeavy Tier = "heavy"
	// TierMain is the session's own agent.
	TierMain Tier = "main"
	// TierAdvisor is the reviewer the main agent consults before committing to
	// a plan and before declaring work done.
	TierAdvisor Tier = "advisor"
)

// tiers is the order every listing and every comparison uses: cheapest first.
var tiers = []Tier{TierBasic, TierLite, TierHeavy, TierMain, TierAdvisor}

// Tiers lists every tier, ordered cheapest to dearest.
//
// The order is a contract rather than a presentation detail: anything that
// escalates by moving one position along it would, given a list in any other
// order, move a stuck child to a cheaper tier.
func Tiers() []Tier { return append([]Tier(nil), tiers...) }

// ParseTier reads a tier name, ignoring case and surrounding space, and reports
// false for anything that is not one. An unknown name is never mapped to a
// nearby tier: a typo that silently chose the main model would bill the dearest
// tier for work someone meant to send to the cheapest.
func ParseTier(s string) (Tier, bool) {
	t := Tier(strings.ToLower(strings.TrimSpace(s)))
	if t.Rank() < 0 {
		return "", false
	}
	return t, true
}

// Rank is the tier's position in Tiers, or -1 for a value that is not a tier.
func (t Tier) Rank() int {
	for i, known := range tiers {
		if t == known {
			return i
		}
	}
	return -1
}

func (t Tier) String() string { return string(t) }

// TierSpec is what serves one tier: a bare model id on the session's provider,
// and the reasoning effort requests at that tier carry. An empty Effort omits
// the field, so the provider's own default applies.
type TierSpec struct {
	Model  string
	Effort string
}

// TierProfile maps each tier to what serves it.
type TierProfile map[Tier]TierSpec

// DefaultTierProfile is the shipped profile: Fable 5.1 advises, Opus 5.5 is the
// main agent and the writing sub-agents, Sonnet 5.5 the read-only ones, and
// Haiku 5.5 the wide fan-out. Every effort is unset.
//
// These ids are the defaults of the llm.tier.* settings, which is where an
// operator moves them; this is their one owner, so the setting's default and
// what a profile-less caller falls back to cannot disagree.
func DefaultTierProfile() TierProfile {
	return TierProfile{
		TierAdvisor: {Model: "claude-fable-5-1"},
		TierMain:    {Model: "claude-opus-5-5"},
		TierHeavy:   {Model: "claude-opus-5-5"},
		TierLite:    {Model: "claude-sonnet-5-5"},
		TierBasic:   {Model: "claude-haiku-5-5"},
	}
}
