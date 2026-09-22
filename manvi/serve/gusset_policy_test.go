//go:build !cgo

package serve

import (
	"errors"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/gussetcheck"
)

func TestPolicyCheckRunsWhenGussetEngineIsNotLinked(t *testing.T) {
	if err := gussetcheck.Ready(); !errors.Is(err, gussetcheck.ErrNotLinked) {
		t.Fatalf("Ready() = %v, want ErrNotLinked", err)
	}
	root := t.TempDir()
	resp := roundTrip(t, hostOpts(), commandCheckWith(t, CommandCheckParams{
		Command: "echo hi",
		Root:    root,
	}))[0]
	if resp.Error != nil {
		t.Fatalf("policy.check.command error %s: %s", resp.Error.Code, resp.Error.Message)
	}
	d := decodeDecision(t, resp)
	if d.Action == "" {
		t.Fatal("policy.check.command returned no action")
	}
}
