package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// randomUsage is a usage with every summed field populated.
func randomUsage(r *rand.Rand) llm.Usage {
	return llm.Usage{
		InputTokens: r.Intn(5000), OutputTokens: 1 + r.Intn(500),
		ReasoningTokens: r.Intn(50), CacheReadTokens: r.Intn(900), CacheWriteTokens: r.Intn(90),
	}
}

func randomScript(r *rand.Rand) []replay.Turn {
	n := 1 + r.Intn(7)
	var turns []replay.Turn
	for i := 0; i < n; i++ {
		var t replay.Turn
		switch r.Intn(6) {
		case 0:
			t = toolTurn("c"+string(rune('a'+i)), "read", `{}`)
		case 1:
			t = textTurn("answer")
		case 2: // a null response: nothing, zero tokens, retried by the loop
			t = replay.Turn{Message: llm.Message{Role: llm.RoleAssistant}, StopReason: llm.StopEndTurn}
		case 3:
			t = malformedTurn("read", "cut off")
		case 4:
			t = replay.Turn{Err: "boom"}
		default:
			t = toolTurn("c"+string(rune('a'+i)), "read", `{}`)
		}
		if t.Err == "" && len(t.Message.Content) > 0 || len(t.Malformed) > 0 {
			t.Usage = randomUsage(r)
		}
		turns = append(turns, t)
	}
	return turns
}

func sumUsage(a, b llm.Usage) llm.Usage {
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.ReasoningTokens += b.ReasoningTokens
	a.CacheReadTokens += b.CacheReadTokens
	a.CacheWriteTokens += b.CacheWriteTokens
	return a
}

// TestAdversarialStepUsageSumsToTheOutcome. Attack 2/3: over random scripts of
// tool, text, null, malformed and provider-error steps, with prices that fail,
// go negative or overflow, the step/end records of a turn must sum to the
// turn's own Outcome: usage, charge, and the count of unpriced steps.
func TestAdversarialStepUsageSumsToTheOutcome(t *testing.T) {
	for seed := int64(0); seed < 400; seed++ {
		r := rand.New(rand.NewSource(seed))
		var price func(llm.Usage) (int64, error)
		switch r.Intn(5) {
		case 0: // no table
		case 1:
			price = func(u llm.Usage) (int64, error) { return int64(u.InputTokens + 3*u.OutputTokens), nil }
		case 2:
			price = func(u llm.Usage) (int64, error) {
				if u.InputTokens%2 == 0 {
					return 0, errors.New("no price")
				}
				return 11, nil
			}
		case 3: // negative charges are refused
			price = func(u llm.Usage) (int64, error) {
				if u.OutputTokens%3 == 0 {
					return -4, nil
				}
				return 9, nil
			}
		default: // overflow: the second priced step would wrap, and must be refused
			price = func(u llm.Usage) (int64, error) { return math.MaxInt64/2 + 1, nil }
		}
		h := build(t, randomScript(r), func(c *Config) { c.Price = price; c.MaxSteps = 40 })
		registerRead(t, h)

		// Two turns on the same loop: the second must not inherit the first's stash.
		for turn := 0; turn < 2; turn++ {
			before := len(h.log.Events())
			out, _ := h.loop.Run(context.Background(), userMsg("go"))

			var usage llm.Usage
			var cost int64
			unpriced, ends := 0, 0
			for _, e := range h.log.Events()[before:] {
				if e.Type != session.StepEnd {
					continue
				}
				ends++
				if len(e.Data) == 0 {
					t.Fatalf("seed %d: step/end seq %d has no payload", seed, e.Seq)
				}
				var d session.StepEndData
				if err := json.Unmarshal(e.Data, &d); err != nil {
					t.Fatalf("seed %d: %v", seed, err)
				}
				usage = sumUsage(usage, d.Usage)
				if d.Priced {
					cost += d.CostNanoUSD
				} else {
					if d.CostNanoUSD != 0 {
						t.Errorf("seed %d: unpriced step carries a charge %d", seed, d.CostNanoUSD)
					}
					unpriced++
				}
			}
			ou := out.Usage
			ou.OutputTokensPerSecond, ou.PromptTokensPerSecond = 0, 0
			if usage != ou {
				t.Fatalf("seed %d turn %d: step usages sum to %+v, outcome says %+v", seed, turn, usage, ou)
			}
			if cost != out.CostNanoUSD {
				t.Fatalf("seed %d turn %d: step charges sum to %d, outcome %d", seed, turn, cost, out.CostNanoUSD)
			}
			if unpriced < out.UnpricedSteps {
				t.Fatalf("seed %d turn %d: %d unpriced step/end < outcome.UnpricedSteps %d", seed, turn, unpriced, out.UnpricedSteps)
			}
			_ = ends
		}
	}
}

// TestAdversarialUnpricedCountSeparatesNeverRespondedFromUnpriced. An unpriced
// step/end that has the zero payload is indistinguishable from "this step got
// no response" and from "this step had zero usage and no table covered it".
// Outcome.UnpricedSteps counts only the latter, so counting step/end with
// Priced=false overstates it. A consumer reconciling the two (the stated
// invariant of ft-aa63) cannot, from the log alone.
func TestAdversarialUnpricedCountSeparatesNeverRespondedFromUnpriced(t *testing.T) {
	price := func(u llm.Usage) (int64, error) { return 1, nil }
	h := build(t, []replay.Turn{toolTurn("c1", "read", `{}`), {Err: "boom"}}, func(c *Config) { c.Price = price })
	registerRead(t, h)
	out, _ := h.loop.Run(context.Background(), userMsg("go"))
	unpriced := 0
	for _, e := range h.log.Events() {
		if e.Type != session.StepEnd {
			continue
		}
		var d session.StepEndData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		if !d.Priced {
			unpriced++
		}
	}
	if unpriced != out.UnpricedSteps {
		t.Errorf("count of unpriced step/end = %d, Outcome.UnpricedSteps = %d: the failed step's zero record "+
			"reads as an unpriced step", unpriced, out.UnpricedSteps)
	}
}

// TestAdversarialCancelAfterResponseKeepsUsage. Attack 4: the context is
// cancelled by a tool handler after the response has been absorbed. The step's
// usage must still be in the log, and the next turn's early-exit step must not
// carry it again.
func TestAdversarialCancelAfterResponseKeepsUsage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	first := toolTurn("c1", "read", `{}`)
	first.Usage = llm.Usage{InputTokens: 123, OutputTokens: 45}
	h := build(t, []replay.Turn{first, textTurn("second turn")}, func(c *Config) {
		c.Price = func(llm.Usage) (int64, error) { return 3, nil }
	})
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "read"}, ReadOnly: true,
		Handler: func(context.Context, tools.Call) tools.Result { cancel(); return tools.Result{Text: "x"} },
	}); err != nil {
		t.Fatal(err)
	}
	out, _ := h.loop.Run(ctx, userMsg("go"))
	ends := stepEnds(t, h)
	var got llm.Usage
	for _, d := range ends {
		got = sumUsage(got, d.Usage)
	}
	if got.InputTokens != 123 || got.OutputTokens != 45 || out.Usage.InputTokens != 123 {
		t.Fatalf("step usages = %+v, outcome %+v; the cancelled step's spend must be in the log", got, out.Usage)
	}

	// A fresh turn: nothing stale may be emitted by it.
	before := len(h.log.Events())
	h.loop.Run(context.Background(), userMsg("again"))
	for _, e := range h.log.Events()[before:] {
		if e.Type == session.StepEnd {
			var d session.StepEndData
			_ = json.Unmarshal(e.Data, &d)
			if d.Usage.InputTokens == 123 {
				t.Errorf("a later step/end repeats the earlier step's usage: %+v", d)
			}
		}
	}
}
