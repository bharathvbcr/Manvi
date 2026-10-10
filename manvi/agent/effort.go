package agent

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// This file answers one question for the loop: is it worth thinking harder
// about this, and how much harder is it allowed to think?
//
// The measurement behind it, taken on this harness against Qwen3.8-27B:
//
//   - "fix the binary-search bug" at effort low: passed, 238s, 2,860 generated
//     tokens. The same task with reasoning off: failed. The model applied
//     `hi = mid - 1` under a `lo < hi` loop and then spent 41 round trips and
//     674s defending it, ending on the step ceiling with the bug still there.
//   - "replace a literal with a constant at 90 sites": passed both with and
//     without reasoning, and reasoning cost 2.6x the tokens (8,295 against
//     2,732) and 1.8x the wall clock to reach the identical result.
//
// So the tier is worth its price on one of those and is pure overhead on the
// other, and a single static llm.effort cannot express that.
//
// The tempting fix is to read the prompt and guess which kind of task it is.
// That is a classifier this harness would have to be right about before the
// first token is generated, on the least information it will ever have, and
// being wrong in the cheap direction reproduces exactly the failure above. The
// loop already holds a much better signal, and holds it *after* the evidence
// exists rather than before: it knows when a turn has stopped getting anywhere,
// because it is already refusing calls on that basis — the verbatim repeat
// ledger in agent/loop.go and the progress tracker in agent/progress.go.
//
// So effort escalates on evidence. A turn that keeps getting somewhere never
// trips either refusal and stays at the tier it was asked for, which is what
// keeps mechanical work cheap without anyone having to classify it. A turn that
// is stuck buys more thinking at the point where the cheap tier has been
// demonstrated — not assumed — to be insufficient.
//
// The limit worth naming: a turn can be stuck without either refusal firing.
// agent/progress.go credits a mutating call that did not error as progress, so
// a model rewriting one file forty different ways is never interrupted and
// never escalated here either. That is the same gap, not a new one, and closing
// it would mean judging whether edits are converging — the judgement the loop
// has already declined to make.

// EffortPlan is the ladder one turn may climb, and how far.
//
// The rungs are the model's own EffortLevels, in the order the adapter declares
// them, which llm.Capability documents as least reasoning first. Using the
// model's list rather than a vocabulary of this package's own is what keeps
// this provider-independent: Anthropic's ladder runs to xhigh and max, Gemini's
// starts at low, xAI's and the local adapter's start at none, and none of that
// has to be known here.
//
// The zero value never escalates, which is what every caller that has not
// configured a ceiling gets.
type EffortPlan struct {
	levels  []string
	base    string
	ceiling string
	// ceilingAt is the index of ceiling in levels, valid only when ceiling is
	// non-empty.
	ceilingAt int
}

// PlanEffort builds the ladder for a turn, refusing a ceiling that cannot be
// climbed to rather than accepting one that would silently never fire.
//
// An empty ceiling is always serviceable and yields a plan that never
// escalates: not configuring the feature must not turn a working configuration
// into an error, whatever the model can or cannot do.
func PlanEffort(capability llm.Capability, base, ceiling string) (EffortPlan, error) {
	if ceiling == "" {
		return EffortPlan{base: base}, nil
	}
	if base == "" {
		return EffortPlan{}, fmt.Errorf(
			"an effort ceiling of %q needs a tier the turn starts at: set the base effort as well, "+
				"since an unset effort omits the field entirely and there is no rung to climb from",
			ceiling)
	}
	if !capability.SupportsReasoning || len(capability.EffortLevels) == 0 {
		return EffortPlan{}, fmt.Errorf(
			"model %s/%s does not support reasoning effort, so an effort ceiling of %q can never apply",
			capability.Provider, capability.Model, ceiling)
	}
	levels := append([]string(nil), capability.EffortLevels...)
	baseAt := slices.Index(levels, base)
	if baseAt < 0 {
		return EffortPlan{}, fmt.Errorf("model %s/%s supports effort %v, not %q",
			capability.Provider, capability.Model, levels, base)
	}
	ceilingAt := slices.Index(levels, ceiling)
	if ceilingAt < 0 {
		return EffortPlan{}, fmt.Errorf("model %s/%s supports effort %v, not %q",
			capability.Provider, capability.Model, levels, ceiling)
	}
	if ceilingAt <= baseAt {
		// Equal is refused as well as lower. A ceiling equal to the base is a
		// setting that does nothing, and silently doing nothing is how an
		// operator concludes the feature is broken; if they meant to run at
		// that tier throughout, the base is where to say so.
		return EffortPlan{}, fmt.Errorf(
			"effort ceiling %q is not above the tier this turn starts at (%q) on %s/%s, whose ladder "+
				"is %s: raise the ceiling, or set the base effort to %q if every request should carry it",
			ceiling, base, capability.Provider, capability.Model, strings.Join(levels, " < "), ceiling)
	}
	return EffortPlan{levels: levels, base: base, ceiling: ceiling, ceilingAt: ceilingAt}, nil
}

// PlanTierEffort builds the ladder for a turn on a tier's model: the tier's own
// effort as the base, and the session's ceiling only where this model can climb
// to it.
//
// It differs from PlanEffort in the two places a tier differs from the session.
// The base is checked even with no ceiling, because a tier's effort is a
// setting in its own right and one its model refuses would otherwise surface as
// a refused request mid-way through a delegated turn. And the ceiling is
// inherited rather than demanded: it was checked against the main model, so on
// a model that does not reason, or a tier that sets no effort, or one already
// at or above the ceiling, the tier runs at its own effort throughout instead of
// being refused for a ceiling nobody set on it. A ceiling the tier's model has
// no rung for at all is still refused — that is the session's setting naming a
// tier this model cannot reach, not a ceiling this tier sits above.
func PlanTierEffort(capability llm.Capability, base, ceiling string) (EffortPlan, error) {
	if base != "" {
		if err := capability.Validate(llm.Request{Model: capability.Model, Effort: base}); err != nil {
			return EffortPlan{}, err
		}
	}
	if ceiling == "" || base == "" || !capability.SupportsReasoning || len(capability.EffortLevels) == 0 {
		return PlanEffort(capability, base, "")
	}
	ceilingAt := slices.Index(capability.EffortLevels, ceiling)
	if ceilingAt < 0 {
		return EffortPlan{}, fmt.Errorf("model %s/%s supports effort %v, not the ceiling %q",
			capability.Provider, capability.Model, capability.EffortLevels, ceiling)
	}
	if ceilingAt <= slices.Index(capability.EffortLevels, base) {
		return PlanEffort(capability, base, "")
	}
	return PlanEffort(capability, base, ceiling)
}

// planEffort resolves the ladder for a loop's configuration.
//
// A ceiling that cannot be checked is refused rather than kept as a setting
// that quietly never fires: an operator who has asked for escalation and is not
// getting it has no way to tell that from a turn that never needed it. The
// capability is only consulted when a ceiling was actually asked for, so a
// provider that cannot describe its model still runs exactly as it did before.
func planEffort(cfg Config) (EffortPlan, error) {
	if cfg.EffortCeiling == "" {
		return PlanEffort(llm.Capability{}, cfg.Effort, "")
	}
	capability, ok := cfg.Provider.Capability(cfg.Model)
	if !ok {
		return EffortPlan{}, fmt.Errorf(
			"agent: %s cannot describe model %q, so the effort ceiling %q cannot be checked against "+
				"the tiers it accepts — this is not a negative result, the check did not run",
			cfg.Provider.Name(), cfg.Model, cfg.EffortCeiling)
	}
	plan, err := PlanEffort(capability, cfg.Effort, cfg.EffortCeiling)
	if err != nil {
		return EffortPlan{}, fmt.Errorf("agent: %w", err)
	}
	return plan, nil
}

// Escalates reports whether this plan can ever raise the tier.
func (p EffortPlan) Escalates() bool { return p.ceiling != "" }

// Next returns the rung above current, and false when there is none — either
// because the plan does not escalate, or because current is already at the
// ceiling.
//
// A current tier that is not on the ladder returns false rather than guessing a
// position for it. It cannot arise from a plan this package built, and picking
// a rung for an unrecognised value would be inventing the one answer the
// capability check exists to prevent.
func (p EffortPlan) Next(current string) (string, bool) {
	if !p.Escalates() {
		return "", false
	}
	at := slices.Index(p.levels, current)
	if at < 0 || at >= p.ceilingAt {
		return "", false
	}
	return p.levels[at+1], true
}
