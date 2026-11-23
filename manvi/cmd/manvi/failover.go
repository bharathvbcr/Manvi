package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// A child whose provider is unavailable re-runs once on its tier's fallback.
//
// Escalation answers "this model was not enough" by climbing to a dearer one
// on the same ladder. Failover answers a different question — "this provider
// is not answering" — and climbing the ladder is the wrong answer to it: the
// next tier is usually on the same provider, rate limited by the same account.
// The fallback is a placement the operator wrote for exactly this case, and it
// must name another provider.
//
// It runs only on the evidence the transport classifies as a service
// condition: a failure Retryable() says another attempt could clear (rate
// limited, overloaded, 5xx, a dropped connection), surfacing after the
// transport's own retries were spent. A refused credential, a bad request, an
// exhausted spend cap and the caller's own cancellation are not provider
// outages, and moving them to another provider would hide them.
//
// It is bounded the way escalation is: once per child, never chained — the
// re-run is placed as an operator placement, which neither escalates nor fails
// over again.

// providerUnavailable reports whether err is a provider service condition the
// transport could not outlast.
func providerUnavailable(err error) (*transport.Error, bool) {
	var terr *transport.Error
	if err == nil || !errors.As(err, &terr) || !terr.Retryable() {
		return nil, false
	}
	return terr, true
}

// failoverFor decides whether a failed child re-runs on its tier's fallback,
// and on which spec.
func (r *subAgentRunner) failoverFor(ctx context.Context, cfg *subAgentConfig, req devcouncil.SubAgentRequest,
	err error) (agents.TierSpec, bool) {
	if _, down := providerUnavailable(err); !down || ctx.Err() != nil || req.Tier == "" {
		return agents.TierSpec{}, false
	}
	if !agents.ParsePlacement(req.ModelSpec, nil).Inherits() {
		// An operator pinned the role to a model; a tier fallback does not
		// overrule that pin, as a tier escalation does not.
		return agents.TierSpec{}, false
	}
	spec, ok := cfg.fallbacks[req.Tier]
	if !ok || spec.Model == "" {
		return agents.TierSpec{}, false
	}
	placed, perr := r.place(cfg, req)
	if perr == nil && placed.provider != nil &&
		spec.Same(agents.TierSpec{Provider: placed.provider.Name(), Model: placed.model}) {
		return agents.TierSpec{}, false
	}
	if perr == nil && placed.provider != nil && placed.provider.Name() == spec.Provider {
		// The same provider that just failed: a different model on one
		// account's exhausted rate limit is not a way out of it.
		return agents.TierSpec{}, false
	}
	return spec, true
}

// failover re-runs the child once on spec and reports both attempts.
func (r *subAgentRunner) failover(ctx context.Context, cfg *subAgentConfig, req devcouncil.SubAgentRequest,
	first attemptResult, firstErr error, spec agents.TierSpec) (devcouncil.SubAgentResult, error) {
	what := "its provider was unavailable"
	if terr, ok := providerUnavailable(firstErr); ok {
		what = fmt.Sprintf("%s was unavailable (http %d after %d attempt(s))", terr.Provider, terr.Status, terr.Attempts)
	}
	rerun := req
	rerun.Member = ""
	rerun.ModelSpec = spec.Placement()
	second, err := r.attempt(ctx, cfg, rerun)
	// Merged as an escalation's attempts are: both attempts' usage, and every
	// path either one wrote, so a writer whose provider died part-way still
	// hands the parent's checkpoint what it changed before it died.
	merged := mergeAttempts(first.result, second.result)
	if err != nil {
		// Only the fallback's error is wrapped. The first failure is reported as
		// text: it was a retryable outage, and wrapping it too would let
		// errors.As find a retryable transport error on a result whose actual
		// cause — the fallback's — may be permanent.
		return merged, fmt.Errorf("sub-agent %q: %s (%s), and its tier %s fallback %s failed too: %w",
			req.Label, what, firstErr.Error(), req.Tier, spec.Placement(), err)
	}
	merged.Summary += fmt.Sprintf("\n\n[this sub-agent: %s; re-ran once on the %s tier's fallback %s]",
		what, req.Tier, spec.Placement())
	return merged, nil
}
