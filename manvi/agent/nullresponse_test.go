package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"manvi/core/bus"
	"manvi/llm"
	"manvi/llm/replay"
	"manvi/session"
	"manvi/tools"
)

func nullTurn() replay.Turn {
	// What the live endpoint actually sends: status completed, no output step,
	// zero output tokens.
	return replay.Turn{
		Message:    llm.Message{Role: llm.RoleAssistant},
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 90, OutputTokens: 0},
	}
}

func answerTurn(text string) replay.Turn {
	return replay.Turn{
		Message: llm.Message{Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock{Text: text}}},
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 90, OutputTokens: 12},
	}
}

func loopOver(t *testing.T, turns []replay.Turn) (*Loop, *session.Log) {
	t.Helper()
	provider := replay.New(replay.Fixture{
		Provider: "replay",
		Capabilities: []llm.Capability{{
			Provider: "replay", Model: "m", ContextWindow: 32768,
			MaxOutputTokens: 4096, SupportsTools: true, SupportsStreaming: true,
		}},
		Turns: turns,
	})
	models := llm.NewRegistry()
	if err := models.Register(provider); err != nil {
		t.Fatal(err)
	}
	log := session.NewLog()
	loop, err := NewLoop(Config{
		Provider: provider, Registry: models, Model: "m",
		SystemPrompt: "p", MaxSteps: 20,
	}, bus.New(), log, tools.NewRegistry(bus.New()))
	if err != nil {
		t.Fatal(err)
	}
	return loop, log
}

func run(t *testing.T, loop *Loop) Outcome {
	t.Helper()
	out, err := loop.Run(context.Background(), llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAnEmptyResponseIsAskedForAgain.
//
// Measured against the live Gemini endpoint on 2026-08-19: 5 of 21 responses in
// one benchmark completed having generated nothing — status "completed",
// total_output_tokens 0, a lone content-free thought step. That ended four of
// five scenarios on a turn that had done real work up to that point.
//
// Nothing was produced, so nothing is harmed by asking again — the same
// reasoning that retries an error frame delivered before any content.
func TestAnEmptyResponseIsAskedForAgain(t *testing.T) {
	loop, log := loopOver(t, []replay.Turn{
		nullTurn(), nullTurn(), answerTurn("here is the answer"),
	})
	out := run(t, loop)

	if out.FinalEmpty {
		t.Error("the turn was reported as having no answer despite one arriving after the retries")
	}
	if got := strings.TrimSpace(out.Final.Text()); got != "here is the answer" {
		t.Errorf("final = %q", got)
	}
	// The retries are recorded, not swallowed.
	var retried int
	for _, e := range log.Events() {
		if e.Type == session.NullResponseRetried {
			retried++
			var data session.NullResponseData
			if err := json.Unmarshal(e.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Of != maxNullRetries {
				t.Errorf("recorded budget = %d, want %d", data.Of, maxNullRetries)
			}
		}
	}
	if retried != 2 {
		t.Errorf("%d null response(s) recorded, want 2 — a silent retry is a cost nobody can explain", retried)
	}
}

// TestAPersistentlyEmptyProviderStillEndsTheTurn.
//
// The retry must be bounded and must not convert a provider that will not
// answer into a clean result. After the budget, the turn ends empty and says so
// — which is exit status 4 and the honest outcome.
func TestAPersistentlyEmptyProviderStillEndsTheTurn(t *testing.T) {
	turns := make([]replay.Turn, 0, maxNullRetries+2)
	for i := 0; i < maxNullRetries+2; i++ {
		turns = append(turns, nullTurn())
	}
	loop, _ := loopOver(t, turns)
	out := run(t, loop)

	if !out.FinalEmpty {
		t.Error("a provider that never answered was reported as having finished")
	}
	if out.Steps > maxNullRetries+1 {
		t.Errorf("spent %d steps on empty responses; the retry is meant to be bounded", out.Steps)
	}
}

// TestAResponseThatSaidSomethingIsNeverRetried is the bound in the other
// direction.
//
// "Did the model answer?" and "did the model produce anything?" are different
// questions. FinalEmpty asks the first and discounts reasoning, because
// reasoning is stripped before a caller sees it. The retry asks the second, and
// must not: reasoning was generated and billed, so re-asking would throw away
// real work and pay for the replacement — and it would pre-empt the turn's own
// terminal checkpoint, which exists to handle a model that thought and had
// nothing to say.
func TestAResponseThatSaidSomethingIsNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		name string
		turn replay.Turn
	}{
		{"text", answerTurn("done")},
		{"tokens but no text", replay.Turn{
			Message:    llm.Message{Role: llm.RoleAssistant},
			StopReason: llm.StopEndTurn,
			Usage:      llm.Usage{OutputTokens: 7},
		}},
		{"reasoning and nothing else", replay.Turn{
			Message: llm.Message{Role: llm.RoleAssistant,
				Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "thinking"}}},
			StopReason: llm.StopEndTurn,
		}},
		{"reasoning tokens only", replay.Turn{
			Message:    llm.Message{Role: llm.RoleAssistant},
			StopReason: llm.StopEndTurn,
			Usage:      llm.Usage{ReasoningTokens: 30},
		}},
	} {
		loop, log := loopOver(t, []replay.Turn{tc.turn})
		run(t, loop)
		for _, e := range log.Events() {
			if e.Type == session.NullResponseRetried {
				t.Errorf("%s: a response that produced output was retried", tc.name)
			}
		}
	}
}
