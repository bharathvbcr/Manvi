package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
)

// criticRole is the built-in role the verification op judges with. It is named
// rather than hard-coded to a model so the router can place it on whichever
// tier it assigns that role; this file makes no placement decision.
const criticRole = "critic"

// criticBuildTimeout bounds resolving the provider and model for the critic.
const criticBuildTimeout = 30 * time.Second

// criticRuntime is a sub-agent runner attached to a provider and a tool
// registry, plus the role definition the critic runs under.
type criticRuntime struct {
	runner *subAgentRunner
	def    agents.Definition
}

// serveCritic is the serve.VerifyCritic `manvi serve` hands the verification
// op. It runs the critic through the same sub-agent runner every other child
// goes through, with ReadOnly forced: RunSubAgent removes every tool that is
// not read-only from the child's registry rather than asking the model to
// leave them alone, so a critic that tries to write finds no such tool.
type serveCritic struct {
	mu      sync.Mutex
	runtime *criticRuntime
	// build resolves the runtime on first use. A field so a test can supply a
	// scripted provider and registry without standing up a harness.
	build func(context.Context) (*criticRuntime, error)
}

func newServeCritic(reg *flags.Registry) *serveCritic {
	return &serveCritic{build: func(ctx context.Context) (*criticRuntime, error) { return buildCriticRuntime(ctx, reg) }}
}

// get builds the runtime once it succeeds. A failure is not cached: a provider
// that was unreachable a minute ago may not be now, and caching the error would
// make every later verification degraded until the host restarted.
func (c *serveCritic) get(ctx context.Context) (*criticRuntime, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runtime != nil {
		return c.runtime, nil
	}
	runtime, err := c.build(ctx)
	if err != nil {
		return nil, err
	}
	c.runtime = runtime
	return runtime, nil
}

// Judge runs one read-only critic turn.
func (c *serveCritic) Judge(ctx context.Context, prompt string, criteria int) (string, error) {
	runtime, err := c.get(ctx)
	if err != nil {
		return "", err
	}
	prompt = strings.TrimSpace(prompt)
	result, err := runtime.runner.RunSubAgent(ctx, devcouncil.SubAgentRequest{
		Label:    "verify-critic",
		Prompt:   prompt,
		ReadOnly: true,
		// The role's own placement, unparsed: the runner resolves it, and a
		// router that places roles later changes what this means, not this line.
		ModelSpec:    runtime.def.Model,
		SystemPrompt: strings.TrimRight(runtime.def.SystemPrompt, "\n") + "\n\n" + devcouncil.CriterionVerdictInstruction(criteria),
		Surface:      runtime.def.Surface(),
	})
	if err != nil {
		return "", err
	}
	return result.Summary, nil
}

// buildCriticRuntime assembles the provider, model and tool surface the critic
// runs on, using the same resolution `manvi run` uses.
func buildCriticRuntime(ctx context.Context, reg *flags.Registry) (*criticRuntime, error) {
	ctx, cancel := context.WithTimeout(ctx, criticBuildTimeout)
	defer cancel()
	def, ok := agents.NewRegistry().Get(criticRole)
	if !ok {
		return nil, fmt.Errorf("the built-in %q role is not registered", criticRole)
	}
	native, pipeline, err := nativeToolsWith(reg, nil)
	if err != nil {
		return nil, err
	}
	providerName, _, err := reg.String(flags.LLMDefaultProvider)
	if err != nil {
		return nil, err
	}
	provider, err := buildProvider(providerName, reg, credentials.NewResolver(), io.Discard, "")
	if err != nil {
		return nil, err
	}
	models, err := registerSessionLLM(pipeline, provider)
	if err != nil {
		return nil, err
	}
	model, _, err := resolveModelFor(ctx, providerName, provider, reg)
	if err != nil {
		return nil, err
	}
	effort, _, err := reg.String(flags.LLMEffort)
	if err != nil {
		return nil, err
	}
	effortCeiling, _, err := reg.String(flags.LLMEffortCeiling)
	if err != nil {
		return nil, err
	}
	runner, _ := harnessFor(pipeline)
	if runner == nil {
		return nil, errors.New("the harness built no sub-agent runner")
	}
	runner.attach(subAgentConfig{
		provider: provider, models: models, model: model,
		effort: effort, effortCeiling: effortCeiling,
		registry: pipeline, native: native, meter: &subAgentMeter{},
		coreToolsOnly:   coreToolsOnly(reg, provider.Name()),
		assertInvariant: assertInvariant(reg),
	})
	return &criticRuntime{runner: runner, def: def}, nil
}
