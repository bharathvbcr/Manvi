package agents

import (
	"fmt"
	"strings"
)

// InheritSpec is the Model value meaning "run this child wherever the parent
// is running". It is the default Register applies to a definition that does
// not name a model.
const InheritSpec = "inherit"

// Placement is where one sub-agent's turn is to run: which provider serves it
// and which model that provider is asked for.
//
// Both fields empty means inherit the parent's. That is the only encoding of
// "inherit" the runner sees, so a runner never has to know that the string
// "inherit" is special — by the time it holds a Placement, the question has
// already been answered.
type Placement struct {
	// Provider names a registered provider, or is empty for the parent's.
	Provider string
	// Model names the model to ask that provider for, or is empty for the
	// parent's. Empty with a non-empty Provider means "that provider's default
	// model", which the runner resolves; the two are not interchangeable.
	Model string
}

// Inherits reports whether the placement asks for the parent's provider and
// model, in which case the runner has nothing to switch.
func (p Placement) Inherits() bool { return p.Provider == "" && p.Model == "" }

// ParsePlacement reads a Definition.Model spec — or a per-call override — into
// the provider and model it names.
//
//	"", "inherit"      the parent's provider and model
//	"provider/model"   that provider, asked for that model
//	"provider"         that provider, at its own default model
//	"model"            the parent's provider, asked for that model
//
// The last two forms are the same syntax, so the bare word is ambiguous on its
// face: "local" is a provider here and "flash" is a model, and nothing about
// the strings says which. isProvider settles it, and it is a callback rather
// than a list in this package because the set of registered providers is a
// property of the run, not of the role catalogue. A definition written against
// a provider that this invocation did not enable must resolve to a model name
// on the parent — not to a provider that is not there — and only the caller
// holding the registry can tell the difference.
//
// A nil isProvider reads every bare word as a model. That is the safe reading:
// it keeps the child on a provider that is known to exist, where naming a
// provider that was never registered would fail the turn.
func ParsePlacement(spec string, isProvider func(string) bool) Placement {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, InheritSpec) {
		return Placement{}
	}

	if provider, model, ok := strings.Cut(spec, "/"); ok {
		provider = strings.TrimSpace(provider)
		model = strings.TrimSpace(model)
		// A spec that is all separator — "/", "/model", "provider/" — has a
		// half that says nothing. The empty half is dropped rather than
		// carried as an empty string, so "/gpt" is the model "gpt" on the
		// parent's provider and not a request for a provider with no name.
		return Placement{Provider: provider, Model: model}
	}

	if isProvider != nil && isProvider(spec) {
		return Placement{Provider: spec}
	}
	return Placement{Model: spec}
}

// ModelPlacement reads a placement a dispatching model wrote — a per-call
// `model`, or the model or tier of a role it is defining — and accepts only a
// tier at or below ceiling.
//
// It exists because ParsePlacement answers a different question. That parser
// serves the operator, who may put a role on any model they have enabled; a
// model given the same parser could put a child on whatever id it knew,
// including one billed at several times the rate of the tier the role was
// meant for, and nothing would have said no. So a model names tiers, never ids,
// and the tiers it may name stop at the role's ceiling.
//
// Empty and "inherit" return the empty tier and no error: the model asked for
// no placement of its own, and the role's routing stands.
func ModelPlacement(spec string, ceiling Tier) (Tier, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, InheritSpec) {
		return "", nil
	}
	allowed := TiersAtOrBelow(ceiling)
	tier, ok := ParseTier(spec)
	if !ok {
		return "", fmt.Errorf(
			"%q is not a tier: a sub-agent's placement may name only a tier, never a model id or provider; "+
				"allowed here: %s", spec, joinTiers(allowed))
	}
	if tier.Rank() > ceiling.Rank() {
		return "", fmt.Errorf(
			"tier %q is above this role's ceiling (%s); allowed here: %s",
			tier, ceiling, joinTiers(allowed))
	}
	return tier, nil
}

// TiersAtOrBelow lists the tiers a placement capped at ceiling may name,
// cheapest first. An unknown ceiling allows none.
func TiersAtOrBelow(ceiling Tier) []Tier {
	var out []Tier
	for _, t := range tiers {
		if t.Rank() <= ceiling.Rank() {
			out = append(out, t)
		}
	}
	return out
}

func joinTiers(ts []Tier) string {
	if len(ts) == 0 {
		return "none"
	}
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = string(t)
	}
	return strings.Join(names, ", ")
}
