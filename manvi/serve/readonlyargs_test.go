package serve

import (
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// A read-only command is read-only only for its arguments too. At the old pin
// the allowlist matched the command's name alone, so these wrote a file of the
// caller's choosing, or read one outside the root, as "read-only".
func TestReadOnlyCommandsAreJudgedByTheirArguments(t *testing.T) {
	root := t.TempDir()
	for _, command := range []string{
		"git diff --output=.git/config",
		"git diff --no-index /etc/passwd README.md",
		"pytest --junitxml=.env",
		"pytest --basetemp=/tmp/elsewhere",
	} {
		d := decodeDecision(t, roundTrip(t, hostOpts(), rawRequest(t, OpPolicyCheckCommand, map[string]any{
			"command": command, "root": root, "enforce_allowlist": true,
		}))[0])
		if d.Action == policy.Allow {
			t.Errorf("%q was allowed (rule=%q); its arguments write or read outside the read-only contract", command, d.Rule)
		}
	}
}
