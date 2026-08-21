package main

import (
	"strings"
	"testing"

	"manvi/flags"
	"manvi/llm/anthropic"
)

// The banner an operator met on a machine with nothing configured:
//
//	manvi  posture dev  model anthropic (unconfigured)
//
// The resolver behind that word does not fail vaguely. It answers `set
// MANVI_MODEL — anthropic serves: …`, naming the variable to set and the values
// that would work, and all of it was being discarded for "unconfigured". The
// word says something is wrong and nothing about what to do, on the first screen
// of a session, before the operator has anything else to look at.

// TestAnUnconfiguredModelCarriesItsRemedy.
func TestAnUnconfiguredModelCarriesItsRemedy(t *testing.T) {
	t.Setenv("MANVI_MODEL", "")
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	host := &harnessHost{reg: reg}

	label, unresolved := host.describeModel(anthropic.Name)

	if !strings.Contains(label, "unconfigured") {
		t.Fatalf("with no model set the banner must say so, got %q", label)
	}
	if unresolved == "" {
		t.Fatal("the resolver's reason was discarded; the operator is told nothing to do")
	}
	if !strings.Contains(unresolved, "MANVI_MODEL") {
		t.Fatalf("the remedy must name what to set: %q", unresolved)
	}
	// The catalogue is the other half of the remedy: knowing the variable
	// without a value that works is one search away from being no help.
	served := anthropic.Models()
	if len(served) == 0 {
		t.Fatal("the anthropic adapter catalogues no models, so no remedy could name one")
	}
	if !strings.Contains(unresolved, served[0]) {
		t.Fatalf("the remedy must name a value that would work; %q names none of %v", unresolved, served)
	}
}

// TestAConfiguredModelSaysNothingExtra. The notice is emitted per session, so a
// reason produced when there is no problem would put a permanent line in front
// of every operator who has already configured one.
func TestAConfiguredModelSaysNothingExtra(t *testing.T) {
	t.Setenv("MANVI_MODEL", "claude-opus-5")
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	host := &harnessHost{reg: reg}

	label, unresolved := host.describeModel(anthropic.Name)
	if label != "claude-opus-5" {
		t.Fatalf("the banner must show the model that would be used, got %q", label)
	}
	if unresolved != "" {
		t.Fatalf("a resolved model has nothing to report, got %q", unresolved)
	}
}
