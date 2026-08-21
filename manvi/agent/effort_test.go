package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"manvi/core/bus"
	"manvi/llm"
	"manvi/llm/replay"
	"manvi/session"
)

// reasoningCaps is the test model with a declared effort ladder. The order is
// the local adapter's own — least reasoning first — because that order is what
// "one rung up" means.
func reasoningCaps(levels ...string) llm.Capability {
	c := caps("test-model")
	c.SupportsReasoning = true
	c.EffortLevels = levels
	return c
}

func localLadder() llm.Capability {
	return reasoningCaps("none", "low", "medium", "high")
}

// efforts is the tier every request the provider was asked to serve carried, in
// order. The request is the only place the escalation is observable from the
// outside, which is exactly why the assertions are made there.
func efforts(h *harness) []string {
	var out []string
	for _, req := range h.provider.Requests() {
		out = append(out, req.Effort)
	}
	return out
}

// TestEffortRisesWhenNearDuplicateWorkIsRefused is the case the whole thing
// exists for: a turn that keeps asking and keeps being told the same nothing.
// The measured failure it stands for is a reasoning model at effort off,
// applying a wrong fix and then spending 41 round trips defending it. The loop
// already knows that turn has stopped getting anywhere — it refuses the call
// for it — so that is where more thinking is bought.
func TestEffortRisesWhenNearDuplicateWorkIsRefused(t *testing.T) {
	// One search that says something, NoProgressLimit that repeat it, one that
	// is refused instead of run, then one more search and a closing answer.
	const searches = 2 + NoProgressLimit
	turns := append(churn("grep", searches), textTurn("done"))
	h := buildOn(t, localLadder(), turns, func(c *Config) {
		c.Effort, c.EffortCeiling, c.MaxSteps = "low", "high", 100
	})

	var ran int
	registerScripted(t, h, "grep", true, []string{"no matches found"}, &ran)

	out, err := h.loop.Run(context.Background(), userMessage("remove the unused imports"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Stalled != 1 {
		t.Fatalf("Stalled = %d, want 1; this test needs the stall refusal to have fired", out.Stalled)
	}

	got := efforts(h)
	if len(got) != len(turns) {
		t.Fatalf("the provider served %d requests, want %d", len(got), len(turns))
	}
	// The refusal happens on the fifth step, so the first five requests were
	// already assembled at the tier the operator asked for.
	for i := 0; i < searches; i++ {
		if got[i] != "low" {
			t.Fatalf("request %d carried effort %q, want %q — the tier must not rise before "+
				"the turn has been shown to be stuck", i+1, got[i], "low")
		}
	}
	if got[searches] != "medium" {
		t.Fatalf("the request after the stall refusal carried effort %q, want %q — one rung up "+
			"the model's own ladder", got[searches], "medium")
	}
	if out.EffortFrom != "low" || out.EffortTo != "medium" || out.EffortRaised != 1 {
		t.Fatalf("Outcome effort = %q -> %q raised %d times, want low -> medium raised once",
			out.EffortFrom, out.EffortTo, out.EffortRaised)
	}
}

// TestEffortRisesWhenAVerbatimRepeatIsRefused covers the other refusal. Both
// mean the same thing — the turn is going in circles — and a tier that rose for
// one but not the other would depend on which disguise the churn happened to
// wear.
//
// It also pins the ceiling: the second refusal has nowhere left to climb.
func TestEffortRisesWhenAVerbatimRepeatIsRefused(t *testing.T) {
	const attempts = RepeatLimit + 2
	var turns []replay.Turn
	for i := 0; i < attempts; i++ {
		turns = append(turns, callTurn("look", `{}`))
	}
	turns = append(turns, textTurn("done"))

	h := buildOn(t, localLadder(), turns, func(c *Config) {
		c.Effort, c.EffortCeiling, c.MaxSteps = "low", "medium", 100
	})
	var ran int
	registerCounter(t, h, "look", &ran)

	out, err := h.loop.Run(context.Background(), userMessage("go"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Repeated != attempts-RepeatLimit {
		t.Fatalf("Repeated = %d, want %d", out.Repeated, attempts-RepeatLimit)
	}

	got := efforts(h)
	// Steps 1..RepeatLimit ran; step RepeatLimit+1 was the first refusal, so it
	// was assembled before anything was known to be wrong.
	for i := 0; i <= RepeatLimit; i++ {
		if got[i] != "low" {
			t.Fatalf("request %d carried effort %q, want low", i+1, got[i])
		}
	}
	for i := RepeatLimit + 1; i < len(got); i++ {
		if got[i] != "medium" {
			t.Fatalf("request %d carried effort %q, want medium — the ceiling", i+1, got[i])
		}
	}
	if out.EffortTo != "medium" || out.EffortRaised != 1 {
		t.Fatalf("Outcome effort ended at %q after %d rise(s), want medium after 1: the second "+
			"refusal had nowhere left to climb", out.EffortTo, out.EffortRaised)
	}
}

// TestEffortNeverRisesWithoutACeiling is the guard on the default. A harness
// that quietly started spending more than it was told to would be worse than
// one that never escalated at all.
func TestEffortNeverRisesWithoutACeiling(t *testing.T) {
	turns := append(churn("grep", 2+NoProgressLimit), textTurn("done"))
	h := buildOn(t, localLadder(), turns, func(c *Config) {
		c.Effort, c.MaxSteps = "low", 100
	})
	var ran int
	registerScripted(t, h, "grep", true, []string{"no matches found"}, &ran)

	out, err := h.loop.Run(context.Background(), userMessage("go"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Stalled == 0 {
		t.Fatal("this test needs the stall refusal to have fired")
	}
	for i, effort := range efforts(h) {
		if effort != "low" {
			t.Fatalf("request %d carried effort %q with no ceiling configured; want low throughout",
				i+1, effort)
		}
	}
	if out.EffortRaised != 0 || out.EffortTo != "low" {
		t.Fatalf("Outcome reported %d rise(s) ending at %q, want none", out.EffortRaised, out.EffortTo)
	}
}

// TestMechanicalWorkStaysCheap is the measurement that motivated the design:
// reasoning cost 2.6x the tokens and 1.8x the wall clock on a task that needed
// none of it. A turn that keeps getting somewhere must never pay for a tier it
// was not asked for, however long it runs.
func TestMechanicalWorkStaysCheap(t *testing.T) {
	const edits = 12
	var turns []replay.Turn
	var outputs []string
	for i := 0; i < edits; i++ {
		turns = append(turns, callTurn("edit", fmt.Sprintf(`{"site":%d}`, i)))
		outputs = append(outputs, fmt.Sprintf("replaced site %d", i))
	}
	turns = append(turns, textTurn("all 12 sites replaced"))

	h := buildOn(t, localLadder(), turns, func(c *Config) {
		c.Effort, c.EffortCeiling, c.MaxSteps = "low", "high", 100
	})
	var ran int
	registerScripted(t, h, "edit", false, outputs, &ran)

	out, err := h.loop.Run(context.Background(), userMessage("replace the literal at every site"))
	if err != nil {
		t.Fatal(err)
	}
	if ran != edits {
		t.Fatalf("the tool ran %d times, want %d", ran, edits)
	}
	if out.EffortRaised != 0 {
		t.Fatalf("EffortRaised = %d on a turn that never stopped making progress", out.EffortRaised)
	}
	for i, effort := range efforts(h) {
		if effort != "low" {
			t.Fatalf("request %d carried effort %q on mechanical work; want low throughout", i+1, effort)
		}
	}
}

// TestEffortResetsBetweenTurns. A turn that went in circles says nothing about
// the next one, which is the same reason the repeat ledger and the progress
// tracker are rebuilt per turn.
func TestEffortResetsBetweenTurns(t *testing.T) {
	first := append(churn("grep", 2+NoProgressLimit), textTurn("done"))
	turns := append(first, textTurn("second turn"))
	h := buildOn(t, localLadder(), turns, func(c *Config) {
		c.Effort, c.EffortCeiling, c.MaxSteps = "low", "high", 100
	})
	var ran int
	registerScripted(t, h, "grep", true, []string{"no matches found"}, &ran)

	if _, err := h.loop.Run(context.Background(), userMessage("first")); err != nil {
		t.Fatal(err)
	}
	served := len(h.provider.Requests())

	second, err := h.loop.Run(context.Background(), userMessage("second"))
	if err != nil {
		t.Fatal(err)
	}
	if got := efforts(h)[served]; got != "low" {
		t.Fatalf("the second turn opened at effort %q, want low — an escalation is per-turn "+
			"evidence, not a setting the loop keeps", got)
	}
	if second.EffortFrom != "low" || second.EffortTo != "low" || second.EffortRaised != 0 {
		t.Fatalf("second turn effort = %q -> %q raised %d, want low -> low raised 0",
			second.EffortFrom, second.EffortTo, second.EffortRaised)
	}
}

// TestACeilingIsRefusedWhenItCannotBeChecked. A ceiling the harness cannot
// check is not a ceiling that works — it is one that silently never fires, and
// a check that did not run must not report what a check that passed reports.
func TestACeilingIsRefusedWhenItCannotBeChecked(t *testing.T) {
	cases := []struct {
		name       string
		capability llm.Capability
		base       string
		ceiling    string
		want       string
	}{
		{
			name: "a model that declares no reasoning", capability: caps("test-model"),
			base: "low", ceiling: "high", want: "does not support reasoning",
		},
		{
			name: "a ceiling the model does not serve", capability: localLadder(),
			base: "low", ceiling: "extreme", want: "extreme",
		},
		{
			name: "a ceiling at or below where the turn starts", capability: localLadder(),
			base: "high", ceiling: "medium", want: "above",
		},
		{
			name: "a ceiling with nothing to climb from", capability: localLadder(),
			base: "", ceiling: "high", want: "starts at",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := PlanEffort(c.capability, c.base, c.ceiling)
			if err == nil {
				t.Fatalf("PlanEffort(%q -> %q) was accepted", c.base, c.ceiling)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("refusal was %q, which does not mention %q", err, c.want)
			}
		})
	}
}

// TestNoCeilingIsAlwaysServiceable is the other side: leaving the ceiling unset
// must never turn an otherwise working configuration into an error, including
// for a model that cannot reason at all.
func TestNoCeilingIsAlwaysServiceable(t *testing.T) {
	for _, capability := range []llm.Capability{caps("test-model"), localLadder()} {
		plan, err := PlanEffort(capability, "", "")
		if err != nil {
			t.Fatalf("an unset ceiling was refused: %v", err)
		}
		if plan.Escalates() {
			t.Fatal("a plan with no ceiling reported that it escalates")
		}
		if _, ok := plan.Next("low"); ok {
			t.Fatal("a plan with no ceiling offered a rung to climb to")
		}
	}
}

// TestALoopWithAnUncheckableCeilingIsRefused. NewLoop is the last place the
// ceiling can be checked before a turn starts spending, and a provider that
// cannot describe the model is a check that did not run.
func TestALoopWithAnUncheckableCeilingIsRefused(t *testing.T) {
	turns := []replay.Turn{textTurn("done")}
	provider := replay.New(replay.Fixture{
		Provider:     "replay",
		Capabilities: []llm.Capability{localLadder()},
		Turns:        turns,
	})
	_, err := NewLoop(Config{
		Provider: provider, Model: "a-model-the-fixture-does-not-describe",
		Effort: "low", EffortCeiling: "high", MaxSteps: 8,
	}, bus.New(), session.NewLog(), nil)
	if err == nil {
		t.Fatal("a ceiling was accepted for a model the provider cannot describe")
	}
	if !strings.Contains(err.Error(), "high") {
		t.Fatalf("refusal was %q, which does not name the ceiling that could not be checked", err)
	}
}

// twoCallTurn is one assistant message carrying two tool calls, which is how a
// step can both go in circles and get somewhere at the same time.
func twoCallTurn(nameA, argsA, nameB, argsB string) replay.Turn {
	return replay.Turn{
		StopReason: llm.StopToolUse,
		Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCallBlock{ID: llm.CallID("a"), Name: nameA, Arguments: []byte(argsA)},
			llm.ToolCallBlock{ID: llm.CallID("b"), Name: nameB, Arguments: []byte(argsB)},
		}},
	}
}

// TestAStepThatRefusesARepeatAndStillGetsSomewhereIsNotTaxed. The refusal alone
// is not evidence that the turn is stuck: a model that re-asks for one thing
// while genuinely moving the work forward is correcting itself, and charging it
// a higher tier for that would tax exactly the recovery that is wanted.
func TestAStepThatRefusesARepeatAndStillGetsSomewhereIsNotTaxed(t *testing.T) {
	turns := []replay.Turn{
		callTurn("look", `{}`),
		callTurn("look", `{}`),
		callTurn("look", `{}`),
		twoCallTurn("look", `{}`, "fresh", `{"n":1}`),
		textTurn("done"),
	}
	h := buildOn(t, localLadder(), turns, func(c *Config) {
		c.Effort, c.EffortCeiling, c.MaxSteps = "low", "high", 100
	})
	var looked, freshRan int
	registerScripted(t, h, "look", true, []string{"the same answer"}, &looked)
	registerScripted(t, h, "fresh", true, []string{"something new"}, &freshRan)

	out, err := h.loop.Run(context.Background(), userMessage("go"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Repeated != 1 {
		t.Fatalf("Repeated = %d, want 1; this test needs the repeat refusal to have fired", out.Repeated)
	}
	if freshRan != 1 {
		t.Fatalf("the progressing call ran %d times, want 1", freshRan)
	}
	if out.EffortRaised != 0 || out.EffortTo != "low" {
		t.Fatalf("effort rose to %q after %d rise(s) on a step that made progress",
			out.EffortTo, out.EffortRaised)
	}
}
