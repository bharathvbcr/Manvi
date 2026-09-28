package serve

import (
	"errors"
	"strings"
	"testing"
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
