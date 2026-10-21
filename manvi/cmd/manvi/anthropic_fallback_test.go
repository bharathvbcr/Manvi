package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
)

const fallbackTurn = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"on the fallback"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`

// TestBuildProviderArmsTheFallbackFromTheEnvironment: with both keys in the
// environment, the adapter buildProvider returns moves a credit-refused
// request to the fallback key in the fallback's workspace — not the primary's
// — and says so on the notes writer without naming either key.
func TestBuildProviderArmsTheFallbackFromTheEnvironment(t *testing.T) {
	const primary, fallback = "sk-ant-fixture-primary-e2e", "sk-ant-fixture-fallback-e2e"
	var mu sync.Mutex
	var keys, workspaces []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		keys = append(keys, r.Header.Get(anthropic.APIKeyHeader))
		workspaces = append(workspaces, r.Header.Get(anthropic.WorkspaceHeader))
		mu.Unlock()
		if r.Header.Get(anthropic.APIKeyHeader) == primary {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, fallbackTurn)
	}))
	t.Cleanup(server.Close)

	providersEnv(t, primary, "wrkspc_primary01")
	t.Setenv(credentials.AnthropicFallbackKeyEnv, fallback)
	t.Setenv(credentials.AnthropicFallbackWorkspaceEnv, "wrkspc_fallback01")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	t.Setenv(envAnthropicBudget, "")

	var notes bytes.Buffer
	provider, err := buildProvider(context.Background(), anthropic.Name, registry(t), credentials.NewResolver(), &notes, "")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), llm.Request{
		Model:     "claude-opus-5",
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Stream = %v, want the request moved to the fallback key", err)
	}
	defer func() { _ = stream.Close() }()
	for {
		if _, err := stream.Next(); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("stream ended with %v", err)
			}
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(keys, ",") != primary+","+fallback {
		t.Fatalf("requests carried keys %v, want the primary then the fallback", keys)
	}
	if !slices.Equal(workspaces, []string{"wrkspc_primary01", "wrkspc_fallback01"}) {
		t.Errorf("workspaces = %v, want each key sent with its own", workspaces)
	}
	if !strings.Contains(notes.String(), credentials.AnthropicFallbackKeyEnv) {
		t.Errorf("notes = %q, want the switch announced by variable name", notes.String())
	}
	if strings.Contains(notes.String(), primary) || strings.Contains(notes.String(), fallback) {
		t.Errorf("notes disclosed a key: %q", notes.String())
	}
}

// TestAMeteredFailoverChargesOnlyTheFallback: under a spend cap, the primary's
// credit refusal is an attempt the budget gate admitted and must release — the
// API refused it before the model saw it — and the turn is charged once, from
// the fallback's reported usage.
func TestAMeteredFailoverChargesOnlyTheFallback(t *testing.T) {
	const primary, fallback = "sk-ant-fixture-primary-metered", "sk-ant-fixture-fallback-metered"
	const turn = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":1000}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":100}}

event: message_stop
data: {"type":"message_stop"}

`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get(anthropic.APIKeyHeader) == primary {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, turn)
	}))
	t.Cleanup(server.Close)

	providersEnv(t, primary, "")
	t.Setenv(credentials.AnthropicFallbackKeyEnv, fallback)
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	ledgerPath := filepath.Join(t.TempDir(), "anthropic.json")
	t.Setenv(envAnthropicLedger, ledgerPath)
	t.Setenv(envAnthropicBudget, "200")

	p, err := buildProvider(context.Background(), anthropic.Name, registry(t), credentials.NewResolver(), io.Discard, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*budget.Provider); !ok {
		t.Fatalf("provider = %T, want it metered", p)
	}
	stream, err := p.Stream(context.Background(), llm.Request{
		Model: "claude-haiku-5-5", MaxTokens: 1000,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream = %v, want the metered request moved to the fallback", err)
	}
	for {
		if _, err := stream.Next(); err != nil {
			break
		}
	}
	if _, err := stream.Response(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := budget.Inspect(context.Background(), ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	// 1000 input at $0.10/MTok plus 100 output at $0.50/MTok, once.
	if s.SpentNanoUSD != 1000*100+100*500 || s.RetainedNanoUSD != 0 || s.RetainedAttempts != 0 {
		t.Errorf("ledger = %+v, want one turn's usage spent and nothing retained", s)
	}
	if s.RefusedAttempts != 1 {
		t.Errorf("refused attempts = %d, want the primary's credit refusal released as unbilled", s.RefusedAttempts)
	}
}
