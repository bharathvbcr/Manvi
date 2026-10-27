package serve

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agent"
)

// The calibrator learns the ratio of what the server counted to what the
// harness estimated for the same request. chat.prepare used to remember its own
// calibrated After as "the estimate" and feed that back as the raw side of the
// next observation, so the correction was applied twice and the ratio settled at
// the square root of the truth: a server counting half the raw estimate read as
// 0.707, and compaction fired earlier than the window required.
func TestChatPrepareCalibrationConvergesToTheTrueRatio(t *testing.T) {
	wire := []WireMessage{{Role: "user", Text: strings.Repeat("alpha beta gamma ", 400)}}
	raw := agent.CountRequestTokens("sys prompt", nil, toNeutralMessages(wire, nil))
	observed := raw / 2
	msgs, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}

	srv := New(&strings.Builder{}, hostOpts())
	ratios := make([]float64, 0, 12)
	for i := 0; i < 12; i++ {
		res, perr := srv.prepare(json.RawMessage(fmt.Sprintf(
			`{"session_id":"cal","system":"sys prompt","context_window":1000000,"observed_prompt_tokens":%d,"messages":%s}`, observed, msgs)))
		if perr != nil {
			t.Fatal(perr)
		}
		prepared, ok := res.(PrepareResult)
		if !ok {
			t.Fatalf("prepare returned %T, want a PrepareResult", res)
		}
		ratios = append(ratios, prepared.CalibrationRatio)
	}
	t.Logf("raw estimate=%d, server count=%d (true ratio 0.5); ratio after each prepare: %.3f", raw, observed, ratios)

	// The agent loop's rule, which this plane must agree with: Observe(raw, counted).
	var loop agent.Calibrator
	for i := 0; i < 12; i++ {
		loop.Observe(raw, observed)
	}
	last := ratios[len(ratios)-1]
	if math.Abs(last-0.5) > 0.01 {
		t.Errorf("serve calibrator settled at %.3f against a true ratio of 0.5 (the loop's rule gives %.3f)", last, loop.Ratio())
	}
	if math.Abs(last-loop.Ratio()) > 0.01 {
		t.Errorf("serve calibrator %.3f disagrees with the agent loop's %.3f on the same observations", last, loop.Ratio())
	}
}

// The estimate paired with a server count is the one for what the host sends:
// the history with this plan's shortenings applied, not the history it started
// from. With compaction in play the raw estimate must follow the steps.
func TestChatPrepareCalibratesAgainstTheCompactedRequest(t *testing.T) {
	d := newDriver(t)
	params := PrepareParams{
		SessionID:     "cal-compact",
		System:        "sys",
		Messages:      bulkyHistory(10, 60),
		ContextWindow: 8192,
	}
	first := d.callPrepare(params)
	if len(first.Steps) == 0 {
		t.Fatal("fixture is not over budget")
	}
	shortened := map[string]string{}
	for _, s := range first.Steps {
		shortened[s.ToolCallID] = s.Text
	}
	sent := append([]WireMessage(nil), params.Messages...)
	for i := range sent {
		if text, ok := shortened[sent[i].ToolCallID]; ok && sent[i].Role == "tool" {
			sent[i].Text = text
		}
	}
	rawSent := agent.CountRequestTokens("sys", nil, toNeutralMessages(sent, nil))

	// The server counts exactly half of what was really sent.
	for i := 0; i < 12; i++ {
		params.ObservedPromptTokens = rawSent / 2
		params.Messages = sent
		got := d.callPrepare(params)
		if i == 11 && math.Abs(got.CalibrationRatio-0.5) > 0.01 {
			t.Fatalf("ratio = %.3f after 12 observations of a server counting half the request that was sent", got.CalibrationRatio)
		}
	}
}
