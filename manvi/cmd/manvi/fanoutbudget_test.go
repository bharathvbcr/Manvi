package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// pricedReplay is a scripted provider that publishes a price table, so a test
// can drive the real per-step pricing path without a network.
type pricedReplay struct {
	*replay.Provider
	prices budget.Prices
}

func (p pricedReplay) PriceOf(string) (budget.Prices, bool) { return p.prices, true }

// rates differ per provider so a split that misattributes a model shows up as
// a wrong cost, not only as a wrong label.
var testRates = map[string]budget.Rates{
	"alpha": {Input: 3, Output: 15, CacheWrite: 4, CacheRead: 1},
	"beta":  {Input: 1, Output: 5, CacheWrite: 2, CacheRead: 1},
}

// pricedRunner is usageRunner on a named, priced provider whose two steps
// also write to the cache.
func pricedRunner(t *testing.T, meter *subAgentMeter, name string) *subAgentRunner {
	t.Helper()
	inner := replay.New(replay.Fixture{
		Provider: name,
		Capabilities: []llm.Capability{{
			Provider: name, Model: "m", ContextWindow: 32768,
			MaxOutputTokens: 4096, SupportsTools: true, SupportsStreaming: true,
		}},
		Turns: withCacheWrites(usageTurns()),
	})
	provider := pricedReplay{Provider: inner, prices: budget.Prices{Rates: testRates[name]}}
	models := llm.NewRegistry()
	if err := models.Register(provider); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry(bus.New())
	if err := registry.Register(tools.Tool{
		Schema:  llm.ToolSchema{Name: "noop", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "{}"} },
	}); err != nil {
		t.Fatal(err)
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: provider, models: models, model: "m",
		registry: registry, systemPrompt: "child", meter: meter,
	})
	return r
}

func withCacheWrites(turns []replay.Turn) []replay.Turn {
	turns[0].Usage.CacheWriteTokens = 20
	turns[1].Usage.CacheWriteTokens = 7
	return turns
}

// childCost is what one child of pricedRunner costs on a provider: each step
// priced on its own, through the same Charge the cap uses.
func childCost(t *testing.T, name string) int64 {
	t.Helper()
	prices := budget.Prices{Rates: testRates[name]}
	var total int64
	for _, turn := range withCacheWrites(usageTurns()) {
		c, err := prices.Charge(turn.Usage)
		if err != nil {
			t.Fatal(err)
		}
		total += c
	}
	return total
}

// TestThirtyTwoChildrenOnTwoModelsMeterExactly.
//
// Thirty-two children write the meter at once, half on each of two models. The
// totals per model must be exactly the sum of that model's children — tokens,
// cache writes and cost — with no update lost, and the overall total must be
// the sum of the two. Run it under -race for the concurrency half of the claim.
func TestThirtyTwoChildrenOnTwoModelsMeterExactly(t *testing.T) {
	const children = 32
	meter := &subAgentMeter{}
	var wg sync.WaitGroup
	for i := range children {
		name := "alpha"
		if i%2 == 1 {
			name = "beta"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := pricedRunner(t, meter, name)
			out, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{Label: "w", Prompt: "do it"})
			if err != nil {
				t.Errorf("child on %s: %v", name, err)
				return
			}
			if out.Usage.Model != name+"/m" || out.Usage.CacheWriteTokens != 27 || out.Usage.CostNanoUSD != childCost(t, name) {
				t.Errorf("child on %s reported %+v; want model %s/m, 27 cache writes, cost %d",
					name, out.Usage, name, childCost(t, name))
			}
		}()
	}
	wg.Wait()

	byModel := meter.ByModel()
	if len(byModel) != 2 {
		t.Fatalf("meter has %d models, want 2: %+v", len(byModel), byModel)
	}
	for _, got := range byModel {
		name := strings.TrimSuffix(got.Model, "/m")
		want := devcouncil.SubAgentUsage{
			Model: got.Model, InputTokens: 312 * children / 2, OutputTokens: 24 * children / 2,
			ReasoningTokens: 8 * children / 2, CacheReadTokens: 110 * children / 2,
			CacheWriteTokens: 27 * children / 2, CostNanoUSD: childCost(t, name) * children / 2,
		}
		if got != want {
			t.Errorf("meter for %s = %+v, want %+v", got.Model, got, want)
		}
	}
	total := meter.Total()
	if total.InputTokens != 312*children || total.CostNanoUSD != (childCost(t, "alpha")+childCost(t, "beta"))*children/2 {
		t.Errorf("meter total = %+v; want the sum of both models", total)
	}
}

// TestTheUsageLineSplitsByModel. The line both faces print is built from the
// parent's own share and the children's, merged per model, and an unpriced
// share is said to be unpriced rather than shown as $0.00.
func TestTheUsageLineSplitsByModel(t *testing.T) {
	own := devcouncil.SubAgentUsage{Model: "anthropic/big", InputTokens: 1000, OutputTokens: 100, CostNanoUSD: 50_000_000}
	children := []devcouncil.SubAgentUsage{
		{Model: "anthropic/big", InputTokens: 500, OutputTokens: 50, CostNanoUSD: 25_000_000},
		{Model: "anthropic/small", InputTokens: 4000, OutputTokens: 400, CacheWriteTokens: 300, CostNanoUSD: 10_000_000},
		{Model: "local/qwen", InputTokens: 9000, OutputTokens: 900, UnpricedSteps: 3},
	}
	e := usageEvent(own, children)
	if e.InputTokens != 14500 || e.OutputTokens != 1450 || e.CostNanoUSD != 85_000_000 || e.UnpricedSteps != 3 {
		t.Errorf("totals = %d in / %d out, %d nano-USD, %d unpriced", e.InputTokens, e.OutputTokens, e.CostNanoUSD, e.UnpricedSteps)
	}
	if len(e.ByModel) != 3 || e.ByModel[0].Model != "anthropic/big" || e.ByModel[0].InputTokens != 1500 || e.ByModel[0].CostNanoUSD != 75_000_000 {
		t.Errorf("by model = %+v; want three rows with the parent and its same-model children merged", e.ByModel)
	}
	for _, want := range []string{
		"cost at least $0.09 (3 step(s) unpriced) across 3 models:",
		"anthropic/big · 1500 in / 150 out · $0.08",
		"anthropic/small · 4000 in / 400 out · cache 0 read / 300 written · $0.01",
		"local/qwen · 9000 in / 900 out · unpriced (3 step(s), no price table)",
	} {
		if !strings.Contains(e.Text, want) {
			t.Errorf("usage text %q lacks %q", e.Text, want)
		}
	}
	if strings.Contains(e.Text, "$0.00") {
		t.Errorf("usage text %q prints $0.00 for a share nobody priced", e.Text)
	}

	solo := usageEvent(devcouncil.SubAgentUsage{Model: "anthropic/big", InputTokens: 10, OutputTokens: 1, CostNanoUSD: 20_000_000}, nil)
	if solo.Text != "cost $0.02 · anthropic/big" || len(solo.ByModel) != 1 {
		t.Errorf("single-model line = %q, %+v", solo.Text, solo.ByModel)
	}
}

// meteredRunner attaches a runner whose children run on the real Anthropic
// adapter behind the real spend cap, against a server that answers nothing
// until the test ends.
func meteredRunner(t *testing.T, limit int64) (*subAgentRunner, *budget.Ledger) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	ledger, err := budget.Open(filepath.Join(t.TempDir(), "ledger.json"), limit)
	if err != nil {
		t.Fatal(err)
	}
	provider := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: ledger}
	models := llm.NewRegistry()
	if err := models.Register(provider); err != nil {
		t.Fatal(err)
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: provider, models: models, model: "claude-opus-5-5",
		registry: tools.NewRegistry(bus.New()), systemPrompt: "child", meter: &subAgentMeter{},
	})
	return r, ledger
}

// TestAdmissionSizesEachMeteredChildFromTheCapItDrawsOn. The runner names the
// ledger the children draw on and what each first request will reserve — the
// same figure the gate would reserve — and reads nothing for a child it cannot
// prepare.
func TestAdmissionSizesEachMeteredChildFromTheCapItDrawsOn(t *testing.T) {
	r, ledger := meteredRunner(t, 1_000_000_000_000)
	reqs := []devcouncil.SubAgentRequest{
		{Label: "a", Prompt: "short"},
		{Label: "b", Prompt: strings.Repeat("a longer instruction ", 2000)},
		{Label: "c", Prompt: " "}, // refused by the runner itself; reserves nothing
	}
	capb, reserves, err := r.AdmitSubAgents(reqs)
	if err != nil || capb == nil {
		t.Fatalf("AdmitSubAgents = %v, %v; want the ledger the children draw on", capb, err)
	}
	if reserves[0] <= 0 || reserves[1] <= reserves[0] || reserves[2] != 0 {
		t.Errorf("reserves = %v; want a, then a dearer b for its longer prompt, then nothing for c", reserves)
	}
	headroom, err := capb.Headroom()
	if err != nil || headroom != ledger.Limit() {
		t.Errorf("Headroom = %d, %v; want the untouched limit %d", headroom, err, ledger.Limit())
	}
	if capb.Render(1_230_000_000) != "$1.23" {
		t.Errorf("Render = %q, want the cap's own dollars", capb.Render(1_230_000_000))
	}
}

// TestAChildRefusedByTheCapIsRecognisedAsExhaustion. The pool stops starting
// children when a started one comes back refused by the cap, which it can only
// do if the refusal survives the runner's and the loop's wrapping.
func TestAChildRefusedByTheCapIsRecognisedAsExhaustion(t *testing.T) {
	r, _ := meteredRunner(t, 1) // one nano-dollar: no request fits
	_, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{Label: "w", Prompt: "do it"})
	if err == nil {
		t.Fatal("a child ran under a cap that covers nothing")
	}
	if !(ledgerBudget{}).Exhausted(err) {
		t.Errorf("error %q does not unwrap to budget.ErrExhausted; the pool would keep starting children", err)
	}
	if (ledgerBudget{}).Exhausted(errors.New("provider went away")) {
		t.Error("an unrelated failure was read as the cap running out")
	}
}

// TestATurnReportsOnlyWhatItsOwnChildrenSpent. The TUI's meter accumulates
// across the whole session, so each turn's line is the difference against the
// snapshot taken before it ran: a model that spent nothing this turn drops out,
// and one that spent again reports only the new spend.
func TestATurnReportsOnlyWhatItsOwnChildrenSpent(t *testing.T) {
	meter := &subAgentMeter{}
	meter.add(devcouncil.SubAgentUsage{Model: "a/m", InputTokens: 100, CostNanoUSD: 10})
	meter.add(devcouncil.SubAgentUsage{Model: "b/m", InputTokens: 50, UnpricedSteps: 1})
	before := meter.ByModel()

	meter.add(devcouncil.SubAgentUsage{Model: "a/m", InputTokens: 7, CacheWriteTokens: 3, CostNanoUSD: 2})
	meter.add(devcouncil.SubAgentUsage{Model: "c/m", InputTokens: 9, UnpricedSteps: 2})

	got := meter.Since(before)
	want := []devcouncil.SubAgentUsage{
		{Model: "a/m", InputTokens: 7, CacheWriteTokens: 3, CostNanoUSD: 2},
		{Model: "c/m", InputTokens: 9, UnpricedSteps: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("Since = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Since[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
