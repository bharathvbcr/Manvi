package main

import (
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
)

// A tier profile the session could not run is refused when the host builds the
// critic, with the error `manvi run` gives, not on the first verification.
func TestServeCriticRefusesABadTierProfileAtStartup(t *testing.T) {
	t.Setenv(flags.EnvKey(flags.LLMDefaultProvider), "anthropic")
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierAdvisor)), "claude-fable-9")
	critic, err := newServeCritic(tierRegistry(t))
	requireTierRefusal(t, err, agents.TierAdvisor, "claude-fable-9", "catalogue")
	if critic != nil {
		t.Fatal("a refused profile still produced a critic")
	}
}

func TestServeCriticAcceptsTheShippedTierProfileAtStartup(t *testing.T) {
	t.Setenv(flags.EnvKey(flags.LLMDefaultProvider), "anthropic")
	critic, err := newServeCritic(tierRegistry(t))
	if err != nil || critic == nil {
		t.Fatalf("newServeCritic = %v, %v", critic, err)
	}
}
