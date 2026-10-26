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
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
)

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

// newServeCritic refuses a tier profile the session could not run before the
// host starts, the same check `manvi run` makes before it contacts anyone. The
// runtime itself is built lazily, on the first verification, because building
// it resolves credentials and reaches the provider.
func newServeCritic(reg *flags.Registry) (*serveCritic, error) {
	if err := checkDefaultTiers(reg); err != nil {
		return nil, err
	}
	return &serveCritic{build: func(ctx context.Context) (*criticRuntime, error) { return buildCriticRuntime(ctx, reg) }}, nil
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
	req := devcouncil.SubAgentRequest{Label: "verify-critic", Prompt: strings.TrimSpace(prompt)}
	// The role's own placement, prompt and surface, read-only whatever the role
	// says: the same policy the checkpoint critic runs under.
	devcouncil.ApplyRole(&req, runtime.def, true)
	req.SystemPrompt = strings.TrimRight(runtime.def.SystemPrompt, "\n") + "\n\n" + devcouncil.CriterionVerdictInstruction(criteria)
	// A verdict goes to the advisor tier. Without a tier the child inherits the
	// session's model, which is the tier profile bypassed.
	req.Tier = agents.Route(runtime.def, true, true, 1)
	result, err := runtime.runner.RunSubAgent(ctx, req)
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
	def, ok := agents.NewRegistry().Get(criticRoleName)
	if !ok {
		return nil, fmt.Errorf("the built-in %q role is not registered", criticRoleName)
	}
	native, pipeline, err := nativeToolsWith(reg, nil) //nolint:contextcheck // buildMCP's only context goes to mcp.Manager.Discover, which never reads it
	if err != nil {
		return nil, err
	}
	// The provider, tier profile, model and effort checks are buildRunEnv's, the
	// one owner `manvi run` and `manvi reflect` build through, so a bad profile
	// is refused here with the same error and the critic is placed on the
	// advisor tier rather than the session's model.
	env, err := buildRunEnv(ctx, io.Discard, reg, pipeline)
	if err != nil {
		return nil, err
	}
	runner, _ := harnessFor(pipeline)
	if runner == nil {
		return nil, errors.New("the harness built no sub-agent runner")
	}
	runner.attach(env.subAgentConfig(reg, pipeline, native, &subAgentMeter{}, coreToolsOnly(reg, env.provider.Name())))
	return &criticRuntime{runner: runner, def: def}, nil
}
