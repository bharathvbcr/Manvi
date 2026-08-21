package main

import (
	"strings"
	"testing"

	"manvi/flags"
	"manvi/llm/local"
	"manvi/prompt"
	"manvi/tools"
)

func routerRegistry(t *testing.T, settings map[string]string) *flags.Registry {
	t.Helper()
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range settings {
		if err := reg.Set(flags.Human, k, v); err != nil {
			t.Fatalf("set %s=%s: %v", k, v, err)
		}
	}
	return reg
}

func sectionNames(t *testing.T, sources []prompt.Source) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, src := range sources {
		secs, err := src.Sections()
		if err != nil {
			t.Fatalf("source %q: %v", src.Name(), err)
		}
		for _, s := range secs {
			out[s.Name] = s.Text
		}
	}
	return out
}

// The compact prompt must carry the same sections as the full one, only
// shorter. The two hand-written branches this replaced had already drifted:
// the compact one omitted mode-guidance, so turning the router on removed the
// pair-programming and YOLO guidance rather than condensing it.
func TestCompactPromptKeepsEverySectionTheFullOneHas(t *testing.T) {
	full := sectionNames(t, promptSources(
		routerRegistry(t, map[string]string{flags.LLMLocalGuidanceRouter: "false"}),
		PromptOptions{Provider: local.Name, TaskToolsOffered: true}))
	compact := sectionNames(t, promptSources(
		routerRegistry(t, map[string]string{flags.LLMLocalGuidanceRouter: "true"}),
		PromptOptions{Provider: local.Name, TaskToolsOffered: true}))

	for name := range full {
		if _, ok := compact[name]; !ok {
			t.Errorf("section %q is in the full prompt but missing from the compact one", name)
		}
	}
	if _, ok := compact["mode-guidance"]; !ok {
		t.Error("mode-guidance is absent from the compact prompt; " +
			"which mode the model is in is not a section worth saving tokens on")
	}
}

// Compact must actually be smaller, or the density is decorative.
func TestCompactPromptIsSmallerThanFull(t *testing.T) {
	opts := PromptOptions{Provider: local.Name, TaskToolsOffered: true}
	full, _ := assemblePromptWithFaults(
		routerRegistry(t, map[string]string{flags.LLMLocalGuidanceRouter: "false"}), opts)
	compact, _ := assemblePromptWithFaults(
		routerRegistry(t, map[string]string{flags.LLMLocalGuidanceRouter: "true"}), opts)

	fullTokens := prompt.EstimateTokens(full)
	compactTokens := prompt.EstimateTokens(compact)
	if compactTokens >= fullTokens {
		t.Fatalf("compact prompt is %d tokens, full is %d; compact must be smaller",
			compactTokens, fullTokens)
	}
	t.Logf("full=%d compact=%d tokens (%.0f%% saved)",
		fullTokens, compactTokens, 100*float64(fullTokens-compactTokens)/float64(fullTokens))
}

// Both densities must state which mode the model is in, and they must not
// disagree about it.
func TestModeGuidanceFollowsThePosture(t *testing.T) {
	for _, density := range []string{"false", "true"} {
		yolo := sectionNames(t, promptSources(
			routerRegistry(t, map[string]string{
				flags.HarnessPosture:         flags.PostureYolo,
				flags.LLMLocalGuidanceRouter: density,
			}),
			PromptOptions{Provider: local.Name, TaskToolsOffered: true}))
		if !strings.Contains(yolo["mode-guidance"], "YOLO") {
			t.Errorf("router=%s: yolo posture did not produce YOLO guidance: %q", density, yolo["mode-guidance"])
		}

		pair := sectionNames(t, promptSources(
			routerRegistry(t, map[string]string{
				flags.HarnessPosture:         flags.PostureStrict,
				flags.LLMLocalGuidanceRouter: density,
			}),
			PromptOptions{Provider: local.Name, TaskToolsOffered: true}))
		if !strings.Contains(pair["mode-guidance"], "Pair Programming") {
			t.Errorf("router=%s: strict posture did not produce pair-programming guidance: %q",
				density, pair["mode-guidance"])
		}
	}
}

// The local-only scaffolding must stay local-only. It is the largest block in
// the prompt and a frontier model does not need it.
func TestLocalOnlySectionsAreLocalOnly(t *testing.T) {
	reg := routerRegistry(t, nil)
	localSecs := sectionNames(t, promptSources(reg,
		PromptOptions{Provider: local.Name, TaskToolsOffered: true}))
	frontier := sectionNames(t, promptSources(reg,
		PromptOptions{Provider: "anthropic", TaskToolsOffered: true}))

	for _, name := range []string{"environment", "tool-contract", "working-method"} {
		if _, ok := localSecs[name]; !ok {
			t.Errorf("section %q is missing for a local provider", name)
		}
		if _, ok := frontier[name]; ok {
			t.Errorf("section %q was sent to a frontier provider that does not need it", name)
		}
	}
}

// A budget must never drop a rule. This is the property Essential exists for,
// and until the budget was wired it could not be exercised at all: the
// assembler was built with MaxTokens 0, so no section was ever a candidate.
func TestABudgetDropsGuidanceBeforeRules(t *testing.T) {
	reg := routerRegistry(t, nil)
	sources := promptSources(reg, PromptOptions{Provider: local.Name, TaskToolsOffered: true})

	// A budget far below any real prompt, so every non-essential section is a
	// candidate and only the essential ones can survive.
	text, _ := assembleSections(sources, 50)

	for _, must := range []string{
		"devcouncil_*",         // identity
		"Posture:",             // posture
		"blocked write",        // policy
		"devcouncil_next_task", // tasks
	} {
		if !strings.Contains(text, must) {
			t.Errorf("a starved budget dropped an essential section: %q is missing", must)
		}
	}
	// And the droppable guidance must be gone, or Essential is meaningless.
	if strings.Contains(text, "Hardening & Adversarial Stress-Testing") {
		t.Error("a starved budget kept non-essential guidance; Essential is not being honoured")
	}
}

// The budget is a share of the declared window, and unbounded off local.
func TestPromptTokenBudgetTracksTheContextWindow(t *testing.T) {
	reg := routerRegistry(t, map[string]string{flags.LLMLocalContextWindow: "8192"})
	if got := promptTokenBudget(reg, local.Name); got != 8192/promptBudgetShare {
		t.Errorf("budget = %d, want %d", got, 8192/promptBudgetShare)
	}
	if got := promptTokenBudget(reg, "anthropic"); got != 0 {
		t.Errorf("budget for a frontier provider = %d, want 0 (unbounded)", got)
	}
}

// The shipped local configuration must fit its own budget with room to spare.
// A default that starts out over budget would drop guidance on every turn and
// report a fault every time.
func TestTheShippedLocalPromptFitsItsBudget(t *testing.T) {
	reg := routerRegistry(t, nil)
	opts := PromptOptions{Provider: local.Name, TaskToolsOffered: true}
	text, faults := assemblePromptWithFaults(reg, opts)
	if len(faults) != 0 {
		t.Fatalf("the shipped local prompt reports faults: %v", faults)
	}
	budget := promptTokenBudget(reg, local.Name)
	if got := prompt.EstimateTokens(text); got > budget {
		t.Fatalf("the shipped local prompt is %d tokens against a %d budget", got, budget)
	}
}

// Compressing a section must not lose what it was there to say. This is the
// failure mode of a compact wording written by hand: the section name survives
// and the substance is edited away, which no section-name comparison catches.
//
// It is asserted at both densities because only one of them is the default at
// any time, and the other is one flag away from being what a run uses.
func TestBothDensitiesCarryTheEngineeringPrinciples(t *testing.T) {
	principles := []string{
		"Systematic Review & Core Reuse",
		"Strictly avoid duplication",
		"Grounding & Dev Map Guidance",
		"Problem Deconstruction & Hypotheses",
		"characterize baseline behavior with tests",
		"Hardening & Adversarial Stress-Testing",
		"Inquiry & Decision Impact",
	}
	for _, router := range []string{"false", "true"} {
		reg := routerRegistry(t, map[string]string{flags.LLMLocalGuidanceRouter: router})
		text, faults := assemblePromptWithFaults(reg,
			PromptOptions{Provider: local.Name, TaskToolsOffered: true})
		if len(faults) != 0 {
			t.Fatalf("router=%s: %v", router, faults)
		}
		for _, want := range principles {
			if !strings.Contains(text, want) {
				t.Errorf("router=%s: compacting dropped %q", router, want)
			}
		}
	}
}

// Dynamic loading and the prompt must agree about what an unlisted tool is.
// Telling a model that an unlisted tool does not exist, while the offered set
// is a fetchable subset, is what makes it substitute the nearest listed tool
// instead of loading the right one.
func TestDynamicToolsPromptTellsTheModelHowToLoadMore(t *testing.T) {
	reg := routerRegistry(t, nil)
	opts := PromptOptions{Provider: local.Name, TaskToolsOffered: true, DynamicTools: true}
	text, faults := assemblePromptWithFaults(reg, opts)
	if len(faults) != 0 {
		t.Fatalf("%v", faults)
	}
	for _, want := range []string{"devcouncil_search_tools", "devcouncil_activate_tools", "working set"} {
		if !strings.Contains(text, want) {
			t.Errorf("dynamic prompt does not mention %q", want)
		}
	}
	if strings.Contains(text, "A tool that is not listed does not exist") {
		t.Error("the dynamic prompt still claims unlisted tools do not exist, which is false when the set can grow")
	}
}

// And with dynamic loading off, the flat rule is correct and must stay.
func TestStaticToolsPromptKeepsTheFlatRule(t *testing.T) {
	reg := routerRegistry(t, nil)
	opts := PromptOptions{Provider: local.Name, TaskToolsOffered: true, DynamicTools: false}
	text, _ := assemblePromptWithFaults(reg, opts)
	if !strings.Contains(text, "A tool that is not listed does not exist") {
		t.Error("without dynamic loading the prompt must still say an unlisted tool does not exist")
	}
	if strings.Contains(text, "devcouncil_activate_tools") {
		t.Error("the prompt offers a tool-loading call that this configuration does not provide")
	}
}

// The shipped local defaults must be the optimized pair. This is the whole of
// the token claim: a default that has to be turned on is not a default.
func TestLocalDefaultsAreTheOptimizedPair(t *testing.T) {
	reg := routerRegistry(t, nil)
	if !dynamicToolsEnabled(reg, local.Name) {
		t.Error("dynamic tools are off by default for local")
	}
	if !guidanceRouterEnabled(reg, local.Name) {
		t.Error("the guidance router is off by default for local")
	}
	// And neither may leak to a frontier provider, where the prompt has never
	// been the scarce resource and the tool set is chosen for accuracy.
	if dynamicToolsEnabled(reg, "anthropic") || guidanceRouterEnabled(reg, "anthropic") {
		t.Error("a local-only optimization is enabled for a frontier provider")
	}
}

// Guidance must follow capability: a section about a tool group appears only
// when the model actually holds that group. Until ActiveGroups was populated,
// prompt.WhenGroupActive could never fire — the condition existed and no
// config ever satisfied it.
func TestGroupGuidanceFollowsTheActiveGroups(t *testing.T) {
	reg := routerRegistry(t, nil)
	base := PromptOptions{Provider: local.Name, TaskToolsOffered: true, DynamicTools: true}

	without := base
	without.ActiveGroups = []string{tools.GroupCore}
	text, faults := assemblePromptWithFaults(reg, without)
	if len(faults) != 0 {
		t.Fatalf("%v", faults)
	}
	if strings.Contains(text, "Dev map navigation") {
		t.Error("nav guidance was sent to a model holding no nav tools")
	}
	if strings.Contains(text, "Delegating to sub-agents") {
		t.Error("sub-agent guidance was sent to a model holding no sub-agent tools")
	}

	with := base
	with.ActiveGroups = []string{tools.GroupCore, tools.GroupNav}
	text, faults = assemblePromptWithFaults(reg, with)
	if len(faults) != 0 {
		t.Fatalf("%v", faults)
	}
	if !strings.Contains(text, "Dev map navigation") {
		t.Error("nav is active but its guidance is missing")
	}
	if strings.Contains(text, "Delegating to sub-agents") {
		t.Error("activating nav also pulled in sub-agent guidance")
	}
}

// The existence hint must NOT be group-gated. A model that can only learn a
// group exists after activating it would never activate it.
func TestToolDiscoveryNamesGroupsThatAreNotYetActive(t *testing.T) {
	reg := routerRegistry(t, nil)
	text, _ := assemblePromptWithFaults(reg, PromptOptions{
		Provider: local.Name, TaskToolsOffered: true, DynamicTools: true,
		ActiveGroups: []string{tools.GroupCore},
	})
	for _, group := range []string{"nav", "subagent", "mcp"} {
		if !strings.Contains(text, group) {
			t.Errorf("tool discovery does not name the %q group, so the model cannot ask for it", group)
		}
	}
}
