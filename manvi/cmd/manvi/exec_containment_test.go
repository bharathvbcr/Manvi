package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
)

// Containment ships off, the operator can turn it on, and a spelling the
// harness does not know is refused where it enters.
func TestExecContainmentFlag(t *testing.T) {
	reg := flags.New()
	if err := reg.Define(harnessFlagDefs()...); err != nil {
		t.Fatal(err)
	}
	if got := execContainmentMode(reg); got != execContainmentOff {
		t.Errorf("default mode = %q, want off", got)
	}
	if err := reg.LoadConfig(map[string]string{execContainmentKey: "secrets"}); err != nil {
		t.Fatal(err)
	}
	if got := execContainmentMode(reg); got != execContainmentSecrets {
		t.Errorf("configured mode = %q, want secrets", got)
	}
	bad := flags.New()
	if err := bad.Define(harnessFlagDefs()...); err != nil {
		t.Fatal(err)
	}
	if err := bad.LoadConfig(map[string]string{execContainmentKey: "on"}); err == nil {
		t.Error("an unknown mode was accepted")
	}
	if got := execContainmentMode(flags.New()); got != execContainmentOff {
		t.Errorf("a registry without the key gave %q, want off", got)
	}
}

// The setting narrows what the agent can read and the repository config is a
// file the agent can write under a relaxed posture: a clone must not be able to
// switch it on or off, and the advice for making it durable must not point at
// the file that refuses it.
func TestExecContainmentIsRefusedFromTheRepository(t *testing.T) {
	isolatedUserConfig(t)
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", execContainmentKey+": secrets\n")
	reg, err := newRegistry(filepath.Join(dir, "config.yaml"))
	if err == nil {
		t.Fatalf("a repository config set containment to %q", execContainmentMode(reg))
	}
	if !strings.Contains(err.Error(), "operator scope") || !strings.Contains(err.Error(), execContainmentKey) {
		t.Fatalf("refused without naming the rule: %v", err)
	}
	if advice := durableAdvice(execContainmentKey, "secrets"); !strings.Contains(advice, "refused in config files") {
		t.Errorf("durable advice sends the operator to a config file: %s", advice)
	}
}
