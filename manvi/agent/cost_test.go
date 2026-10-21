package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// costTurns is a two-step turn whose steps straddle a pricing threshold: each
// prompt is under 150 tokens, their sum is over it.
func costTurns() []replay.Turn {
	first := toolTurn("c1", "read", `{}`)
	first.Usage = llm.Usage{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 40, CacheWriteTokens: 30}
	second := textTurn("done")
	second.Usage = llm.Usage{InputTokens: 120, OutputTokens: 20, CacheReadTokens: 70, CacheWriteTokens: 5}
	return []replay.Turn{first, second}
}

func registerRead(t *testing.T, h *harness) {
	t.Helper()
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "read"}, ReadOnly: true,
		Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "x"} },
	}); err != nil {
		t.Fatal(err)
	}
}

// TestATurnCarriesItsCacheWrites. The loop summed input, output, reasoning and
// cache reads and dropped cache writes, so a turn that wrote 35 tokens to the
// provider's cache reported none — and cache writes are the dearest input
// tokens there are.
func TestATurnCarriesItsCacheWrites(t *testing.T) {
	h := build(t, costTurns(), nil)
	registerRead(t, h)
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Usage.CacheWriteTokens != 35 || out.Usage.CacheReadTokens != 110 {
		t.Errorf("usage = %+v, want 35 cache writes and 110 cache reads", out.Usage)
	}
}

// TestATurnIsPricedOneStepAtATime. A price table can switch rates on the size
// of one request's prompt. Pricing the turn's summed usage instead would put
// two short requests on the long-prompt rate, so the loop prices each response
// as it arrives and adds the charges.
func TestATurnIsPricedOneStepAtATime(t *testing.T) {
	var priced []llm.Usage
	price := func(u llm.Usage) (int64, error) {
		priced = append(priced, u)
		rate := int64(1)
		if u.InputTokens > 150 {
			rate = 10 // the long-prompt rate a summed usage would wrongly trip
		}
		return int64(u.InputTokens+u.OutputTokens) * rate, nil
	}
	h := build(t, costTurns(), func(c *Config) { c.Price = price })
	registerRead(t, h)
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(priced) != 2 {
		t.Fatalf("priced %d usages, want one per step", len(priced))
	}
	if want := int64(110 + 140); out.CostNanoUSD != want || out.UnpricedSteps != 0 {
		t.Errorf("cost = %d with %d unpriced, want %d with none", out.CostNanoUSD, out.UnpricedSteps, want)
	}
}

// TestAStepThatCannotBePricedIsCountedNotZeroed. A step the table cannot price
// adds nothing to the cost, and a cost that silently omitted it would read as
// the whole bill. The count is what lets a reader say "at least".
func TestAStepThatCannotBePricedIsCountedNotZeroed(t *testing.T) {
	calls := 0
	price := func(u llm.Usage) (int64, error) {
		calls++
		if calls == 2 {
			return 0, errors.New("no price")
		}
		return 7, nil
	}
	h := build(t, costTurns(), func(c *Config) { c.Price = price })
	registerRead(t, h)
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if out.CostNanoUSD != 7 || out.UnpricedSteps != 1 {
		t.Errorf("cost = %d with %d unpriced, want 7 with 1", out.CostNanoUSD, out.UnpricedSteps)
	}

	unpriced := build(t, costTurns(), nil)
	registerRead(t, unpriced)
	out, err = unpriced.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if out.CostNanoUSD != 0 || out.UnpricedSteps != 2 {
		t.Errorf("with no price table: cost = %d with %d unpriced, want 0 with every step unpriced",
			out.CostNanoUSD, out.UnpricedSteps)
	}
}

// stepEnds decodes the payload of every step/end in the log. A step/end with no
// payload fails the test: the loop must say what each step spent, even when the
// answer is nothing.
func stepEnds(t *testing.T, h *harness) []session.StepEndData {
	t.Helper()
	var out []session.StepEndData
	for _, e := range h.log.Events() {
		if e.Type != session.StepEnd {
			continue
		}
		if len(e.Data) == 0 {
			t.Fatalf("step/end seq %d carries no payload; per-step usage was dropped", e.Seq)
		}
		var d session.StepEndData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("step/end seq %d: %v", e.Seq, err)
		}
		out = append(out, d)
	}
	return out
}

// TestEachStepEndCarriesItsOwnUsageAndCharge. Per-step usage was computed to
// fill the turn's Outcome and then lost: the log kept only the sum, so a run
// that blew its budget could not say which step did it.
func TestEachStepEndCarriesItsOwnUsageAndCharge(t *testing.T) {
	price := func(u llm.Usage) (int64, error) { return int64(u.InputTokens + u.OutputTokens), nil }
	turns := costTurns()
	h := build(t, turns, func(c *Config) { c.Price = price })
	registerRead(t, h)
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	ends := stepEnds(t, h)
	if len(ends) != 2 {
		t.Fatalf("%d step/end events, want 2", len(ends))
	}
	var sum int64
	for i, d := range ends {
		if d.Usage != turns[i].Usage {
			t.Errorf("step %d usage = %+v, want the fixture's %+v", i+1, d.Usage, turns[i].Usage)
		}
		if !d.Priced {
			t.Errorf("step %d not marked priced", i+1)
		}
		sum += d.CostNanoUSD
	}
	if ends[0].CostNanoUSD != 110 || ends[1].CostNanoUSD != 140 {
		t.Errorf("charges = %d, %d; want 110, 140", ends[0].CostNanoUSD, ends[1].CostNanoUSD)
	}
	if sum != out.CostNanoUSD {
		t.Errorf("step charges sum to %d, outcome says %d; the one pricing path must agree with itself", sum, out.CostNanoUSD)
	}
}

// TestAnUnpricedStepEndSaysSo. A zero charge and an unpriced step are different
// facts, and the record keeps them apart.
func TestAnUnpricedStepEndSaysSo(t *testing.T) {
	calls := 0
	price := func(llm.Usage) (int64, error) {
		calls++
		if calls == 2 {
			return 0, errors.New("no price")
		}
		return 7, nil
	}
	h := build(t, costTurns(), func(c *Config) { c.Price = price })
	registerRead(t, h)
	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	ends := stepEnds(t, h)
	if len(ends) != 2 || !ends[0].Priced || ends[0].CostNanoUSD != 7 {
		t.Fatalf("steps = %+v, want the first priced at 7", ends)
	}
	if ends[1].Priced || ends[1].CostNanoUSD != 0 {
		t.Errorf("second step = %+v, want unpriced with no charge", ends[1])
	}

	bare := build(t, costTurns(), nil)
	registerRead(t, bare)
	if _, err := bare.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	for i, d := range stepEnds(t, bare) {
		if d.Priced {
			t.Errorf("step %d priced with no price table", i+1)
		}
	}
}

// TestAStepClosedByAnEarlyExitCarriesZeroUsage. closeOpenBoundaries ends a step
// whose response never arrived; its usage is zero and it is not priced, and
// the log still holds a well-formed step/end.
func TestAStepClosedByAnEarlyExitCarriesZeroUsage(t *testing.T) {
	h := build(t, []replay.Turn{{Err: "boom"}}, func(c *Config) {
		c.Price = func(llm.Usage) (int64, error) { return 5, nil }
	})
	if _, err := h.loop.Run(context.Background(), userMsg("go")); err == nil {
		t.Fatal("the provider error was swallowed")
	}
	ends := stepEnds(t, h)
	if len(ends) != 1 {
		t.Fatalf("%d step/end events, want the one closeOpenBoundaries wrote", len(ends))
	}
	if ends[0] != (session.StepEndData{NoResponse: true}) {
		t.Errorf("step/end = %+v, want zero usage, no charge, marked no-response", ends[0])
	}
}

// TestUsageDoesNotLeakIntoTheNextStep. The stash is per step: a step that ends
// without a response must not inherit the previous step's numbers.
func TestUsageDoesNotLeakIntoTheNextStep(t *testing.T) {
	first := toolTurn("c1", "read", `{}`)
	first.Usage = llm.Usage{InputTokens: 100, OutputTokens: 10}
	h := build(t, []replay.Turn{first, {Err: "boom"}}, func(c *Config) {
		c.Price = func(llm.Usage) (int64, error) { return 5, nil }
	})
	registerRead(t, h)
	if _, err := h.loop.Run(context.Background(), userMsg("go")); err == nil {
		t.Fatal("the provider error was swallowed")
	}
	ends := stepEnds(t, h)
	if len(ends) != 2 {
		t.Fatalf("%d step/end events, want 2", len(ends))
	}
	if ends[1] != (session.StepEndData{NoResponse: true}) {
		t.Errorf("second step/end = %+v, want it empty rather than the first step's usage", ends[1])
	}
}

// TestDraftIsTheFirstRequestTheTurnWouldSend. A fan-out is admitted on the size
// of this request, so it must carry what the first real request carries — the
// system prompt, the tool schemas the turn is offered, the user's instruction —
// and must not run or record anything.
func TestDraftIsTheFirstRequestTheTurnWouldSend(t *testing.T) {
	h := build(t, costTurns(), func(c *Config) { c.ReadOnly = true })
	registerRead(t, h)
	if err := h.tools.Register(tools.Tool{
		Schema:  llm.ToolSchema{Name: "write"},
		Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "x"} },
	}); err != nil {
		t.Fatal(err)
	}
	req := h.loop.Draft(userMsg("go"))
	if req.Model != "test-model" || req.System != "you are a builder" || req.MaxTokens != 1024 {
		t.Errorf("draft = %+v; want the loop's model, system prompt and output bound", req)
	}
	if len(req.Messages) != 1 || req.Messages[0].Text() != "go" {
		t.Errorf("draft messages = %+v; want the one user instruction", req.Messages)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "read" {
		t.Errorf("draft tools = %+v; a read-only turn is offered only the read-only tool", req.Tools)
	}
	if n := h.log.Len(); n != 0 {
		t.Errorf("drafting recorded %d events; it must record nothing", n)
	}
}
