package serve

import (
	"encoding/json"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// promptInputs are the files an agent must not write (see
// devcouncil/zsec_prompt_inputs_test.go) and a host's own writer must still be
// able to: the managed plane materialises instructions through the internal
// path, and that is the only door the protection leaves open.
var promptInputs = []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md", "mcp.json", ".mcp.json", ".agents/skills/x/SKILL.md"}

// internalFileCheck is fileCheck with the Internal flag a host's own writer sets.
func internalFileCheck(t *testing.T, id, root, path string, internal bool) Request {
	t.Helper()
	params, err := json.Marshal(FileCheckParams{Root: root, Path: path, Internal: internal})
	if err != nil {
		t.Fatal(err)
	}
	return Request{ID: id, Op: OpPolicyCheckFile, Params: params}
}

// TestPromptInputsAreRefusedToAgentsButWritableInternally pins both halves of
// the dh-4 contract on the serve plane: an ordinary write is a hard
// path.restricted denial whatever HardRules says, and a write marked Internal is
// not refused by it. Red against a DevCouncil without the prompt-input rung;
// see devcouncil/zsec_prompt_inputs_test.go for what that means for the pin.
func TestPromptInputsAreRefusedToAgentsButWritableInternally(t *testing.T) {
	for _, hard := range []bool{true, false} {
		opts := hostOpts()
		opts.HardRules = hard
		for _, name := range promptInputs {
			root := t.TempDir()
			agent := decodeDecision(t, roundTrip(t, opts, internalFileCheck(t, "1", root, name, false))[0])
			if agent.Action != policy.Deny || agent.Rule != policy.RuleRestrictedPath || agent.Severity != policy.Hard || agent.Demoted != "" {
				t.Errorf("hard=%v %s: agent write = %s/%s/%s demoted=%q; want a hard %s "+
					"(waiting on the DevCouncil patch and the module-pins bump, task ft-d4be683f7753622abb4af08414900e39)",
					hard, name, agent.Action, agent.Rule, agent.Severity, agent.Demoted, policy.RuleRestrictedPath)
			}
			host := decodeDecision(t, roundTrip(t, opts, internalFileCheck(t, "2", root, name, true))[0])
			if host.Blocked() {
				t.Errorf("hard=%v %s: internal write = %s/%s, want allowed: the host's own writer must keep working",
					hard, name, host.Action, host.Rule)
			}
		}
	}
}
