package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/llm/local"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/repomap"
	"github.com/bharathvbcr/Manvi/manvi/prompt"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// PromptOptions is what the assembled prompt has to match.
type PromptOptions struct {
	// Provider selects provider-specific scaffolding.
	Provider string
	// TaskToolsOffered reports whether the task-lifecycle tools are in the set
	// the model can see. When they are not, the prompt must not tell it to use
	// them: naming a tool that is not offered spends tokens teaching the model
	// to reach for something that does not exist, and a refusal it cannot act
	// on is worse than silence.
	TaskToolsOffered bool
	// DynamicTools reports whether the offered set can grow during the turn.
	//
	// It changes what is true about an unlisted tool, which the tool contract
	// otherwise states flatly: without dynamic loading a tool that is not
	// listed does not exist and naming it wastes a step, and with it the tool
	// exists and there is a call that fetches it. A model told the first while
	// the second is true does not decline — it substitutes the nearest listed
	// tool, which is the failure llm.local.core_tools_only was measured
	// producing (0/32 on tasks needing an omitted tool, never once declining).
	DynamicTools bool
	// ActiveGroups names the tool groups the model can actually reach this
	// turn, from tools.Registry.ActiveGroups. Guidance for a group the model
	// does not hold is context spent on a capability that is not there.
	ActiveGroups []string
	// CodeMapAvailable reports whether the code graph can actually be reached
	// this turn: the navigation tools are held or can be activated, and an
	// index and a binary exist behind them.
	//
	// All three have to be true and any one of them can be false
	// independently, which is why this is a resolved answer rather than
	// something the prompt infers from the group list.
	CodeMapAvailable bool
	// DocLookupAvailable reports whether this harness can reach documentation
	// at all — which, absent an MCP server that provides it, it cannot: there
	// is no first-party fetch, and a shell `curl` without a lease is refused
	// by name.
	//
	// Instructing a model to consult current documentation when nothing here
	// can fetch any is not harmless advice. It is an instruction whose only
	// available compliance is to recall something and present it as checked.
	DocLookupAvailable bool
	// Areas is the repository's own shape, from the code map, used to orient
	// the model instead of asking it to go and discover the layout.
	Areas []repomap.AreaSummary
}

// assemblePromptReported builds the system prompt and keeps the account of how
// it was built. It is the only assembly path: the string-only wrappers that
// sent faults to stderr were retired with the line-mode face.
//
// The Report has always been computed and always been thrown away: the faults
// went to stderr, which is invisible under this face's alternate screen and is
// not part of any session. So a turn whose hypothesis and hardening guidance
// had been displaced by an oversized instructions file looked, in the log and
// in every resumed session, exactly like one that carried them — and the
// evidence trail could not explain the difference in behaviour.
func assemblePromptReported(reg *flags.Registry, opts PromptOptions) (string, prompt.Report, []string) {
	return assembleSections(promptSources(reg, opts), promptTokenBudget(reg, opts.Provider))
}

// recordPromptAssembly writes what a turn's prompt actually carried into the
// session log.
//
// Failure to record is returned rather than swallowed. This is the event that
// explains a turn behaving unlike the prompt an operator believes it sent, and
// a log missing it is a log that will be read as though the prompt were whole.
//
// text is the exact system prompt the report describes. Its digest is recorded
// beside the section list because the list cannot show a changed byte, and a
// changed byte is what makes a provider's prefix cache go cold.
func recordPromptAssembly(log *session.Log, report prompt.Report, text string, budget int) error {
	if log == nil {
		return nil
	}
	sum := sha256.Sum256([]byte(text))
	data := session.PromptAssembledData{
		Tokens: report.Tokens, Budget: budget, Digest: hex.EncodeToString(sum[:]),
	}
	for _, in := range report.Included {
		data.Included = append(data.Included, in.Name)
	}
	// Both lists, because they mean different things: Omitted holds sections
	// the budget refused *and* essential ones kept past it, and Failed holds
	// contributors that broke. Flattening them would lose which happened.
	for _, group := range [][]prompt.Omitted{report.Omitted, report.Failed} {
		for _, o := range group {
			data.Dropped = append(data.Dropped, o.Name+": "+o.Reason)
		}
	}
	_, err := log.Append(session.PromptAssembled, data)
	return err
}

// promptTokenBudget is how many estimated tokens the system prompt may spend
// before non-essential sections start being dropped.
//
// It is a share of the model's declared context window rather than a constant,
// because the number only means anything relative to what the model has: 3,000
// tokens of preamble is rounding error on a frontier context and a tenth of a
// small local one. Zero means unbounded, which is what a frontier provider
// gets — there the prompt has never been the scarce resource, and a budget
// that never binds is a number to maintain for nothing.
//
// The share is deliberately generous. This is a backstop against a section
// growing without bound — in practice project-instructions, which is whatever
// AGENTS.md happens to say — not a lever for shaving the fixed sections, which
// is what the compact density is for. A budget tight enough to bite on a
// healthy prompt would drop guidance on every turn and report a fault every
// time, which trains an operator to ignore the report.
func promptTokenBudget(reg *flags.Registry, provider string) int {
	if provider != local.Name {
		return 0
	}
	window, _, err := reg.Int(flags.LLMLocalContextWindow)
	if err != nil || window <= 0 {
		return 0
	}
	return window / promptBudgetShare
}

// promptBudgetShare is the divisor in promptTokenBudget: the system prompt may
// use up to one part in this many of the context window. At the default 32,768
// window that is 4,096 tokens, against a full prompt of roughly 900.
const promptBudgetShare = 8

// promptSources lists the sections a turn's prompt is built from, in the order
// they are contributed. Order within the prompt is the Section.Order field, not
// this slice — see prompt.Assemble.
func promptSources(reg *flags.Registry, opts PromptOptions) []prompt.Source {
	return promptRouter(reg, opts).Sources()
}

// promptRouter builds the routed prompt for a turn.
//
// There is one definition of every section here, and the router picks the
// wording. What stood here before was two branches — one per state of
// llm.local.guidance_router — holding the same twelve sections at two lengths,
// which is the duplication this repo refuses everywhere else. It had already
// drifted: the compact branch dropped mode-guidance entirely, so turning the
// router on silently took away the pair-programming and YOLO guidance rather
// than shortening it. A single list cannot lose a section that way.
func promptRouter(reg *flags.Registry, opts PromptOptions) *prompt.Router {
	provider := opts.Provider
	posture, _, _ := reg.String(flags.HarnessPosture)

	density := prompt.DensityFull
	if guidanceRouterEnabled(reg, provider) {
		density = prompt.DensityCompact
	}
	r := prompt.NewRouter(prompt.RouterConfig{
		Density:      density,
		Provider:     provider,
		ActiveGroups: opts.ActiveGroups,
	})

	// Essential is the line between what the model must be told and what makes
	// it work better. Identity, posture, the task rules, the policy rule and
	// the tool contract are the first: a model that has lost the policy section
	// to a budget is a model that will try the write the gate refused. The
	// working method, the review and hardening guidance, and whatever the
	// project's own instructions file says are the second — worth sending, not
	// worth sending in place of a rule.
	// Identity and the policy rule are the same at either density: both are
	// already as short as they can be said, and shortening a rule is how a rule
	// stops being followed.
	r.Vary("identity", 10, true, func(prompt.RouterConfig) string {
		return "You are a builder agent inside MANVI (DevCouncil harness).\n\n" +
			"Work through the native devcouncil_* tools. They are the only way to read or write\n" +
			"files here, and every write passes a policy gate before it lands."
	})

	r.Vary("posture", 20, true, func(cfg prompt.RouterConfig) string {
		text := fmt.Sprintf("Posture: %s.", posture)
		effect := flags.DescribePosture(posture)
		if !effect.Relaxed {
			return text
		}
		text += "\n" + effect.Notice
		if cfg.Density == prompt.DensityFull {
			text += "\n" +
				"A relaxed posture is not permission to widen what you were asked to do: it\n" +
				"means the operator, not the gate, is the only thing left checking."
		}
		return text
	})

	r.Vary("codebase-review", 22, false, func(cfg prompt.RouterConfig) string {
		if cfg.Density == prompt.DensityCompact {
			return "Systematic Review & Core Reuse:\n" +
				"Review the codebase to understand existing functions. Strictly avoid duplication:\n" +
				"extend existing seams."
		}
		return "Systematic Review & Core Reuse:\n" +
			"Upon initiating any project, systematically review the codebase to comprehend existing\n" +
			"functions, registries, and features. Construct upon them as a deliberate core.\n" +
			"Strictly avoid duplication, parallel abstractions, and bloat: extend existing seams\n" +
			"rather than standing up duplicate implementations."
	})

	// This was one section telling the model to do two things it could not
	// necessarily do: check current documentation, and navigate by the dev map.
	// Neither capability is guaranteed — there is no first-party fetch in this
	// harness at all, and the code-graph tools are Extended, so on a dynamic
	// surface they are not offered until something activates them, and even
	// then they need an index and a binary. The section shipped unconditionally
	// in both densities, so the ordinary local run was instructed to use tools
	// it did not hold and to consult sources it could not reach.
	//
	// PromptOptions already states the rule this breaks — "naming a tool that is
	// not offered spends tokens teaching the model to reach for something that
	// does not exist" — and already applies it to the task tools. Split in two
	// and gated the same way, so each half appears exactly when the thing it
	// names is real.
	if opts.DocLookupAvailable {
		r.Vary("grounding-docs", 23, false, func(cfg prompt.RouterConfig) string {
			if cfg.Density == prompt.DensityCompact {
				return "Grounding:\n" +
					"Check current documentation rather than relying on memory."
			}
			return "Grounding:\n" +
				"When working with a language, library, or framework, check its current documentation\n" +
				"rather than relying on memory or intuition. Recall of an API is a guess about the\n" +
				"version in front of you; the documentation is the version in front of you."
		})
	}

	if opts.CodeMapAvailable {
		r.Vary("devmap-grounding", 24, false, func(cfg prompt.RouterConfig) string {
			if cfg.Density == prompt.DensityCompact {
				return "Dev map:\n" +
					"Use the repository dev map for structured symbol navigation rather than guessing\n" +
					"at paths."
			}
			return "Dev map:\n" +
				"This repository has a code index. Use the dev map and code intelligence for structured\n" +
				"navigation and exact symbol relationships rather than guessing at paths or grepping for\n" +
				"names you expect to exist."
		})
	}

	// The shape of the repository, supplied rather than requested.
	//
	// The section above used to tell the model to go and find the structure;
	// this hands it over. It costs a few dozen tokens and removes the round
	// trips a model otherwise spends discovering that a repository has a policy
	// layer and where it lives.
	if len(opts.Areas) > 0 {
		r.Vary("repo-orientation", 21, false, func(cfg prompt.RouterConfig) string {
			return repoOrientationSection(opts.Areas, cfg.Density)
		})
	}

	// Kept in both densities. Which of these two the model is reading is the
	// difference between it asking before acting and it acting alone, and a
	// shorter prompt is not worth getting that backwards.
	r.Vary("mode-guidance", 25, false, func(cfg prompt.RouterConfig) string {
		if posture == flags.PostureYolo {
			if cfg.Density == prompt.DensityCompact {
				return "YOLO Autonomous Mode:\n" +
					"Execute decisively and autonomously. Explore, test hypotheses, and create structured\n" +
					"artifacts. When options are ambiguous, proceed with the recommended default."
			}
			return "YOLO Autonomous Mode:\n" +
				"Move fast with decisive, autonomous execution. Take initiative to explore, discover files, " +
				"test hypotheses, and create structured artifacts. When options are ambiguous, proceed with the recommended default."
		}
		if cfg.Density == prompt.DensityCompact {
			return "Pair Programming & Guidance:\n" +
				"Collaborate transparently. State the rationale for non-obvious changes, the tradeoffs,\n" +
				"and the verification evidence. Plan before acting on underspecified requirements."
		}
		return "Pair Programming & Guidance:\n" +
			"Collaborate transparently with the user. Clearly communicate rationale for non-obvious changes, " +
			"design tradeoffs, and verification evidence. For underspecified requirements, formulate structured plans."
	})

	r.Vary("tasks", 30, true, func(cfg prompt.RouterConfig) string {
		if !opts.TaskToolsOffered {
			// The task tools are not in this model's set, so there is nothing
			// to check out and nothing to look for.
			return "There is no task to check out here. Read, edit and run commands directly;\n" +
				"the gate judges each write on its own."
		}
		if cfg.Density == prompt.DensityCompact {
			return "Tasks: devcouncil_next_task lists planned work. Check a task out only if it is\n" +
				"listed there — task ids cannot be invented. If the work is not a planned task,\n" +
				"read, edit and run commands directly."
		}
		return "Tasks: devcouncil_next_task lists planned work. Check a task out only if it is\n" +
			"listed there — task ids cannot be invented, and inventing one is refused. If the\n" +
			"work you were asked to do is not a planned task, do not look for one and do not\n" +
			"check one out: read, edit and run commands directly. The gate judges each write\n" +
			"on its own in that case."
	})

	r.Vary("policy", 40, true, func(prompt.RouterConfig) string {
		return "A blocked write is not a reason to try another path to the same file — in\n" +
			"particular, do not write through the shell what the write tool refused. Say what\n" +
			"was blocked and why, and let the operator decide."
	})

	r.Vary("problem-deconstruction", 42, false, func(cfg prompt.RouterConfig) string {
		if cfg.Density == prompt.DensityCompact {
			return "Problem Deconstruction & Hypotheses:\n" +
				"Deconstruct the problem and state hypotheses before implementing.\n" +
				"Break it before you fix it: characterize baseline behavior with tests first."
		}
		return "Problem Deconstruction & Hypotheses:\n" +
			"Before implementation, deconstruct problems and formulate explicit hypotheses.\n" +
			"Verify completeness by identifying and resolving all gaps.\n" +
			"Break it before you fix it: characterize baseline behavior with tests before modifying code,\n" +
			"and maintain clean persistent artifacts under .devcouncil/artifacts/."
	})

	r.Vary("hardening-stress-testing", 43, false, func(cfg prompt.RouterConfig) string {
		if cfg.Density == prompt.DensityCompact {
			return "Hardening & Adversarial Stress-Testing:\n" +
				"Validate boundaries, fail closed, bound timeouts and retries, propagate cancellation,\n" +
				"and adversarially probe edge cases (empty, nil, boundary, concurrent, timeout)."
		}
		return "Hardening & Adversarial Stress-Testing:\n" +
			"Guarantee universality and resilience by applying maximal hardening measures: validate boundaries,\n" +
			"fail closed, bound timeouts and retries, and propagate cancellation. Rigorously stress-test and\n" +
			"adversarially probe the solution (empty, nil, boundary, concurrent, and error inputs) to disrupt\n" +
			"existing logic and confirm robustness prior to deployment."
	})

	r.Vary("inquiry-impact", 44, false, func(cfg prompt.RouterConfig) string {
		if cfg.Density == prompt.DensityCompact {
			return "Inquiry & Decision Impact:\n" +
				"Clarify intent and surface architectural trade-offs before undertaking complex tasks\n" +
				"or designing collaborative UI."
		}
		return "Inquiry & Decision Impact:\n" +
			"Prioritize inquiry and clarify the impact of decisions before undertaking complex tasks\n" +
			"or designing the UI for collaborative applications. Clarify user intent and surface\n" +
			"architectural trade-offs early."
	})

	// Local-only, in both densities. These three answer questions a local model
	// asks and a frontier one does not — what the environment is, what the tool
	// contract requires literally, how to proceed when unsure — and they are
	// the sections whose absence shows up as malformed calls rather than as
	// worse judgement.
	onLocal := prompt.WhenProvider(local.Name)
	r.Vary("environment", 15, true, func(prompt.RouterConfig) string { return environmentSection() }, onLocal)
	r.Vary("tool-contract", 45, true, func(prompt.RouterConfig) string {
		return toolContractSection(opts.DynamicTools, opts.CodeMapAvailable)
	}, onLocal)
	if opts.DynamicTools {
		// Essential, and not only for local. The offered set is smaller than
		// the registry whenever this is on, and a model that does not know the
		// rest can be fetched reaches for the nearest listed tool instead of
		// the right one.
		r.Vary("tool-discovery", 46, true, func(prompt.RouterConfig) string {
			return toolDiscoverySection()
		})
	}
	r.Vary("working-method", 50, false,
		func(prompt.RouterConfig) string { return workingMethodSection(opts.CodeMapAvailable) }, onLocal)

	// Guidance that follows capability. These sections cost nothing on a turn
	// where their tools are not loaded, and appear on the turn the model
	// activates the group — which is exactly when it needs them and has not
	// been told anything about how the tools behave.
	//
	// The existence hint lives in tool-discovery, not here, and that split is
	// load-bearing: gating the hint on the same condition would mean a model
	// could only learn a group exists after activating it, which it would
	// never do.
	// Gated on the capability as well as on the group. "Active group" and
	// "offered tool" are different questions and core_tools_only is where they
	// diverge: it narrows the schemas the model receives and leaves the group
	// list alone, so this section named devcouncil_graph_query to a model that
	// had not been given it.
	navGuidance := func(prompt.RouterConfig) string {
		return "Dev map navigation:\n" +
			"  - devcouncil_graph_query and devcouncil_graph_context resolve exact symbol\n" +
			"    relationships. Prefer them to grep for \"who calls this\" and \"what breaks\".\n" +
			"  - The graph is built from an index that can be stale. When it disagrees with\n" +
			"    the file you just read, the file wins."
	}
	if opts.CodeMapAvailable {
		r.Vary("nav-guidance", 26, false, navGuidance, prompt.WhenGroupActive(tools.GroupNav))
	}

	r.Vary("subagent-guidance", 27, false, func(prompt.RouterConfig) string {
		return "Delegating to sub-agents:\n" +
			"  - Give each child one scoped instruction and the context to act on it; a child\n" +
			"    does not see this conversation.\n" +
			"  - A child cannot dispatch children, so do not plan a tree.\n" +
			"  - Read what each child returned before acting on it. An empty summary is a\n" +
			"    failure, not a silent success."
	}, prompt.WhenGroupActive(tools.GroupSubagent))

	// The project's own instructions are the one section with no bound on their
	// size: AGENTS.md is whatever the repo wrote. It is not Essential for that
	// reason — a file that grew past the budget must lose to the rules, not
	// push them out.
	if pSrc, ok := projectInstructionsSource(promptTokenBudget(reg, provider)); ok {
		r.Add(pSrc)
	}
	return r
}

func projectInstructionsSource(budget int) (prompt.Source, bool) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"} {
		data, err := os.ReadFile(name)
		if err == nil && len(bytes.TrimSpace(data)) > 0 {
			body := boundProjectInstructions(strings.TrimSpace(string(data)), budget)
			return prompt.Static("project-instructions", 35, false,
				fmt.Sprintf("Project instructions (%s):\n%s", name, body)), true
		}
	}
	return nil, false
}

// projectInstructionShare is the fraction of the prompt budget this file may
// occupy: one over this, so 2 means half.
//
// It exists because assembly is a single greedy pass in section order and this
// section sits at 35, ahead of the four that carry hypotheses, hardening,
// inquiry and working method at 42 through 50. An instructions file large
// enough to fit and large enough to exhaust what remains therefore displaced
// exactly those four — silently, since the report went to stderr, which is
// invisible under the TUI's alternate screen.
//
// Bounding this rather than reordering the sections is the deliberate half of
// the fix. The operator's own instructions are more specific to the task than
// any generic guidance here, so they should not be the first thing dropped;
// what they should not be able to do is take everything else with them.
const projectInstructionShare = 2

// boundProjectInstructions trims the instructions file to its share of the
// budget, on a line boundary, and says that it did.
//
// Cut at a line rather than at a character because half a sentence of a rule is
// worse than no rule: it reads as a complete instruction that happens to be
// wrong. The marker is not decoration — an operator whose instructions stop
// being followed needs to know the file is too long for the window rather than
// concluding the model ignores them.
func boundProjectInstructions(body string, budget int) string {
	if budget <= 0 {
		// No budget means an unbounded prompt, which is what every non-local
		// provider gets. Nothing here is scarce, so nothing is cut.
		return body
	}
	limit := budget / projectInstructionShare
	if prompt.EstimateTokens(body) <= limit {
		return body
	}
	var kept strings.Builder
	spent := 0
	for line := range strings.Lines(body) {
		cost := prompt.EstimateTokens(line)
		if spent+cost > limit {
			break
		}
		kept.WriteString(line)
		spent += cost
	}
	return strings.TrimRight(kept.String(), "\n") +
		fmt.Sprintf("\n\n[this instructions file was cut here: it exceeds %d tokens, which is "+
			"half the system-prompt budget on this model. The rest was not sent. Shorten it, or "+
			"the guidance after it in the prompt is what pays for the excess]", limit)
}

// assembleSections registers every source, assembles, and names every source
// whose content did not reach the prompt.
//
// The test is "did this source's content arrive", not "does the Report mention
// it". Report.Omitted also carries the benign case of an essential section
// kept past the budget, so matching on reason strings would make this brittle
// in the direction of crying wolf on a healthy prompt. Comparing what was
// registered against Report.Included answers the question directly, and stays
// correct if the Report grows new reasons.
func assembleSections(sources []prompt.Source, maxTokens int) (string, prompt.Report, []string) {
	a := prompt.New()
	a.MaxTokens = maxTokens
	var faults, registered []string
	for _, src := range sources {
		if err := a.Add(src); err != nil {
			// Add refuses a duplicate or unnamed source, which makes the
			// report ambiguous about which contributor produced what. That is
			// a programming error, but it was being discarded seven times over
			// and would have cost a whole section without a word.
			faults = append(faults, fmt.Sprintf("%q was not registered: %v", src.Name(), err))
			continue
		}
		registered = append(registered, src.Name())
	}

	text, report := a.Assemble()

	arrived := make(map[string]bool, len(report.Included))
	for _, in := range report.Included {
		arrived[in.Source] = true
	}
	for _, name := range registered {
		if arrived[name] {
			continue
		}
		faults = append(faults, fmt.Sprintf("%q is missing from the prompt: %s", name, whyMissing(report, name)))
	}
	return text, report, faults
}

// whyMissing recovers the Report's own account of a source that did not
// arrive. A reason the assembler already worked out is more use than one this
// function invents, and the fallback exists only so a source that vanished for
// a reason the Report does not carry is still reported as vanished.
func whyMissing(report prompt.Report, source string) string {
	for _, group := range [][]prompt.Omitted{report.Failed, report.Omitted} {
		for _, o := range group {
			if o.Source == source {
				return o.Reason
			}
		}
	}
	return "the assembly report does not say why"
}

// environmentSection states the facts a model cannot infer and will otherwise
// invent: where it is, and what it is running on.
func environmentSection() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd = "(unknown)"
	}
	return fmt.Sprintf(
		"Environment:\n"+
			"  working directory: %s\n"+
			"  platform: %s/%s\n"+
			"Paths you pass to tools are relative to the working directory unless absolute.",
		cwd, runtime.GOOS, runtime.GOARCH)
}

// toolContractSection spells out the call contract.
//
// Every line here is a failure seen from a smaller model: inventing a tool that
// was not offered, describing an edit instead of making it, and re-reading the
// same file because nothing said the result was already in the conversation.
func toolContractSection(dynamic, codeMap bool) string {
	unlisted := "  - Use only the tools listed for you. A tool that is not listed does not exist,\n" +
		"    and naming one wastes a step.\n"
	if dynamic {
		// Under dynamic loading the flat rule above is false, and stating it
		// anyway is worse than saying nothing: it tells the model its only
		// option is a listed tool, so it picks the closest one rather than
		// fetching the right one. See toolDiscoverySection.
		unlisted = "  - The tools listed for you are a working set, not the whole registry. Do not\n" +
			"    name a tool that is not listed — activate it first, see below.\n"
	}
	// The same rule as the sections above: this used to say "consult the dev
	// map" on every local run, including the ones with no index and no
	// navigator, which is an instruction whose only available compliance is to
	// pretend. What is left when there is no map is still true.
	beforeEditing := "  - Read a file before editing it; do not re-read unchanged files.\n"
	if codeMap {
		beforeEditing = "  - Read a file and consult the dev map before editing it; do not re-read\n" +
			"    unchanged files.\n"
	}
	return "Calling tools:\n" + unlisted +
		"  - To change a file, call the write or patch tool. Describing the change in prose\n" +
		"    does not change anything on disk.\n" +
		beforeEditing +
		"  - One step may contain several tool calls when they are independent. Calls that\n" +
		"    depend on each other's results belong in separate steps.\n" +
		"  - If a call is refused, read the reason before trying anything else. Repeating\n" +
		"    an identical call returns an identical answer and is refused after a few tries."
}

// toolDiscoverySection is contributed only when the offered set can grow.
//
// It is the other half of dynamic loading. Narrowing the set without it does
// not make a model ask for what is missing; it makes it answer with what is
// present, which is how "which task next?" gets answered by list_dir.
func toolDiscoverySection() string {
	return "Loading more tools:\n" +
		"  - The listed tools are a working set. Task lifecycle, code-graph navigation,\n" +
		"    sub-agents, artifacts and MCP tools exist but are not loaded yet.\n" +
		"  - devcouncil_search_tools finds them by keyword or group and costs no context.\n" +
		"  - devcouncil_activate_tools loads them by name or by group — task, nav, subagent,\n" +
		"    artifact, mcp — and they are callable from the next step onward.\n" +
		"  - When no listed tool does what the step needs, search and activate. Do not\n" +
		"    substitute the closest listed tool, and do not report the work impossible."
}

// workingMethodSection gives the shape of a turn and, importantly, its end.
// Not essential: it is guidance rather than contract, so it yields to budget
// before the policy and tool sections do.
func workingMethodSection(codeMap bool) string {
	// The same conditional as the sections above. "Review existing code" is
	// true everywhere; "and dev map navigation" is only true where there is a
	// dev map, and it shipped on every local run regardless.
	review := "  - Systematically review existing code before changing anything.\n"
	if codeMap {
		review = "  - Systematically review existing code and dev map navigation before changing\n" +
			"    anything.\n"
	}
	return "Working method:\n" + review +
		"  - Deconstruct problems, formulate hypotheses, and identify gaps upfront.\n" +
		"  - Characterize baseline behavior with tests first; reuse and extend the core without bloat.\n" +
		"  - Make the smallest change that does the job, then check it.\n" +
		"  - Apply maximal hardening and stress-test edge cases before completing.\n" +
		"  - Clarify decisions and surface impact early on complex or UI workflows.\n" +
		"  - When the task is done, stop and say what you changed. Do not keep calling tools\n" +
		"    to confirm work you have already confirmed.\n" +
		"  - If you cannot finish, say what is blocking you rather than doing something else\n" +
		"    instead."
}
