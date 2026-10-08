//go:build cgo

package serve

import (
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/gussetfn"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// Policy checks are decided by fnmatch (DevCouncil's
// docs/gusset-candidates.md), so with the engine closed they still answer,
// and with an ordinary decision. Before, serve asked the engine every pattern
// question: an engine that failed its check refused each policy check with
// E_INTERNAL, and one that failed mid-decision turned it into a hard
// engine_unavailable denial. Closing is one-way for this test binary; nothing
// in serve uses the engine.
func TestPolicyChecksDoNotUseTheEngine(t *testing.T) {
	if err := gussetfn.Close(); err != nil {
		t.Fatalf("closing the engine: %v", err)
	}
	root := t.TempDir()
	for name, req := range map[string]Request{
		"file":     fileCheck(t, "1", root, "src/a.go"),
		"secret":   fileCheck(t, "2", root, ".env"),
		"command":  commandCheckWith(t, CommandCheckParams{Command: "go test ./...", Root: root, AllowedCommands: []string{"go test *"}}),
		"redirect": commandCheckWith(t, CommandCheckParams{Command: "echo hi > notes.txt", Root: root, AllowedCommands: []string{"echo *"}}),
	} {
		resp := roundTrip(t, hostOpts(), req)[0]
		if resp.Error != nil {
			t.Fatalf("%s: refused with the engine closed: %s %s", name, resp.Error.Code, resp.Error.Message)
		}
		d := decodeDecision(t, resp)
		if d.Rule == policy.RulePathEngineUnavailable || d.Rule == policy.RuleCommandEngineUnavailable {
			t.Errorf("%s: decided by the engine: %s/%s (%s)", name, d.Action, d.Rule, d.Reason)
		}
		if name == "secret" && !d.Blocked() {
			t.Errorf("secret: .env was not refused: %s/%s", d.Action, d.Rule)
		}
	}
}
