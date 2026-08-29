package main

import (
	"strings"
	"testing"

	"manvi/llm/local"
	"manvi/repomap"
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
// someAreas stands in for a loaded code map, so the capability-gated sections
// are exercised rather than skipped by this guard.
var someAreas = []repomap.AreaSummary{{Name: "policy", Files: 9}}

func TestPromptNeverContradictsTheOfferedToolProfile(t *testing.T) {
	reg := newTestRegistry(t)
	_, pipeline, err := nativeToolsWith(reg, nil)
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}

	// The dynamic surface is the third profile and it is the one this class of
	// defect actually shipped on: it is smaller than the registry at the start
	// of a turn, so a section written against the full surface names tools the
	// model does not have. It is built on its own registry because
	// EnableDynamic is not reversible.
	dynamicReg := newTestRegistry(t)
	_, dynamicPipeline, err := nativeToolsWith(dynamicReg, nil)
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	dynamicPipeline.EnableDynamic()

	// The whole registry, so a tool absent from a narrowed profile is still
	// something this test knows to look for. Taken from the unnarrowed
	// pipeline, which is the only one that has them all.
	var allTools []string
	for _, s := range pipeline.Schemas() {
		allTools = append(allTools, s.Name)
	}

	for _, provider := range []string{"anthropic", "gemini", "xai", local.Name} {
		for _, profile := range []string{"full", "core-only", "dynamic"} {
			t.Run(provider+"/"+profile, func(t *testing.T) {
				pipeline, coreOnly, dynamic := pipeline, false, false
				switch profile {
				case "core-only":
					coreOnly = true
				case "dynamic":
					pipeline, dynamic = dynamicPipeline, true
				}
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
					DynamicTools:     dynamic,
					ActiveGroups:     pipeline.ActiveGroups(),
					// Resolved the same way the faces resolve it, so this test
					// exercises the decision that ships rather than a second
					// one written for the test.
					CodeMapAvailable: codeMapAvailable(
						harnessCapability{CodeMapConfigured: true, Areas: someAreas},
						pipeline, dynamic, coreOnly),
					DocLookupAvailable: true,
					Areas:              someAreas,
				})

				// Direction one: every devcouncil_* tool the prompt names by
				// its full id must be in the set the model will be given.
				//
				// Under dynamic loading the offered set can grow during the
				// turn, so naming an unlisted tool is no longer a dead end —
				// but only because tool-discovery tells the model how to fetch
				// it. Naming one without that section is the same defect as
				// before, so the exemption is conditional on the section being
				// there rather than on the mode being on.
				fetchable := dynamic && strings.Contains(text, "activate_tools")
				for _, s := range allTools {
					if !strings.Contains(text, s) || inProfile[s] {
						continue
					}
					if fetchable {
						continue
					}
					t.Errorf("the prompt names %q, which this profile does not offer", s)
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

// TestPromptContainsCoreEngineeringPrinciples asserts that the unconditional
// engineering principles are embedded and attributable across providers.
//
// The two that used to be listed here and no longer are — check the
// documentation, navigate by the dev map — moved to
// TestCapabilityGuidanceFollowsTheCapability, because they are no longer
// unconditional. Both name something this harness may not have: there is no
// first-party fetch at all, and the code graph needs a binary, an index and an
// activated tool group. They are asserted there in both directions.
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

// TestCapabilityGuidanceFollowsTheCapability is the rule PromptOptions already
// stated for the task tools, applied to the rest: the prompt never instructs
// work this harness cannot do.
//
// It fails against the shape it replaced, where one unconditional section told
// every run — including a local one with no MCP server and no code index — to
// "verify current documentation and online references" and to "utilize the
// repository dev map". Neither was reachable. An instruction whose only
// available form of compliance is to recall something and present it as checked
// is worse than no instruction.
func TestCapabilityGuidanceFollowsTheCapability(t *testing.T) {
	reg := registryWith(t, nil)
	for _, provider := range []string{"anthropic", local.Name} {
		t.Run(provider+"/absent", func(t *testing.T) {
			got := assemblePrompt(reg, PromptOptions{Provider: provider, TaskToolsOffered: true})
			for _, forbidden := range []string{"current documentation", "dev map", "code index"} {
				if strings.Contains(strings.ToLower(got), forbidden) {
					t.Errorf("the prompt mentions %q while the harness cannot do it", forbidden)
				}
			}
		})
		t.Run(provider+"/present", func(t *testing.T) {
			got := assemblePrompt(reg, PromptOptions{
				Provider: provider, TaskToolsOffered: true,
				CodeMapAvailable:   true,
				DocLookupAvailable: true,
				Areas: []repomap.AreaSummary{
					{Name: "policy", Files: 9}, {Name: "llm", Files: 40},
				},
			})
			for _, required := range []string{"current documentation", "dev map"} {
				if !strings.Contains(strings.ToLower(got), required) {
					t.Errorf("the prompt omits %q while the harness can do it", required)
				}
			}
			// And the shape is supplied rather than requested: the largest area
			// first, with its size.
			if !strings.Contains(got, "llm (40 files)") {
				t.Errorf("the repository shape did not reach the prompt:\n%s", got)
			}
			if strings.Index(got, "llm (40 files)") > strings.Index(got, "policy (9 files)") {
				t.Error("areas are not ordered largest first, so the list does not answer " +
					"\"where would this live\" at a glance")
			}
		})
	}
}
