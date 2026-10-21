package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// runEnv is what a command that drives a model needs and that does not depend on
// what it asks the model: the provider, the model registry, where the session's
// model and effort resolve to, the tier profile, and the scrubber that guards
// what comes back.
//
// It is the one owner of that setup. `manvi run` and `manvi reflect` both build
// it here, so a check added to it — the effort validation was the last — reaches
// both, instead of one of them quietly lacking it.
type runEnv struct {
	providerName  string
	provider      llm.Provider
	tiers         tierTable
	models        *llm.Registry
	model         string
	effort        string
	effortCeiling string
	scrubber      *credentials.Scrubber
}

// subAgentConfig is what a sub-agent runner is attached with for this run: the
// provider, models and tool surface every child shares. The caller adds what
// only it knows — the system prompt a child inherits and the sink its evidence
// is forwarded to.
func (e runEnv) subAgentConfig(reg *flags.Registry, pipeline *tools.Registry, native *devcouncil.Registry,
	meter *subAgentMeter, coreOnly bool) subAgentConfig {
	return subAgentConfig{
		provider:        e.provider,
		models:          e.models,
		model:           e.model,
		effort:          e.effort,
		effortCeiling:   e.effortCeiling,
		tiers:           e.tiers.Profile(),
		registry:        pipeline,
		native:          native,
		meter:           meter,
		coreToolsOnly:   coreOnly,
		assertInvariant: assertInvariant(reg),
	}
}

// buildRunEnv resolves a run's provider and model from the flag registry and
// registers the provider on pipeline. It validates what can be validated before
// any model is contacted: a tier the provider cannot serve, price or run at its
// effort, and an effort or effort ceiling the model does not accept, are
// configuration mistakes, and found here they name the setting instead of
// arriving as a refused request mid-way through the work.
//
// ctx bounds the provider build, which can probe a local server for its address
// and wait on the spend ledger's lock.
func buildRunEnv(ctx context.Context, notes io.Writer, reg *flags.Registry, pipeline *tools.Registry) (runEnv, error) {
	providerName, _, err := reg.String(flags.LLMDefaultProvider)
	if err != nil {
		return runEnv{}, err
	}
	resolver := credentials.NewResolver()
	provider, err := buildProvider(ctx, providerName, reg, resolver, notes, "")
	if err != nil {
		return runEnv{}, err
	}
	tiers, err := checkTiers(reg, providerName)
	if err != nil {
		return runEnv{}, err
	}
	if set := tiers.inactiveOverrides(); len(set) > 0 {
		if _, err := fmt.Fprintf(notes, "manvi: %s set, but the tier profile applies on %s only and this run is on %s\n",
			strings.Join(set, ", "), anthropic.Name, providerName); err != nil {
			return runEnv{}, err
		}
	}
	models, err := registerSessionLLM(pipeline, provider)
	if err != nil {
		return runEnv{}, err
	}

	model, _, err := resolveModelFor(ctx, providerName, provider, reg)
	if err != nil {
		return runEnv{}, err
	}
	effort, _, _, err := mainEffort(reg, providerName)
	if err != nil {
		return runEnv{}, err
	}
	effortCeiling, _, err := reg.String(flags.LLMEffortCeiling)
	if err != nil {
		return runEnv{}, err
	}
	if effort != "" || effortCeiling != "" {
		// Checked here rather than at request time for the reason the TUI checks
		// it at attach: an effort the model does not accept is a configuration
		// mistake, and found now it names the setting instead of arriving as a
		// refused request after the run has started.
		if capability, ok := provider.Capability(model); ok {
			if verr := capability.Validate(llm.Request{Model: model, Effort: effort}); verr != nil {
				return runEnv{}, verr
			}
			// The same plan the loop builds, from the same function, so a
			// ceiling accepted here cannot be one the turn then refuses.
			if _, verr := agent.PlanEffort(capability, effort, effortCeiling); verr != nil {
				return runEnv{}, fmt.Errorf("%s=%s: %w", flags.EnvKey(flags.LLMEffortCeiling), effortCeiling, verr)
			}
		}
	}

	return runEnv{
		providerName: providerName, provider: provider, tiers: tiers, models: models,
		model: model, effort: effort, effortCeiling: effortCeiling,
		scrubber: newArmedScrubber(resolver),
	}, nil
}
