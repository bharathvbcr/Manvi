package devcouncil

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// TestAdvLoadSkillSchemaTokenCost measures, with the repo's own estimator
// (agent.CountToolTokens -> prompt.EstimateTokens), what the always-registered
// devcouncil_load_skill costs every request, including the lean local profile
// (core tools only) and a repository with no skills.
func TestAdvLoadSkillSchemaTokenCost(t *testing.T) {
	f := newSkillFixture(t) // no skills anywhere under f.root
	pipe := tools.NewRegistry(bus.New())
	if err := f.reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	var one []llm.ToolSchema
	for _, s := range pipe.CoreSchemas() {
		if s.Name == "devcouncil_load_skill" {
			one = append(one, s)
		}
	}
	if len(one) != 1 {
		t.Fatalf("devcouncil_load_skill is not in the core schema set (%d)", len(one))
	}
	skill := agent.CountToolTokens(one)
	core := agent.CountToolTokens(pipe.CoreSchemas())
	all := agent.CountToolTokens(pipe.Schemas())
	const window = 32768 // local.DefaultContextWindow
	t.Logf("load_skill schema: %d tokens; core set (%d tools, lean local profile): %d tokens; all: %d tokens",
		skill, len(pipe.CoreSchemas()), core, all)
	t.Logf("load_skill = %.2f%% of the lean core tool set, %.3f%% of the %d-token default local window, %.2f%% of its %d-token system-prompt budget (window/8)",
		100*float64(skill)/float64(core), 100*float64(skill)/window, window, 100*float64(skill)/(window/8), window/8)
}
