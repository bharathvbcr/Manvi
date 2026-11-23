package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agent"
)

// The batching rule ships enforcing, an operator can relax it, and a spelling
// the loop does not know is refused where it enters rather than reaching
// agent.NewLoop as an unknown mode.
func TestSerialLookupsFlag(t *testing.T) {
	reg := flags.New()
	if err := reg.Define(harnessFlagDefs()...); err != nil {
		t.Fatal(err)
	}
	if got := serialLookupMode(reg); got != agent.SerialLookupsRefuse {
		t.Errorf("default mode = %q, want refuse", got)
	}
	if err := reg.LoadConfig(map[string]string{serialLookupsKey: "note"}); err != nil {
		t.Fatal(err)
	}
	if got := serialLookupMode(reg); got != agent.SerialLookupsNote {
		t.Errorf("configured mode = %q, want note", got)
	}
	bad := flags.New()
	if err := bad.Define(harnessFlagDefs()...); err != nil {
		t.Fatal(err)
	}
	if err := bad.LoadConfig(map[string]string{serialLookupsKey: "always"}); err == nil {
		t.Error("an unknown mode was accepted")
	}
	if got := serialLookupMode(flags.New()); got != agent.SerialLookupsRefuse {
		t.Errorf("a registry without the key gave %q, want the strict default", got)
	}
}

// The rule exists to bind the agent, and the repository config is a file the
// agent can write under a relaxed posture: a clone that switched it off would
// switch it off for every agent working in it.
func TestSerialLookupsIsRefusedFromTheRepository(t *testing.T) {
	isolatedUserConfig(t)
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", serialLookupsKey+": off\n")
	reg, err := newRegistry(filepath.Join(dir, "config.yaml"))
	if err == nil {
		t.Fatalf("a repository config switched the batching rule to %q", serialLookupMode(reg))
	}
	if !strings.Contains(err.Error(), "operator scope") || !strings.Contains(err.Error(), serialLookupsKey) {
		t.Fatalf("refused without naming the rule: %v", err)
	}
}
