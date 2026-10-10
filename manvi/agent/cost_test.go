package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
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
