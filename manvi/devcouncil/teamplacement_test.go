package devcouncil

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
)

// teamRunner is a recordingRunner that knows the operator's team, as the
// harness runner does once it is attached.
type teamRunner struct {
	recordingRunner
	roster agents.Roster
	tiers  map[agents.Tier]string
}

func (r *teamRunner) TeamRoster() agents.Roster          { return r.roster }
func (r *teamRunner) TierPlacement(t agents.Tier) string { return r.tiers[t] }

func newTeamRunner(t *testing.T, roster string) *teamRunner {
	t.Helper()
	team, err := agents.ParseRoster(roster)
	if err != nil {
		t.Fatal(err)
	}
	return &teamRunner{roster: team}
}

// A dispatching model may name a member of the operator's team for a child,
// and the child carries the member and the member's tier to the runner. Before
// there was a team, every name that was not a tier was refused.
func TestInvokeSubagentPlacesAChildOnANamedTeamMember(t *testing.T) {
	runner := newTeamRunner(t, "scout=basic:gemini/gemini-4-hydrogen")
	f, _ := fixtureWithRoles(t, runner)

	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "research", "role": "surveyor", "prompt": "survey it", "model": "scout"},
		},
	})
	if res.IsError {
		t.Fatalf("a team member within the role's ceiling was refused: %s", res.Text)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.seen) != 1 {
		t.Fatalf("the runner was asked for %d children, want 1", len(runner.seen))
	}
	if got := runner.seen[0]; got.Member != "scout" || got.Tier != agents.TierBasic {
		t.Fatalf("child = member %q tier %q, want scout on basic", got.Member, got.Tier)
	}
}

// A member is held to the role's ceiling by its own tier: an advisor-tier
// member cannot be put on a read-only research role, whatever it is called.
func TestInvokeSubagentRefusesAMemberAboveTheRolesCeiling(t *testing.T) {
	runner := newTeamRunner(t, "fable=advisor:anthropic/claude-fable-5-1,scout=basic:gemini/gemini-4-hydrogen")
	f, _ := fixtureWithRoles(t, runner)

	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "research", "role": "r", "prompt": "survey it", "model": "fable"},
		},
	})
	if !res.IsError || !strings.Contains(res.Text, "ceiling") || !strings.Contains(res.Text, "scout") {
		t.Fatalf("an advisor member on research = %s; want a refusal naming the members allowed", res.Text)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.seen) != 0 {
		t.Fatalf("%d children ran from a refused dispatch", len(runner.seen))
	}
}

// Where a member or a tier puts a child is where the fan-out width is judged:
// a team member on a local model narrows the dispatch as a role pinned there
// does.
func TestATeamPlacementReachesTheFanoutWidth(t *testing.T) {
	runner := newTeamRunner(t, "gpu=basic:local/qwen")
	runner.tiers = map[agents.Tier]string{agents.TierLite: "gemini/gemini-4-hydrogen"}
	reqs := []SubAgentRequest{
		{Member: "gpu", Tier: agents.TierBasic},
		{Tier: agents.TierLite},
		{Tier: agents.TierHeavy},
		{Member: "gpu", ModelSpec: "anthropic/claude-opus-5-5"},
	}
	got := teamPlacements(runner, reqs)
	want := []string{"local/qwen", "gemini/gemini-4-hydrogen"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("placements = %v, want %v", got, want)
	}
	if teamPlacements(&recordingRunner{}, reqs) != nil {
		t.Fatal("a runner with no team implies no placements")
	}
}
