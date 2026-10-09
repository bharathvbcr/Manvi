package budget_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
	"github.com/bharathvbcr/Manvi/manvi/llm/gemini"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
)

const model = "claude-opus-5-5"

func request() llm.Request {
	return llm.Request{Model: model, MaxTokens: 100, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "test"}}}}}
}

// turnSSE is one answered turn: 1000 prompt tokens of which 800 were read
// from cache, 50 output tokens.
const turnSSE = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":200,"cache_read_input_tokens":800}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":50}}

event: message_stop
data: {"type":"message_stop"}

`

// turnCost is turnSSE priced at Opus 5.5 rates: 200 fresh at 4000, 800 cached
// at 200, 50 output at 20000.
const turnCost = 200*4_000 + 800*200 + 50*20_000

func ledger(t *testing.T, limit int64) *budget.Ledger {
	t.Helper()
	l, err := budget.Open(filepath.Join(t.TempDir(), "ledger.json"), limit)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func snapshot(t *testing.T, l *budget.Ledger) budget.Snapshot {
	t.Helper()
	s, err := l.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func drain(t *testing.T, s llm.Stream) llm.Response {
	t.Helper()
	for {
		_, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Response()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBudgetedAnthropicSettlesOnlyTheAttemptThatAnswered(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, turnSSE)
	}))
	defer server.Close()
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: l}
	stream, err := p.Stream(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, stream)
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, l)
	if calls.Load() != 2 || s.Settled != 1 || s.SpentNanoUSD != turnCost || s.RetainedAttempts != 1 || s.RetainedNanoUSD <= 0 || len(s.InFlight) != 0 {
		t.Fatalf("calls=%d snapshot=%+v, want one retained 503 and one settled turn of %d", calls.Load(), s, turnCost)
	}
}

func TestExhaustedBudgetRefusesBeforeAnythingIsSent(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: ledger(t, 1_000)}
	if _, err := p.Stream(context.Background(), request()); !errors.Is(err, budget.ErrExhausted) {
		t.Fatalf("error = %v, want ErrExhausted", err)
	}
	if calls.Load() != 0 {
		t.Fatal("a request the budget could not cover reached the server")
	}
}

func TestCancellationAfterAdmissionRetainsTheReservation(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel(); <-release }))
	defer server.Close()
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: l}
	_, err := p.Stream(ctx, request())
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if s := snapshot(t, l); s.RetainedAttempts != 1 || s.RetainedNanoUSD <= 0 || len(s.InFlight) != 0 {
		t.Fatalf("a cancelled sent attempt was refunded: %+v", s)
	}
}

func TestStreamClosedBeforeItsResponseRetainsTheReservation(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, turnSSE)
	}))
	defer server.Close()
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: l}
	stream, err := p.Stream(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if s := snapshot(t, l); s.RetainedAttempts != 1 || s.Settled != 0 || len(s.InFlight) != 0 {
		t.Fatalf("an abandoned stream was not retained: %+v", s)
	}
}

func TestCancelledStreamDoesNotSettleUsage(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, turnSSE)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: l}
	stream, err := p.Stream(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := stream.Response(); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if s := snapshot(t, l); s.Settled != 0 || s.RetainedAttempts != 1 {
		t.Fatalf("cancelled stream settled usage: %+v", s)
	}
}

func TestUngatedProviderIsRefusedBeforeItConsumesATurn(t *testing.T) {
	inner := replay.New(replay.Fixture{Turns: []replay.Turn{{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "fixture"}}}}}})
	p := &budget.Provider{Inner: inner, Ledger: ledger(t, 1_000_000_000)}
	if _, err := p.Stream(context.Background(), request()); err == nil {
		t.Fatal("ungated provider accepted")
	}
	stream, err := inner.Stream(context.Background(), request())
	if err != nil {
		t.Fatal("budget wrapper already consumed the provider's turn: ", err)
	}
	defer stream.Close()
}

func TestUnpricedProviderOrModelIsRefusedBeforeSending(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	// Gemini routes through the gate but publishes no prices.
	unpriced := &budget.Provider{Inner: gemini.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: ledger(t, 1_000_000_000)}
	req := request()
	req.Model = "gemini-3.7-flash"
	if _, err := unpriced.Stream(context.Background(), req); err == nil {
		t.Fatal("unpriced provider accepted")
	}
	unknown := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: ledger(t, 1_000_000_000)}
	req = request()
	req.Model = "claude-not-a-model"
	if _, err := unknown.Stream(context.Background(), req); err == nil {
		t.Fatal("unpriced model accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("an unmeterable request reached the server")
	}
}

func TestProviderRequiresAContext(t *testing.T) {
	p := &budget.Provider{Inner: anthropic.New("", adaptertest.Secret("fixture-key")), Ledger: ledger(t, 1_000_000_000)}
	var none context.Context
	if _, err := p.Stream(none, request()); err == nil {
		t.Fatal("missing context admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Stream(ctx, request()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// deadlineProbe is a gated, priced provider that records the context it is
// handed and makes no HTTP attempt.
type deadlineProbe struct{ hasDeadline bool }

func (*deadlineProbe) Name() string { return "probe" }
func (*deadlineProbe) Capability(string) (llm.Capability, bool) {
	return llm.Capability{ContextWindow: 1_000, MaxOutputTokens: 100}, true
}
func (*deadlineProbe) AttemptGateSupported() bool { return true }
func (*deadlineProbe) PriceOf(string) (budget.Prices, bool) {
	return budget.Prices{Rates: budget.Rates{Input: 1, Output: 1, CacheWrite: 1, CacheRead: 1}}, true
}
func (d *deadlineProbe) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	_, d.hasDeadline = ctx.Deadline()
	return replay.New(replay.Fixture{Turns: []replay.Turn{{Message: llm.Message{Role: llm.RoleAssistant}}}}).Stream(ctx, req)
}

// A long reasoning turn routinely streams for minutes. The metering wrapper
// must not put a deadline on a stream the caller did not.
func TestMeteringImposesNoDeadlineOnTheStream(t *testing.T) {
	probe := &deadlineProbe{}
	p := &budget.Provider{Inner: probe, Ledger: ledger(t, 1_000_000_000)}
	stream, err := p.Stream(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if probe.hasDeadline {
		t.Fatal("the budget wrapper bounded the stream with a deadline of its own")
	}
}

func TestMeteringKeepsTheInnerReplayRule(t *testing.T) {
	var p llm.Provider = &budget.Provider{Inner: anthropic.New("", adaptertest.Secret("fixture-key")), Ledger: ledger(t, 1)}
	r, ok := p.(llm.ReasoningReplayer)
	if !ok {
		t.Fatal("the metered provider hides the inner provider's replay rule")
	}
	if !r.ReplayableOn(model, model) || r.ReplayableOn(model, "claude-sonnet-5-5") || r.ReplayableOn("", "") {
		t.Fatal("the metered provider answers differently from the anthropic rule")
	}
}

func TestARateLimitedAttemptIsNotCountedAsSpend(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("retry-after", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, turnSSE)
	}))
	defer server.Close()
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: l}
	stream, err := p.Stream(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, stream)
	stream.Close()
	s := snapshot(t, l)
	if calls.Load() != 2 || s.RefusedAttempts != 1 || s.RetainedAttempts != 0 || s.CommittedNanoUSD() != turnCost {
		t.Fatalf("calls=%d snapshot=%+v; a 429 is refused before the model runs and must cost nothing", calls.Load(), s)
	}
}

// A turn cut off after message_start has a known prompt. Retaining its whole
// reservation — priced from the request's byte length at the cache-write rate
// — overstated a cached turn by two orders of magnitude.
func TestACutShortTurnIsBoundedByItsReportedPrompt(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	const cut = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":200,"cache_read_input_tokens":800}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, cut)
	}))
	defer server.Close()
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: l}
	stream, err := p.Stream(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := stream.Next(); err != nil {
			break
		}
	}
	if _, err := stream.Response(); err == nil {
		t.Fatal("a truncated stream produced a response")
	}
	stream.Close()
	// 200 fresh at 4000 + 800 cached at 200, plus all 100 allowed output tokens at 20000.
	const bound = 200*4_000 + 800*200 + 100*20_000
	if s := snapshot(t, l); s.RetainedNanoUSD != bound || s.RetainedAttempts != 1 || s.Settled != 0 {
		t.Fatalf("retained %+v, want the prompt-bounded %d", s, bound)
	}
}
