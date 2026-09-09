package budget

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/gemini"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
)

func budgetRequest() llm.Request {
	return llm.Request{Model: "gemini-3.7-flash", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "test"}}}}, MaxTokens: 10}
}

const budgetSSE = `event: interaction.created
data: {"event_type":"interaction.created","interaction":{"status":"in_progress"}}

event: step.start
data: {"event_type":"step.start","index":0,"step":{"type":"model_output","content":[{"type":"text","text":"done"}]}}

event: step.stop
data: {"event_type":"step.stop","index":0}

event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"completed","usage":{"total_input_tokens":10,"total_output_tokens":5,"total_thought_tokens":2}}}

`

func TestBudgetedGeminiSettlesOnlyTheLastSuccessfulHTTPAttempt(t *testing.T) {
	ledger := testLedger(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, budgetSSE)
	}))
	defer server.Close()
	p := &Provider{Inner: gemini.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: ledger}
	stream, err := p.Stream(context.Background(), budgetRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		_, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stream.Response(); err != nil {
		t.Fatal(err)
	}
	attempts := ledger.Snapshot().Attempts
	if calls.Load() != 2 || len(attempts) != 2 || attempts[0].State != "rejected" || attempts[0].ReservedNanoUSD != 200 || attempts[1].State != "settled" || attempts[1].ChargedNanoUSD != 24 {
		t.Fatalf("attempts=%+v calls=%d", attempts, calls.Load())
	}
}
func TestCancellationAfterHTTPAdmissionRetainsReservation(t *testing.T) {
	ledger := testLedger(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel(); <-release }))
	defer server.Close()
	p := &Provider{Inner: gemini.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: ledger}
	_, err := p.Stream(ctx, budgetRequest())
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	attempts := ledger.Snapshot().Attempts
	if len(attempts) != 1 || attempts[0].ReservedNanoUSD != 200 || attempts[0].State != "unknown" {
		t.Fatalf("cancelled sent attempt refunded: %+v", attempts)
	}
}
func TestBudgetRejectsUngatedProviderBeforeItConsumesTurn(t *testing.T) {
	inner := replay.New(replay.Fixture{Turns: []replay.Turn{{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "fixture"}}}}}})
	p := &Provider{Inner: inner, Ledger: testLedger(t)}
	if _, err := p.Stream(context.Background(), budgetRequest()); err == nil {
		t.Fatal("ungated provider accepted")
	}
	stream, err := inner.Stream(context.Background(), budgetRequest())
	if err != nil {
		t.Fatal("budget wrapper already invoked ungated provider", err)
	}
	defer stream.Close()
}
func TestProviderRequiresContextAndCancellationGuardsSettlement(t *testing.T) {
	p := &Provider{Inner: gemini.New("", adaptertest.Secret("fixture-key")), Ledger: testLedger(t)}
	if _, err := p.Stream(nil, budgetRequest()); err == nil {
		t.Fatal("missing context admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Stream(ctx, budgetRequest()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	inner := replay.New(replay.Fixture{Turns: []replay.Turn{{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "fixture"}}}, Usage: llm.Usage{InputTokens: 1}}}})
	fixture, err := inner.Stream(context.Background(), budgetRequest())
	if err != nil {
		t.Fatal(err)
	}
	settled := false
	s := &budgetStream{ctx: ctx, inner: fixture, cancel: cancel, settle: func(llm.Usage) error { settled = true; return nil }}
	if _, err := s.Response(); !errors.Is(err, context.Canceled) || settled {
		t.Fatal("cancelled stream settled usage", err)
	}
}
