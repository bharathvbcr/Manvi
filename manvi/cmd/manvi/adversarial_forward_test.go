package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// Attack 8: N children forwarding concurrently into one JSON sink. Every line
// must be a whole JSON object attributed to its own child, no step.usage or
// turn frame may appear, and the race detector must stay quiet (run with -race
// where the toolchain links it).
func TestAdversarialConcurrentChildrenForwardIntoOneSink(t *testing.T) {
	var out bytes.Buffer
	sink := ui.NewJSONSink(&out, nil)
	const children, rounds = 16, 100

	var wg sync.WaitGroup
	for c := 0; c < children; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			label := fmt.Sprintf("child-%d", c)
			observe := forwardChildEvents(sink, label)
			feed := func(typ session.Type, payload any) {
				raw, _ := json.Marshal(payload)
				observe(session.Event{Seq: 1, Type: typ, Data: raw})
			}
			for i := 0; i < rounds; i++ {
				feed(session.StepEnd, session.StepEndData{Usage: llm.Usage{InputTokens: 7}, Priced: true})
				feed(session.VerifyReport, session.VerifyReportData{Verdict: "passed", Source: label})
				feed(session.MalformedToolCall, session.MalformedCallData{ToolCallID: "c", Reason: label})
				feed(session.UserMessage, session.MessageData{})
				feed(session.TurnEnd, nil)
			}
		}(c)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if want := children * rounds * 2; len(lines) != want {
		t.Fatalf("%d lines, want %d (verify.report + diagnostic per round)", len(lines), want)
	}
	for _, l := range lines {
		var e ui.Event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("torn line %q: %v", l, err)
		}
		if e.Kind == ui.KindStepUsage || e.Kind == ui.KindTurnStart || e.Kind == ui.KindTurnEnd {
			t.Fatalf("forwarded %s from a child", e.Kind)
		}
		if e.Kind == ui.KindVerifyReport && e.Agent != e.Source {
			t.Fatalf("attribution crossed between children: agent %q, source %q", e.Agent, e.Source)
		}
	}
}
