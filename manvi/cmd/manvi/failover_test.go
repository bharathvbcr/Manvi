package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/gemini"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// downProvider serves its models and fails every request with err.
type downProvider struct {
	tierProvider
	err error
}

func (p *downProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	p.seen = append(p.seen, req)
	p.mu.Unlock()
	return nil, p.err
}

func failoverRunner(t *testing.T, failure error, fallback agents.TierSpec) (*subAgentRunner, *downProvider, *tierProvider) {
	t.Helper()
	claude := &downProvider{
		tierProvider: tierProvider{name: anthropic.Name, reasons: map[string][]string{"parent-model": nil, "c-scout": nil}},
		err:          failure,
	}
	gem := &tierProvider{name: gemini.Name, reasons: map[string][]string{"g-scout": nil}}
	models := llm.NewRegistry()
	for _, p := range []llm.Provider{claude, gem} {
		if err := models.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: claude, models: models, model: "parent-model",
		tiers:     agents.TierProfile{agents.TierBasic: {Model: "c-scout"}},
		fallbacks: map[agents.Tier]agents.TierSpec{agents.TierBasic: fallback},
		registry:  surfaceRegistry(t),
	})
	return r, claude, gem
}

// A child whose provider is rate limited past the transport's retries re-runs
// once on its tier's fallback, on the other provider, and says so.
func TestAChildFailsOverWhenItsProviderIsUnavailable(t *testing.T) {
	limited := &transport.Error{Provider: anthropic.Name, Status: 429, Attempts: 4, Body: "rate_limit_error"}
	r, claude, gem := failoverRunner(t, limited, agents.TierSpec{Provider: gemini.Name, Model: "g-scout"})

	res, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "scout", Prompt: "look", ReadOnly: true, Tier: agents.TierBasic,
	})
	if err != nil {
		t.Fatalf("the child did not fail over: %v", err)
	}
	if len(claude.requests()) == 0 {
		t.Fatal("the first attempt never reached the tier's own provider")
	}
	seen := gem.requests()
	if len(seen) == 0 || seen[0].Model != "g-scout" {
		t.Fatalf("fallback requests = %+v, want g-scout on gemini", seen)
	}
	if !strings.Contains(res.Summary, "fallback gemini/g-scout") || !strings.Contains(res.Summary, "http 429") {
		t.Fatalf("the failover is not reported on the result: %q", res.Summary)
	}
}

// A refused request is not an outage: moving it to another provider would
// hide a fault the operator has to fix.
func TestAPermanentFailureDoesNotFailOver(t *testing.T) {
	bad := &transport.Error{Provider: anthropic.Name, Status: 400, Attempts: 1, Body: "invalid_request_error"}
	r, _, gem := failoverRunner(t, bad, agents.TierSpec{Provider: gemini.Name, Model: "g-scout"})

	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "scout", Prompt: "look", ReadOnly: true, Tier: agents.TierBasic,
	}); err == nil {
		t.Fatal("a 400 was answered by the fallback")
	}
	if n := len(gem.requests()); n != 0 {
		t.Fatalf("the fallback served %d request(s) for a permanent failure", n)
	}
}

// When the fallback fails too, the error the caller gets is the fallback's:
// a permanent failure there must not read as the first provider's retryable
// outage, so nothing upstream mistakes it for a condition another attempt clears.
func TestAFailedFallbackReportsItsOwnCause(t *testing.T) {
	limited := &transport.Error{Provider: anthropic.Name, Status: 429, Attempts: 4, Body: "rate_limit_error"}
	refused := &transport.Error{Provider: gemini.Name, Status: 400, Attempts: 1, Body: "invalid_request_error"}
	claude := &downProvider{
		tierProvider: tierProvider{name: anthropic.Name, reasons: map[string][]string{"parent-model": nil, "c-scout": nil}},
		err:          limited,
	}
	gem := &downProvider{tierProvider: tierProvider{name: gemini.Name, reasons: map[string][]string{"g-scout": nil}}, err: refused}
	models := llm.NewRegistry()
	for _, p := range []llm.Provider{claude, gem} {
		if err := models.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: claude, models: models, model: "parent-model",
		tiers:     agents.TierProfile{agents.TierBasic: {Model: "c-scout"}},
		fallbacks: map[agents.Tier]agents.TierSpec{agents.TierBasic: {Provider: gemini.Name, Model: "g-scout"}},
		registry:  surfaceRegistry(t),
	})
	_, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "scout", Prompt: "look", ReadOnly: true, Tier: agents.TierBasic,
	})
	if err == nil {
		t.Fatal("both providers failed and the child reported success")
	}
	if len(gem.requests()) == 0 {
		t.Fatal("the fallback was never tried")
	}
	var terr *transport.Error
	if !errors.As(err, &terr) || terr.Status != 400 || terr.Provider != gemini.Name {
		t.Fatalf("the wrapped cause = %+v; want the fallback's 400", terr)
	}
	if _, down := providerUnavailable(err); down {
		t.Fatalf("a permanent fallback failure reads as a retryable outage: %v", err)
	}
	if !strings.Contains(err.Error(), "http 429") {
		t.Fatalf("the first failure is missing from the message: %v", err)
	}
}

func TestAFallbackOnTheFailedProviderIsNotTried(t *testing.T) {
	limited := &transport.Error{Provider: anthropic.Name, Status: 529, Attempts: 4}
	r, claude, _ := failoverRunner(t, limited, agents.TierSpec{Provider: anthropic.Name, Model: "parent-model"})

	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "scout", Prompt: "look", ReadOnly: true, Tier: agents.TierBasic,
	}); err == nil {
		t.Fatal("the child reported success with its only provider down")
	}
	for _, req := range claude.requests() {
		if req.Model == "parent-model" {
			t.Fatal("the child re-ran on the provider that had just failed")
		}
	}
}

func TestATierFallbackMustNameItsProvider(t *testing.T) {
	t.Setenv(flags.EnvKey(tierFallbackKey(agents.TierBasic)), "gemini-4-hydrogen")
	if _, err := resolveTiers(tierRegistry(t), anthropic.Name); err == nil || !strings.Contains(err.Error(), "provider/model") {
		t.Fatalf("a bare fallback = %v; want it refused", err)
	}
	t.Setenv(flags.EnvKey(tierFallbackKey(agents.TierBasic)), "gemini/gemini-4-hydrogen")
	table, err := checkTiers(tierRegistry(t), anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	if fb := table.Fallbacks()[agents.TierBasic]; fb.Provider != gemini.Name || fb.Model != "gemini-4-hydrogen" {
		t.Fatalf("fallback = %+v", fb)
	}
	if got := table.extraProviders(); strings.Join(got, ",") != gemini.Name {
		t.Fatalf("a fallback's provider must be built: %v", got)
	}
}
