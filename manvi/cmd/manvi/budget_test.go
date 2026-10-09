package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
)

func TestParseUSDIsExactAndStrict(t *testing.T) {
	for raw, want := range map[string]int64{
		"200": 200_000_000_000, "49.50": 49_500_000_000, "0.000000001": 1, " 7 ": 7_000_000_000, "1000000": 1_000_000_000_000_000,
	} {
		got, err := parseUSD(raw)
		if err != nil || got != want {
			t.Errorf("parseUSD(%q) = %d, %v; want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "0", "0.0", "-5", "+5", "1e3", "Inf", "NaN", "$200", "1,000", "1.0000000001", "1000000.01", "12x", ".5"} {
		if got, err := parseUSD(raw); err == nil {
			t.Errorf("parseUSD(%q) = %d, want a refusal", raw, got)
		}
	}
}

func anthropicFixture(t *testing.T, baseURL string) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-fixture-budget")
	t.Setenv("ANTHROPIC_BASE_URL", baseURL)
}

func registry(t *testing.T) *flags.Registry {
	t.Helper()
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestAnthropicIsUnmeteredUntilACapIsSet(t *testing.T) {
	anthropicFixture(t, "")
	t.Setenv(envAnthropicBudget, "")
	p, err := buildProvider(anthropic.Name, registry(t), credentials.NewResolver(), io.Discard, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*anthropic.Adapter); !ok {
		t.Fatalf("provider = %T, want the bare adapter", p)
	}
}

func TestACapMetersEveryAnthropicSession(t *testing.T) {
	const turn = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":1000}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":100}}

event: message_stop
data: {"type":"message_stop"}

`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, turn)
	}))
	defer server.Close()
	anthropicFixture(t, server.URL)
	ledgerPath := filepath.Join(t.TempDir(), "nested", "anthropic.json")
	t.Setenv(envAnthropicLedger, ledgerPath)
	t.Setenv(envAnthropicBudget, "200")

	p, err := buildProvider(anthropic.Name, registry(t), credentials.NewResolver(), io.Discard, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*budget.Provider); !ok {
		t.Fatalf("provider = %T, want it metered", p)
	}
	req := llm.Request{Model: "claude-haiku-5-5", MaxTokens: 1000, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}}}
	stream, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := stream.Next(); err != nil {
			break
		}
	}
	if _, err := stream.Response(); err != nil {
		t.Fatal(err)
	}
	stream.Close()

	var out strings.Builder
	if err := showBudget(&out); err != nil {
		t.Fatal(err)
	}
	// 1000 input at $0.10/MTok plus 100 output at $0.50/MTok is $0.00015.
	for _, want := range []string{ledgerPath, "limit      $200.00", "spent      $0.00 over 1 requests", "remaining  $200.00"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("budget report missing %q:\n%s", want, out.String())
		}
	}
	s, err := budget.Inspect(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if s.SpentNanoUSD != 1000*100+100*500 {
		t.Fatalf("spent = %d nano-USD", s.SpentNanoUSD)
	}
}

func TestAnUnusableCapRefusesToRunUnmetered(t *testing.T) {
	anthropicFixture(t, "")
	for name, env := range map[string][2]string{
		"bad_amount":    {"two hundred", filepath.Join(t.TempDir(), "l.json")},
		"relative_path": {"200", "anthropic.json"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(envAnthropicBudget, env[0])
			t.Setenv(envAnthropicLedger, env[1])
			if p, err := buildProvider(anthropic.Name, registry(t), credentials.NewResolver(), io.Discard, ""); err == nil {
				t.Fatalf("built %T from an unusable cap", p)
			}
		})
	}
	t.Run("corrupt_ledger", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "l.json")
		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(envAnthropicBudget, "200")
		t.Setenv(envAnthropicLedger, path)
		if p, err := buildProvider(anthropic.Name, registry(t), credentials.NewResolver(), io.Discard, ""); err == nil {
			t.Fatalf("built %T over a corrupt ledger", p)
		}
	})
}

func TestBudgetReportWithoutALedgerOrLimit(t *testing.T) {
	t.Setenv(envAnthropicBudget, "")
	t.Setenv(envAnthropicLedger, filepath.Join(t.TempDir(), "none.json"))
	var out strings.Builder
	if err := showBudget(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "limit      none") || !strings.Contains(out.String(), "no metered request yet") {
		t.Fatalf("report:\n%s", out.String())
	}
}

// The probe spends real money on every run, so it uses the cheapest current
// model — and that model must be one a capped session can price.
func TestProbeModelIsServedAndPriced(t *testing.T) {
	m := probeModel(anthropic.Name)
	if _, ok := anthropic.Capability(m); !ok {
		t.Fatalf("probe model %q is not served", m)
	}
	if _, ok := anthropic.PriceOf(m); !ok {
		t.Fatalf("probe model %q is not priced", m)
	}
}

// A managed Claude agent is a separate process speaking to the API itself.
// Handed the metered key while a cap is set, it would spend from the account
// with nothing recording it.
func TestManagedClaudeAgentsDoNotInheritTheMeteredKey(t *testing.T) {
	t.Setenv(envAnthropicBudget, "")
	if got := meteredAnthropicEnv(); got != nil {
		t.Fatalf("withheld %q with no cap set", got)
	}
	t.Setenv(envAnthropicBudget, "200")
	if got := meteredAnthropicEnv(); len(got) != 1 || got[0] != "ANTHROPIC_API_KEY" {
		t.Fatalf("withheld %q, want the key the meter resolves", got)
	}
}
