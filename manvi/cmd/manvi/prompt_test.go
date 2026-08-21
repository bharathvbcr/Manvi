package main

import (
	"strings"
	"testing"

	"manvi/llm/local"
)

// The prompt used to be identical for a frontier model and a 4-bit 27B. The
// smaller model does not infer what is unsaid, and this is the cheapest quality
// lever available.
func TestLocalModelsGetScaffoldingAFrontierModelDoesNot(t *testing.T) {
	reg := registryWith(t, nil)

	generic := systemPromptFor(reg, "anthropic")
	scaffolded := systemPromptFor(reg, local.Name)

	if len(scaffolded) <= len(generic) {
		t.Fatal("the local prompt carries no extra scaffolding")
	}
	for _, want := range []string{"working directory", "Calling tools", "does not change anything on disk"} {
		if !strings.Contains(scaffolded, want) {
			t.Errorf("the local prompt does not mention %q", want)
		}
		if strings.Contains(generic, want) {
			t.Errorf("the scaffolding leaked into every provider's prompt: %q", want)
		}
	}
	// The stopping condition matters most: a model that does not know when to
	// stop burns the step ceiling confirming work it already finished.
	if !strings.Contains(scaffolded, "stop and say what you changed") {
		t.Error("the local prompt never states when to stop")
	}
}

// Whatever else changes, the contract sections must survive, because a model
// told it may write anywhere will try.
func TestEveryProviderKeepsThePolicyContract(t *testing.T) {
	reg := registryWith(t, nil)
	for _, provider := range []string{"anthropic", "gemini", "xai", local.Name, ""} {
		got := systemPromptFor(reg, provider)
		if !strings.Contains(got, "policy gate") {
			t.Errorf("provider %q: the prompt does not mention the policy gate", provider)
		}
		if !strings.Contains(got, "blocked write") {
			t.Errorf("provider %q: the prompt does not state what to do about a block", provider)
		}
	}
}

func TestThePromptIsDeterministic(t *testing.T) {
	// Two runs with the same inputs must produce the same bytes, or the
	// server's prefix cache has nothing stable to key the system prompt on.
	reg := registryWith(t, nil)
	first := systemPromptFor(reg, local.Name)
	for i := 0; i < 5; i++ {
		if got := systemPromptFor(reg, local.Name); got != first {
			t.Fatal("the assembled prompt is not byte-stable across calls")
		}
	}
}

// The prompt must not name a tool the model cannot see. Under the reduced tool
// surface the task-lifecycle tools are gone, so telling the model to consult
// them teaches it to reach for something that does not exist — and the refusal
// it gets back is one it cannot act on.
func TestThePromptNeverNamesAToolTheProfileRemoved(t *testing.T) {
	reg := registryWith(t, nil)

	full := assemblePrompt(reg, PromptOptions{Provider: local.Name, TaskToolsOffered: true})
	if !strings.Contains(full, "devcouncil_next_task") {
		t.Error("the full surface should still explain the task tools")
	}

	reduced := assemblePrompt(reg, PromptOptions{Provider: local.Name, TaskToolsOffered: false})
	for _, absent := range []string{
		"devcouncil_next_task", "devcouncil_checkout_task", "check one out",
	} {
		if strings.Contains(reduced, absent) {
			t.Errorf("the reduced prompt still refers to %q", absent)
		}
	}
	// It must still say what to do instead, rather than simply going quiet.
	if !strings.Contains(reduced, "Read, edit and run commands directly") {
		t.Error("the reduced prompt dropped the guidance instead of replacing it")
	}
	// And the policy contract survives either way.
	if !strings.Contains(reduced, "blocked write") {
		t.Error("the reduced prompt lost the policy contract")
	}
}

// TestPromptNeverContradictsTheOfferedToolProfile is the mechanical version of
// the two hand-written checks above, run against the tool surface the harness
// actually ships rather than against a hard-coded expectation.
//
// It guards the class in both directions, because it has now failed in both.
// First the prompt named a tool the profile had removed: core_tools_only
// dropped the task lifecycle while the prompt still said "devcouncil_next_task
// lists planned work", so the model was taught to reach for something that did
// not exist. Then the profile was closed under its own prerequisites,
// devcouncil_checkout_task became core, and the prompt denied a tool the model
// could see — telling it "There is no task to check out here" with the tool in
// its list. Deriving TaskToolsOffered from the registry fixes the second; this
// test is what notices either one coming back.
func TestPromptNeverContradictsTheOfferedToolProfile(t *testing.T) {
	reg := newTestRegistry(t)
	_, pipeline, err := nativeToolsWith(reg, nil)
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}

	for _, provider := range []string{"anthropic", "gemini", "xai", local.Name} {
		for _, coreOnly := range []bool{false, true} {
			name := provider
			if coreOnly {
				name += "/core-only"
			}
			t.Run(name, func(t *testing.T) {
				offered := pipeline.Schemas()
				if coreOnly {
					offered = pipeline.CoreSchemas()
				}
				inProfile := make(map[string]bool, len(offered))
				for _, s := range offered {
					inProfile[s.Name] = true
				}

				text := assemblePrompt(reg, PromptOptions{
					Provider:         provider,
					TaskToolsOffered: taskToolsOffered(pipeline, coreOnly),
				})

				// Direction one: every devcouncil_* tool the prompt names by
				// its full id must be in the set the model will be given.
				for _, s := range pipeline.Schemas() {
					if strings.Contains(text, s.Name) && !inProfile[s.Name] {
						t.Errorf("the prompt names %q, which this profile does not offer", s.Name)
					}
				}

				// Direction two: the prompt must not deny the task lifecycle
				// while the profile offers its entry point.
				deniesTasks := strings.Contains(text, "There is no task to check out here")
				if deniesTasks && inProfile[taskCheckoutTool] {
					t.Errorf("the prompt says there is no task to check out while %q is offered",
						taskCheckoutTool)
				}
				if !deniesTasks && !inProfile[taskCheckoutTool] {
					t.Errorf("the prompt tells the model to check a task out while %q is not offered",
						taskCheckoutTool)
				}
			})
		}
	}
}

// TestPromptContainsCoreEngineeringPrinciples asserts that all 5 foundational
// engineering principles are embedded and attributable in the prompt across providers.
func TestPromptContainsCoreEngineeringPrinciples(t *testing.T) {
	reg := registryWith(t, nil)
	for _, provider := range []string{"anthropic", "gemini", "xai", local.Name} {
		got := systemPromptFor(reg, provider)
		principles := []struct {
			name string
			key  string
		}{
			{"Systematic Review & Core Reuse", "Systematic Review & Core Reuse"},
			{"No duplication/bloat", "Strictly avoid duplication"},
			{"Grounding & Dev Map", "Grounding & Dev Map Guidance"},
			{"Problem Deconstruction & Hypotheses", "Problem Deconstruction & Hypotheses"},
			{"Characterize baseline tests", "characterize baseline behavior with tests"},
			{"Hardening & Adversarial Testing", "Hardening & Adversarial Stress-Testing"},
			{"Inquiry & Decision Impact", "Inquiry & Decision Impact"},
		}
		for _, p := range principles {
			if !strings.Contains(got, p.key) {
				t.Errorf("provider %q: prompt missing principle %s (%q)", provider, p.name, p.key)
			}
		}
	}
}
