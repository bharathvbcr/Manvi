package budget_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
)

// TestHeadroomIsTheLimitLessEverythingCommitted. A fan-out is admitted against
// this number, so it has to count what is in flight as well as what is spent:
// a reservation another session holds right now is money this one cannot use.
func TestHeadroomIsTheLimitLessEverythingCommitted(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	got, err := l.Headroom()
	if err != nil || got != 1_000_000_000 {
		t.Fatalf("Headroom of an empty ledger = %d, %v; want the whole limit", got, err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the body first: the server notices the client hanging up only
		// once it has, and Close waits for this handler.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: l}
	defer holdAttempt(p, request())()

	// Wait for the attempt to be admitted and held in flight.
	var held int64
	for held == 0 {
		held = snapshot(t, l).InFlightNanoUSD()
	}
	got, err = l.Headroom()
	if err != nil || got != 1_000_000_000-held {
		t.Errorf("Headroom with %d in flight = %d, %v; want %d", held, got, err, 1_000_000_000-held)
	}
}

// TestEstimateMatchesWhatTheGateReserves. Estimate is the number a fan-out is
// admitted on, and the gate's reservation is the number the ledger then
// charges. If they were computed two ways they would drift, and admission
// would approve fan-outs whose first requests the cap then refuses.
func TestEstimateMatchesWhatTheGateReserves(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the body first: the server notices the client hanging up only
		// once it has, and Close waits for this handler.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: l}

	req := request()
	req.System = strings.Repeat("a system prompt long enough to move the bound ", 400)
	estimate, err := p.Estimate(req)
	if err != nil {
		t.Fatal(err)
	}

	defer holdAttempt(p, req)()
	var reserved int64
	for reserved == 0 {
		reserved = snapshot(t, l).InFlightNanoUSD()
	}
	// The gate sizes the prompt from the adapter's wire body and Estimate from
	// the request's own encoding; the two differ by the envelope, which the
	// prompt overhead already dwarfs.
	if lo, hi := reserved*95/100, reserved*105/100; estimate < lo || estimate > hi {
		t.Errorf("Estimate = %s, gate reserved %s; want within 5%%", budget.USD(estimate), budget.USD(reserved))
	}
}

// TestEstimateRefusesWhatTheGateWouldRefuse. A model the gate cannot price is
// not a free child: it is one whose request will be refused, and admission
// must hear that now rather than read it as a reservation of zero.
func TestEstimateRefusesWhatTheGateWouldRefuse(t *testing.T) {
	p := &budget.Provider{Inner: anthropic.New("http://127.0.0.1:0", adaptertest.Secret("fixture-key")), Ledger: ledger(t, 1_000_000_000)}
	req := request()
	req.Model = "not-a-model"
	if _, err := p.Estimate(req); err == nil {
		t.Fatal("Estimate priced a model the provider publishes no price for")
	}
}

// TestAMeteredProviderStillPublishesItsPrices. Wrapping a provider in the cap
// must not hide its price table from anything that prices usage after the
// fact — the per-model cost a run reports is computed from the same table.
func TestAMeteredProviderStillPublishesItsPrices(t *testing.T) {
	inner := anthropic.New("http://127.0.0.1:0", adaptertest.Secret("fixture-key"))
	var p llm.Provider = &budget.Provider{Inner: inner, Ledger: ledger(t, 1_000_000_000)}
	pricer, ok := p.(budget.Pricer)
	if !ok {
		t.Fatal("budget.Provider does not forward PriceOf")
	}
	got, ok := pricer.PriceOf(model)
	want, _ := inner.PriceOf(model)
	if !ok || got != want {
		t.Errorf("PriceOf(%s) = %+v, %v; want the inner table %+v", model, got, ok, want)
	}
}

// holdAttempt sends req through p and leaves its attempt in flight until the
// returned function is called, which cancels it and waits until the ledger
// has recorded the outcome.
func holdAttempt(p *budget.Provider, req llm.Request) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if stream, err := p.Stream(ctx, req); err == nil {
			_ = stream.Close()
		}
	}()
	return func() { cancel(); <-done }
}
