package agents

// BasicFanoutWidth is the narrowest fan-out whose read-only children route to
// the basic tier.
//
// A dispatch that splits a question four or more ways has already made each
// share narrow — one file, one symbol, one search — and that is the work the
// cheapest tier is for. Below it, each read-only child carries a real share of
// the question and gets the lite tier.
const BasicFanoutWidth = 4

// Route decides the tier a child runs on from facts known at dispatch time.
//
// It never reads the prompt. agent/effort.go rejected classifying a prompt up
// front because that judgement is made on the least information the harness
// will ever have; the same applies to choosing a model. What is known before a
// token is generated is the role, whether the child may write, whether it is
// being asked for a verdict, and how wide the fan-out is — and those decide:
//
//   - a verdict request goes to advisor;
//   - a child that may write goes to heavy;
//   - a read-only child in a fan-out of at least BasicFanoutWidth goes to basic;
//   - any other read-only child goes to lite.
//
// readOnly is whether the caller asked for a non-mutating child; a role that
// does not enable writes is read-only whatever the caller asked. The answer is
// then capped at the role's ceiling, so a role that states a cheaper tier is
// never routed above it.
//
// No child routes to main. Main is the tier the dispatching agent itself runs
// on, and a child placed there would be a second copy of the parent rather than
// a delegate.
func Route(role Definition, readOnly, verdict bool, width int) Tier {
	readOnly = readOnly || !role.EnableWriteTools
	var routed Tier
	switch {
	case verdict:
		routed = TierAdvisor
	case !readOnly:
		routed = TierHeavy
	case width >= BasicFanoutWidth:
		routed = TierBasic
	default:
		routed = TierLite
	}
	if ceiling := role.Ceiling(); routed.Rank() > ceiling.Rank() {
		return ceiling
	}
	return routed
}

// Ceiling is the dearest tier this role may run on.
//
// A role that states its tier is capped there. One that does not — an untyped
// fan-out, an unregistered name, a role authored without one — is capped by
// what it may do: heavy for a role that may write, lite for one that may not.
// It is derived rather than defaulted to the top because a ceiling nobody wrote
// down is one nobody reviewed, and the direction an unreviewed spend limit has
// to fail is low.
func (d Definition) Ceiling() Tier {
	if t, ok := ParseTier(string(d.Tier)); ok {
		return t
	}
	return d.PermissionCeiling()
}

// PermissionCeiling is the ceiling this role's permissions alone allow,
// ignoring any tier it states. It is the most a role authored by a model may
// claim: see devcouncil_define_subagent.
func (d Definition) PermissionCeiling() Tier {
	if d.EnableWriteTools {
		return TierHeavy
	}
	return TierLite
}
