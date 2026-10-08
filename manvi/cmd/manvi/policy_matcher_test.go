package main

import (
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
)

// fnmatch is the one matcher policy decisions are made with (DevCouncil's
// docs/gusset-candidates.md): a nil Matcher is fnmatch. The gate used to be
// handed the Gusset engine in a cgo build, which answered the same questions
// about twice as slowly and could refuse a decision under
// path.engine_unavailable.
func TestBuildGateDecidesWithFnmatch(t *testing.T) {
	cleanup := setProjectRootForTest(t.TempDir())
	defer cleanup()
	reg, err := flags.NewHarnessRegistry(configPath())
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	g, _, err := buildGate(reg)
	if err != nil {
		t.Fatalf("buildGate: %v", err)
	}
	if g.Matcher != nil {
		t.Fatalf("gate matcher is %T; policy decisions must use fnmatch (nil)", g.Matcher)
	}
}
