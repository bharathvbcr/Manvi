package main

import (
	"fmt"

	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
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
		CoreToolsOnly:   deps.coreToolsOnly,
		AssertInvariant: deps.assertInvariant,
	}, loopBus, deps.log, deps.tools)
	if err != nil {
		return nil, err
	}
	if err := attachSensor(loopBus, face.check); err != nil {
		return nil, fmt.Errorf("the end-of-turn check could not be attached: %w", err)
	}
	return loop, nil
}
