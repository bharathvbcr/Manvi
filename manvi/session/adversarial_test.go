package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// typicalTurn is a prompt, `steps` tool-using steps and a final answer, with
// the step/end payload either absent (the old shape) or as the loop writes it.
func typicalTurn(t *testing.T, l *Log, steps int, payload bool) {
	t.Helper()
	mustAppend(t, l, TurnStart, nil)
	mustAppend(t, l, UserMessage, MessageData{Message: user(strings.Repeat("p", 200))})
	for s := 0; s < steps; s++ {
		mustAppend(t, l, StepStart, nil)
		mustAppend(t, l, AssistantMessage, MessageData{Message: assistant(strings.Repeat("a", 150))})
		mustAppend(t, l, ToolResult, ToolResultData{ToolCallID: "c", Text: strings.Repeat("r", 2000)})
		var data any
		if payload {
			data = StepEndData{
				Usage:       llm.Usage{InputTokens: 48213, OutputTokens: 412, CacheReadTokens: 41000, CacheWriteTokens: 1200, ReasoningTokens: 96, OutputTokensPerSecond: 61.25, PromptTokensPerSecond: 1450.5},
				CostNanoUSD: 1234567, Priced: true,
			}
		}
		mustAppend(t, l, StepEnd, data)
	}
	mustAppend(t, l, TurnEnd, nil)
}

func encodedSize(t *testing.T, l *Log) int {
	t.Helper()
	per, err := encodeEvents(l.Events())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range per {
		n += len(p) + 1
	}
	return n
}

// Attack 5 (quantified): the step/end payload counts toward the session byte
// budget. Measured on a turn of three steps that each carry a 2 KB tool result; see the logged figures. A bare step grows by more than
// 40 percent of what the step used to cost, so the same 8 MiB holds fewer
// turns than before. Logged, and failed over 10 percent, with a 2 KB tool result in every step per turn.
func TestAdversarialStepEndPayloadShareOfTheByteBudget(t *testing.T) {
	old, now := NewLog(), NewLog()
	typicalTurn(t, old, 3, false)
	typicalTurn(t, now, 3, true)
	a, b := encodedSize(t, old), encodedSize(t, now)

	var bare, loaded int
	for _, e := range now.Events() {
		if e.Type == StepEnd {
			loaded = len(e.Data)
		}
	}
	for _, e := range old.Events() {
		if e.Type == StepEnd {
			bare = len(e.Data)
		}
	}
	t.Logf("turn of 3 short steps: %d bytes before, %d after (+%d, %.1f%%); step/end payload %d bytes (was %d)",
		a, b, b-a, 100*float64(b-a)/float64(a), loaded, bare)

	// The same budget keeps how many whole turns, before and after.
	const budget = 64 << 10
	keep := func(payload bool) int {
		l := NewLog()
		for i := 0; i < 400; i++ {
			typicalTurn(t, l, 3, payload)
		}
		_, dropped, _, _, err := trimToBudget(l.Events(), budget)
		if err != nil {
			t.Fatal(err)
		}
		return 400 - dropped
	}
	before, after := keep(false), keep(true)
	t.Logf("under a %d byte budget: %d turns kept before, %d after", budget, before, after)
	if float64(b-a)/float64(a) > 0.10 {
		t.Errorf("per-step usage grew a turn by %.1f%%, evicting earlier turns sooner (%d -> %d kept)",
			100*float64(b-a)/float64(a), before, after)
	}
}

// Attack 5: shapes of step/end and prompt_assembled payloads from other
// writers (older, newer, damaged). RestoreLog and the store round trip must
// accept all of them and derive the same history; decoding is by the reader.
func TestAdversarialRestoreAcceptsEveryStepEndShape(t *testing.T) {
	shapes := map[string]string{
		"null":           `null`,
		"empty object":   `{}`,
		"unknown fields": `{"usage":{"input_tokens":1,"future_tokens":9},"cost_nano_usd":1,"priced":true,"model":"m","latency_ms":12}`,
		"wrong types":    `{"usage":"x","priced":"yes"}`,
		"scalar":         `42`,
		"huge int":       `{"usage":{"input_tokens":99999999999999999999999}}`,
	}
	for name, raw := range shapes {
		l := NewLog()
		mustAppend(t, l, TurnStart, nil)
		mustAppend(t, l, UserMessage, MessageData{Message: user("u")})
		mustAppend(t, l, StepStart, nil)
		mustAppend(t, l, AssistantMessage, MessageData{Message: assistant("a")})
		mustAppend(t, l, StepEnd, json.RawMessage(raw))
		mustAppend(t, l, TurnEnd, nil)
		want := derived(t, l)

		store := NewStore(t.TempDir())
		id := newID(t)
		if _, err := store.Save(id, l); err != nil {
			t.Fatalf("%s: Save: %v", name, err)
		}
		loaded, _, err := store.Load(id)
		if err != nil {
			t.Fatalf("%s: Load: %v", name, err)
		}
		if derived(t, loaded) != want {
			t.Errorf("%s: history changed across a round trip", name)
		}
		if _, err := RestoreLog(l.Events()); err != nil {
			t.Errorf("%s: RestoreLog refused it: %v", name, err)
		}
	}
}
