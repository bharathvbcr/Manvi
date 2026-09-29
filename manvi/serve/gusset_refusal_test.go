package serve

import (
	"errors"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// The refusal branch of requireGusset: a failed engine check refuses the
// policy answer with E_INTERNAL, and the engine's own error text stays off
// the wire.
func TestPolicyCheckIsRefusedWhenTheEngineFailedItsCheck(t *testing.T) {
	prev := gussetReady
	t.Cleanup(func() { gussetReady = prev })
	gussetReady = func() error { return errors.New("engine-secret-detail: pattern is not valid UTF-8") }

	resp := roundTrip(t, hostOpts(), commandCheckWith(t, CommandCheckParams{
		Command: "echo hi",
		Root:    t.TempDir(),
	}))[0]
	if resp.Error == nil {
		t.Fatal("a failed engine check still answered the policy check")
	}
	if resp.Error.Code != ErrInternal {
		t.Fatalf("code %s, want %s", resp.Error.Code, ErrInternal)
	}
	if strings.Contains(resp.Error.Message, "engine-secret-detail") {
		t.Fatalf("engine error text reached the wire: %q", resp.Error.Message)
	}
}

// failingEngine is an engine that passed its check and then could not answer.
type failingEngine struct{ calls *int }

func (f failingEngine) MatchAny([]string, string) (bool, error) {
	*f.calls++
	return false, errors.New("engine is down")
}

func (f failingEngine) MatchAnyFold([]string, string) (bool, error) {
	*f.calls++
	return false, errors.New("engine is down")
}

// Once the engine has passed its check, it makes the decision: both gates and
// a redirect target ask it, and a matcher that cannot answer is a hard
// denial under the engine rule — never a guessed allow, and never demoted by
// the host posture.
func TestPolicyCheckAsksTheEngineAndFailsClosed(t *testing.T) {
	prevReady, prevMatcher := gussetReady, engineMatcher
	t.Cleanup(func() { gussetReady, engineMatcher = prevReady, prevMatcher })
	calls := 0
	gussetReady = func() error { return nil }
	engineMatcher = func() policy.Matcher { return failingEngine{calls: &calls} }

	root := t.TempDir()
	for name, req := range map[string]Request{
		"file":     fileCheck(t, "1", root, "src/a.go"),
		"command":  commandCheckWith(t, CommandCheckParams{Command: "go test ./...", Root: root, AllowedCommands: []string{"go test *"}}),
		"redirect": commandCheckWith(t, CommandCheckParams{Command: "echo hi > notes.txt", Root: root, AllowedCommands: []string{"echo *"}}),
	} {
		calls = 0
		resp := roundTrip(t, hostOpts(), req)[0]
		if resp.Error != nil {
			t.Fatalf("%s: %v", name, resp.Error)
		}
		d := decodeDecision(t, resp)
		if d.Action != policy.Deny || d.Severity != policy.Hard ||
			(d.Rule != policy.RulePathEngineUnavailable && d.Rule != policy.RuleCommandEngineUnavailable) {
			t.Errorf("%s: %s/%s/%s, want a hard engine denial (%s)", name, d.Action, d.Rule, d.Severity, d.Reason)
		}
		if calls == 0 {
			t.Errorf("%s: the engine was never asked", name)
		}
	}
}
