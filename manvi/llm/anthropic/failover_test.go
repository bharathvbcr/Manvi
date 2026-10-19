package anthropic

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

const (
	primaryKey  = "sk-primary-key-value"
	fallbackKey = "sk-fallback-key-value"

	fallbackWorkspace = "wrkspc_01FallbackWorkspace"

	// creditExhaustedBody is the body the Messages API returns, with a 400, to a key
	// whose organisation has run out of prepaid credit.
	creditExhaustedBody = `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."},"request_id":"req_011credit"}`
)

// seen is one request as the server received it.
type seen struct {
	key, workspace, body string
}

// keyedServer answers each request by the key it carries: answer(key) returns
// the status and body to send. It records every request in arrival order.
func keyedServer(t *testing.T, answer func(key string) (int, string)) (string, func() []seen) {
	t.Helper()
	var mu sync.Mutex
	var log []seen
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := r.Header.Get(APIKeyHeader)
		mu.Lock()
		log = append(log, seen{key: key, workspace: r.Header.Get(WorkspaceHeader), body: string(body)})
		mu.Unlock()
		status, payload := answer(key)
		if status == http.StatusOK {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(server.Close)
	return server.URL, func() []seen {
		mu.Lock()
		defer mu.Unlock()
		return append([]seen(nil), log...)
	}
}

// failoverAdapter is a primary-keyed adapter with the fallback armed. Retries
// are made instant so a retried status does not slow the suite.
func failoverAdapter(t *testing.T, url string, onFailover func()) *Adapter {
	t.Helper()
	adapter := New(url, adaptertest.Secret(primaryKey))
	if err := adapter.SetFallback(Fallback{
		Workspace:  fallbackWorkspace,
		Resolve:    adaptertest.Secret(fallbackKey),
		OnFailover: onFailover,
	}); err != nil {
		t.Fatal(err)
	}
	adapter.client.Retry.BaseDelay = 0
	adapter.fallback.Retry.BaseDelay = 0
	return adapter
}

// TestACreditExhaustedPrimaryFailsOverBeforeAnyContent: the primary key's
// organisation is out of credit, so the API refuses the request with a 400
// before the model sees it. The same request is sent once on the fallback key,
// in the fallback's workspace, and every later request goes straight there.
func TestACreditExhaustedPrimaryFailsOverBeforeAnyContent(t *testing.T) {
	url, requests := keyedServer(t, func(key string) (int, string) {
		if key == primaryKey {
			return http.StatusBadRequest, creditExhaustedBody
		}
		return http.StatusOK, happyStream
	})
	var failovers atomic.Int32
	adapter := failoverAdapter(t, url, func() { failovers.Add(1) })

	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatalf("Stream = %v; a credit-exhausted primary must be retried on the fallback key", err)
	}
	chunks, _, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := adaptertest.TextOf(chunks); got != "Reading the file." {
		t.Errorf("text = %q", got)
	}

	got := requests()
	if len(got) != 2 || got[0].key != primaryKey || got[1].key != fallbackKey {
		t.Fatalf("requests went to %v, want the primary once and then the fallback once", keysOf(got))
	}
	if got[0].body != got[1].body {
		t.Errorf("the fallback was sent a different request:\nprimary:  %s\nfallback: %s", got[0].body, got[1].body)
	}
	if got[0].workspace != "" || got[1].workspace != fallbackWorkspace {
		t.Errorf("workspaces = %q then %q, want none then %q", got[0].workspace, got[1].workspace, fallbackWorkspace)
	}
	if !adapter.FailedOver() || failovers.Load() != 1 {
		t.Errorf("FailedOver = %v after %d notifications, want true after exactly one", adapter.FailedOver(), failovers.Load())
	}

	// Sticky: the primary's credit does not come back mid-session, so asking it
	// again would only add a refused request to every turn.
	stream, err = adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adaptertest.Drain(stream); err != nil {
		t.Fatal(err)
	}
	if got := requests(); len(got) != 3 || got[2].key != fallbackKey {
		t.Fatalf("requests went to %v, want the second turn sent straight to the fallback", keysOf(got))
	}
	if failovers.Load() != 1 {
		t.Errorf("notified %d times, want once", failovers.Load())
	}
}

// TestAMidStreamCreditErrorIsNotReplayed: once content has been handed to the
// caller, re-issuing the request would duplicate it. An error frame that
// arrives after content — whatever it says — settles the turn and is never
// retried on either key.
func TestAMidStreamCreditErrorIsNotReplayed(t *testing.T) {
	midStream := startFrame + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Half an "}}

event: error
data: {"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}

`
	url, requests := keyedServer(t, func(string) (int, string) { return http.StatusOK, midStream })
	adapter := failoverAdapter(t, url, func() { t.Error("failed over on an error that arrived after content") })

	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if err := drainNext(t, stream); err == nil || !strings.Contains(err.Error(), "credit balance") {
		t.Fatalf("stream ended with %v, want the in-stream error reported", err)
	}
	if got := requests(); len(got) != 1 || got[0].key != primaryKey {
		t.Fatalf("requests went to %v, want exactly one, on the primary", keysOf(got))
	}
	if adapter.FailedOver() {
		t.Error("FailedOver = true after a mid-stream error")
	}
}

// TestAPreflightCreditErrorIsNotFailedOver: an error frame after message_start
// but before content is inside a response the API already accepted, and
// message_start means the prompt was metered. Failover is for a request the
// API refused outright, so this is reported, not moved to the other key.
func TestAPreflightCreditErrorIsNotFailedOver(t *testing.T) {
	preflightError := startFrame + `event: error
data: {"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}

`
	url, requests := keyedServer(t, func(string) (int, string) { return http.StatusOK, preflightError })
	adapter := failoverAdapter(t, url, func() { t.Error("failed over on an in-stream error") })

	if _, err := adapter.Stream(adaptertest.Ctx(), request()); err == nil {
		t.Fatal("Stream accepted a response whose stream opened with an error")
	}
	if got := requests(); len(got) != 1 || got[0].key != primaryKey {
		t.Fatalf("requests went to %v, want exactly one, on the primary", keysOf(got))
	}
}

// TestOnlyTheCreditErrorFailsOver: every other refusal is about the request,
// the workspace, or a transient condition the fallback key would not change —
// and a spend limit or a rate limit is a ceiling someone chose, which moving
// to another organisation's key would quietly defeat.
func TestOnlyTheCreditErrorFailsOver(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"rate limited": {http.StatusTooManyRequests,
			`{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"}}`},
		"workspace not found": {http.StatusNotFound,
			"{\"type\":\"error\",\"error\":{\"type\":\"not_found_error\",\"message\":\"Workspace `wrkspc_01X` not found.\"}}"},
		"server error": {http.StatusInternalServerError,
			`{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`},
		"overloaded": {529,
			`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
		"malformed request": {http.StatusBadRequest,
			`{"type":"error","error":{"type":"invalid_request_error","message":"This model does not support assistant message prefill."}}`},
		"spend limit": {http.StatusBadRequest,
			`{"type":"error","error":{"type":"invalid_request_error","message":"You have reached your specified workspace API usage limits."}}`},
		"billing details": {http.StatusPaymentRequired,
			`{"type":"error","error":{"type":"billing_error","message":"There is an issue with your payment method."}}`},
		"credit message under the wrong type": {http.StatusBadRequest,
			`{"type":"error","error":{"type":"permission_error","message":"Your credit balance is too low."}}`},
		"credit message as plain text": {http.StatusBadRequest,
			`Your credit balance is too low to access the Anthropic API.`},
		"credit message on a 403": {http.StatusForbidden,
			`{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low."}}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			url, requests := keyedServer(t, func(string) (int, string) { return tc.status, tc.body })
			adapter := failoverAdapter(t, url, func() { t.Error("failed over") })
			if _, err := adapter.Stream(adaptertest.Ctx(), request()); err == nil {
				t.Fatal("Stream succeeded against a refusing server")
			}
			for _, r := range requests() {
				if r.key != primaryKey {
					t.Fatalf("requests went to %v, want every one on the primary", keysOf(requests()))
				}
			}
			if adapter.FailedOver() {
				t.Error("FailedOver = true")
			}
		})
	}
}

// TestAnExhaustedFallbackIsReportedNotPingPonged: when both organisations are
// out of credit the fallback's refusal is the answer, and the next turn does
// not go back to a primary already known to be empty.
func TestAnExhaustedFallbackIsReportedNotPingPonged(t *testing.T) {
	url, requests := keyedServer(t, func(string) (int, string) { return http.StatusBadRequest, creditExhaustedBody })
	adapter := failoverAdapter(t, url, nil)

	_, err := adapter.Stream(adaptertest.Ctx(), request())
	if err == nil || !strings.Contains(err.Error(), "credit balance") {
		t.Fatalf("Stream = %v, want the fallback's credit refusal", err)
	}
	if strings.Contains(err.Error(), primaryKey) || strings.Contains(err.Error(), fallbackKey) {
		t.Fatalf("the error disclosed a key: %v", err)
	}
	if _, err := adapter.Stream(adaptertest.Ctx(), request()); err == nil {
		t.Fatal("second Stream succeeded")
	}
	if got := keysOf(requests()); strings.Join(got, ",") != primaryKey+","+fallbackKey+","+fallbackKey {
		t.Fatalf("requests went to %v, want primary, fallback, fallback", got)
	}
}

// TestWithoutAFallbackTheCreditErrorIsReturned: an adapter with no second key
// behaves exactly as before — one request, the refusal reported.
func TestWithoutAFallbackTheCreditErrorIsReturned(t *testing.T) {
	url, requests := keyedServer(t, func(string) (int, string) { return http.StatusBadRequest, creditExhaustedBody })
	adapter := New(url, adaptertest.Secret(primaryKey))
	if _, err := adapter.Stream(adaptertest.Ctx(), request()); err == nil || !strings.Contains(err.Error(), "credit balance") {
		t.Fatalf("Stream = %v, want the credit refusal", err)
	}
	if len(requests()) != 1 || adapter.FailedOver() {
		t.Fatalf("requests = %d, FailedOver = %v; want one request and no failover", len(requests()), adapter.FailedOver())
	}
}

// TestConcurrentTurnsFailOverOnce: a fan-out of turns that all hit the empty
// primary at once each retry on the fallback, and the switch is announced once.
func TestConcurrentTurnsFailOverOnce(t *testing.T) {
	url, requests := keyedServer(t, func(key string) (int, string) {
		if key == primaryKey {
			return http.StatusBadRequest, creditExhaustedBody
		}
		return http.StatusOK, happyStream
	})
	var failovers atomic.Int32
	adapter := failoverAdapter(t, url, func() { failovers.Add(1) })

	const turns = 8
	var wg sync.WaitGroup
	errs := make(chan error, turns)
	for range turns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := adapter.Stream(adaptertest.Ctx(), request())
			if err == nil {
				_, _, err = adaptertest.Drain(stream)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent turn failed: %v", err)
		}
	}
	if failovers.Load() != 1 {
		t.Errorf("announced %d failovers, want 1", failovers.Load())
	}
	fallbacks := 0
	for _, r := range requests() {
		if r.key == fallbackKey {
			fallbacks++
		}
	}
	if fallbacks != turns {
		t.Errorf("%d turns reached the fallback, want %d", fallbacks, turns)
	}
}

// TestTheHeaderTimeoutReachesBothKeys: the bound is a property of the adapter,
// so it must not depend on whether it was set before or after the fallback.
func TestTheHeaderTimeoutReachesBothKeys(t *testing.T) {
	before := New("", adaptertest.Secret(primaryKey))
	before.SetResponseHeaderTimeout(7 * time.Second)
	if err := before.SetFallback(Fallback{Resolve: adaptertest.Secret(fallbackKey)}); err != nil {
		t.Fatal(err)
	}
	after := New("", adaptertest.Secret(primaryKey))
	if err := after.SetFallback(Fallback{Resolve: adaptertest.Secret(fallbackKey)}); err != nil {
		t.Fatal(err)
	}
	after.SetResponseHeaderTimeout(7 * time.Second)
	for name, a := range map[string]*Adapter{"set before": before, "set after": after} {
		if a.client.ResponseHeaderTimeout != 7*time.Second || a.fallback.ResponseHeaderTimeout != 7*time.Second {
			t.Errorf("%s: timeouts = %v / %v, want 7s on both", name, a.client.ResponseHeaderTimeout, a.fallback.ResponseHeaderTimeout)
		}
	}
}

// TestAMalformedFallbackWorkspaceIsRefused: the fallback's workspace becomes a
// header exactly as the primary's does, and is held to the same alphabet.
func TestAMalformedFallbackWorkspaceIsRefused(t *testing.T) {
	adapter := New("", adaptertest.Secret(primaryKey))
	if err := adapter.SetFallback(Fallback{Workspace: "wrkspc_1\r\nx-api-key: stolen", Resolve: adaptertest.Secret(fallbackKey)}); err == nil {
		t.Fatal("a malformed fallback workspace was accepted")
	}
	if err := adapter.SetFallback(Fallback{}); err == nil {
		t.Fatal("a fallback with no credential was accepted")
	}
}

func keysOf(requests []seen) []string {
	out := make([]string, len(requests))
	for i, r := range requests {
		out[i] = r.key
	}
	return out
}
