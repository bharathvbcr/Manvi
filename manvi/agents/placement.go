package agents

import "strings"

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

// String renders the placement the way ParsePlacement would read it back, for
// run reports and log lines.
func (p Placement) String() string {
	switch {
	case p.Inherits():
		return InheritSpec
	case p.Provider == "":
		return p.Model
	case p.Model == "":
		return p.Provider
	default:
		return p.Provider + "/" + p.Model
	}
}

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
