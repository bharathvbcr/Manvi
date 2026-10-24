package main

import (
	"fmt"
	"os"
	"sync"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
	"github.com/bharathvbcr/Manvi/manvi/llm/local"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// turnFace is what the faces that run an agent loop deliberately do
// differently. Each difference is a field here rather than a line one face's
// copy of the construction happened to have, which is how `manvi run` and the
// TUI came to name different output bounds without either saying so.
type turnFace struct {
	// maxTokens is the output bound the face names. Zero leaves it to the
	// adapter, which applies the window it discovered — what `manvi run` and a
	// sub-agent want. The TUI names one; see tuiMaxTokens.
	maxTokens int
	// maxSteps is the turn's step ceiling.
	maxSteps int
	// readOnly offers only non-mutating tools, for a search child.
	readOnly bool
	// check is the end-of-turn sensor. Nil runs the loop without one, which
	// only a sub-agent does: its written paths return to the parent through
	// SubAgentResult.Wrote, and the parent's check covers them.
	check *sensor
	// chatOnly offers the model no tools. The zero value keeps a child
	// tool-capable. A chat-only parent has no schemas, so it cannot spawn.
	chatOnly bool
}

// turnDeps is what every face supplies the same way.
type turnDeps struct {
	provider        llm.Provider
	models          *llm.Registry
	model           string
	effort          string
	effortCeiling   string
	systemPrompt    string
	coreToolsOnly   bool
	assertInvariant bool
	log             *session.Log
	tools           *tools.Registry
}

// newTurnLoop builds the agent loop for one turn of any face.
//
// It owns the bus. The loop registers compaction on the bus it is given, and
// the end-of-turn check is attached here to that same instance: a listener on
// any other bus is a listener that never fires, which is how compaction was
// nearly shipped inert. One bus per call, so nothing a previous turn
// registered survives into this one.
//
// A check that fails to attach fails the construction rather than being
// logged. A harness that silently ran without its end-of-turn check would
// report every turn as finished, which is the exact condition the check was
// built to make impossible.
func newTurnLoop(face turnFace, deps turnDeps) (*agent.Loop, error) {
	loopBus := bus.New()
	loop, err := agent.NewLoop(agent.Config{
		Provider:        deps.provider,
		Registry:        deps.models,
		Model:           deps.model,
		Effort:          deps.effort,
		EffortCeiling:   deps.effortCeiling,
		SystemPrompt:    deps.systemPrompt,
		MaxSteps:        face.maxSteps,
		MaxTokens:       face.maxTokens,
		ReadOnly:        face.readOnly,
		ChatOnly:        face.chatOnly,
		CoreToolsOnly:   deps.coreToolsOnly,
		AssertInvariant: deps.assertInvariant,
		Price:           stepPrice(deps.provider, deps.model),
	}, loopBus, deps.log, deps.tools)
	if err != nil {
		return nil, err
	}
	if err := attachSensor(loopBus, face.check); err != nil {
		return nil, fmt.Errorf("the end-of-turn check could not be attached: %w", err)
	}
	return loop, nil
}

// stepPrice prices one response's usage from the table the provider publishes
// for model, or is nil when it publishes none — a local server, or a model the
// table does not list — so the loop counts those steps as unpriced rather than
// as free.
//
// Every face builds its loop here, so the parent's turn and every child's are
// priced by the same function from the same table the spend cap uses.
func stepPrice(provider llm.Provider, model string) func(llm.Usage) (int64, error) {
	pricer, ok := provider.(budget.Pricer)
	if !ok {
		return nil
	}
	prices, ok := pricer.PriceOf(model)
	if !ok {
		return nil
	}
	return prices.Charge
}

// maxSteps bounds a turn. A ceiling in code, not a prompt instruction.
//
// 500, and it is a backstop rather than a work budget.
//
// 24 was measured wrong, not merely tight: against a local Qwen3.8-27B, turns
// were repeatedly cut off at exactly 24 steps while still making progress, so
// the harness was reporting "ran out of steps" for work it had no reason to
// stop. A real change is locate (5-15 searches) plus read (5-15) plus edit
// (5-20) plus two or three build-and-fix rounds of a dozen each — over a
// hundred steps before anything has gone wrong. A ceiling set near the middle
// of legitimate work is not a safety bound, it is a random failure mode, and
// the exit-2 that reports it stops meaning anything.
//
// 500 is where kon, a comparable harness, sits, and no smaller number can be
// justified from evidence here: what was measured is that 24 is too low, not
// where the real distribution ends.
//
// The turn is not held together by this number. Context is bounded by the
// budget and compaction (agent/compaction.go), verbatim churn by RepeatLimit,
// and near-duplicate churn by NoProgressLimit — and since agent.StallCost
// charges a step that changed nothing three units instead of one, a turn that
// never gets anywhere hits the ceiling after about 170 steps rather than 500.
// This is the last line, for the turn that is bounded by nothing else.
// MANVI_MAX_STEPS is the escape hatch, and it used to be read here straight
// from the environment with fmt.Sscanf(v, "%d", &n) — a second, weaker config
// path standing beside the registry every other setting goes through. Sscanf
// stops at the first byte it cannot use and reports success for what it
// consumed, so "12x" became 12 and "1e3" became 1: an operator writing 1e3
// meaning a thousand got a ceiling of one step, and every turn ended after a
// single tool call reporting "ran out of steps". Nothing distinguished that
// from work that genuinely hit the ceiling, and because the key was not in the
// catalogue, `manvi flags --all` could not show it and Lookup could not say
// where it came from.
//
// It is flags.MaxSteps now, so a malformed value is refused at boot by
// LoadEnv, a config file can set it, and its origin is reportable.
//
// What is left here is the one thing the registry's KindInt validation cannot
// express: zero and negatives parse as integers perfectly well and would end
// every turn before it began. Those fall back to the shipped ceiling rather
// than to something smaller — this is the last line for a turn bounded by
// nothing else, and no misconfiguration should be able to tighten it. The
// fallback is said out loud, once: a ceiling that quietly reverted is how the
// original defect stayed invisible, and the resolved value cannot change while
// the harness runs, so there is exactly one thing to say.
var warnMaxSteps sync.Once

func maxSteps(reg *flags.Registry) int {
	n, origin, err := reg.Int(flags.MaxSteps)
	if err != nil || n <= 0 {
		warnMaxSteps.Do(func() {
			fmt.Fprintf(os.Stderr,
				"manvi: %s is not a positive whole number (origin %s); using the default ceiling of %d\n",
				flags.MaxSteps, origin, flags.DefaultMaxSteps)
		})
		return flags.DefaultMaxSteps
	}
	return n
}

// coreToolsOnly reports whether to offer the reduced tool surface. It applies
// to the local provider only: the setting is about a smaller model choosing
// from a long list, and it names that provider for the same reason every other
// llm.local.* setting does.
func coreToolsOnly(reg *flags.Registry, provider string) bool {
	if provider != local.Name {
		return false
	}
	on, _, err := reg.Bool(flags.LLMLocalCoreToolsOnly)
	return err == nil && on
}

// taskCheckoutTool is the task lifecycle's entry point, and the one tool whose
// presence decides which task section the prompt carries.
const taskCheckoutTool = "devcouncil_checkout_task"

// taskToolsOffered reports whether the profile this turn will actually offer
// still contains the task-lifecycle entry point.
//
// It asks the registry rather than reading llm.local.core_tools_only, because
// the flag is a request for a smaller surface, not a statement about what is
// in it. The prompt used to compute `!coreToolsOnly(...)`, which was a second,
// hand-maintained copy of devcouncil's profile rule living in a different
// package from the rule — and it went stale the first time the rule moved.
// Closing the core profile under its own prerequisites made
// devcouncil_checkout_task core, so core_tools_only stopped removing it; the
// prompt went on saying "There is no task to check out here" while the tool
// sat in the model's list, and `manvi run --task T` held a lease while telling
// the model there was no task to hold.
//
// Derived from the same two inputs agent.Loop uses to pick the set, so the
// prompt and the offered tools cannot drift apart again without the loop's own
// selection changing with them.
func taskToolsOffered(pipeline *tools.Registry, coreOnly bool) bool {
	offered := pipeline.Schemas()
	if coreOnly {
		offered = pipeline.CoreSchemas()
	}
	for _, s := range offered {
		if s.Name == taskCheckoutTool {
			return true
		}
	}
	return false
}

func assertInvariant(reg *flags.Registry) bool {
	on, _, err := reg.Bool(flags.LogModelVisibleAssert)
	return err == nil && on
}

func dynamicToolsEnabled(reg *flags.Registry, provider string) bool {
	if provider != local.Name {
		return false
	}
	on, _, err := reg.Bool(flags.LLMLocalDynamicTools)
	return err == nil && on
}

func guidanceRouterEnabled(reg *flags.Registry, provider string) bool {
	if provider != local.Name {
		return false
	}
	on, _, err := reg.Bool(flags.LLMLocalGuidanceRouter)
	return err == nil && on
}
