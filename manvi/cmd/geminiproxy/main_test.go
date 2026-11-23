package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProxyRecordsBodyAndHeaderNamesWithoutHeaderValues(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "secret-key-value" {
			t.Errorf("upstream missing forwarded key header")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: interaction.created\ndata: {\"ok\":true}\n\n")
	}))
	defer upstream.Close()

	capture, err := os.CreateTemp(t.TempDir(), "capture")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{capture: capture, upstream: upstream.URL, client: upstream.Client()}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1beta/interactions?alt=sse", strings.NewReader(`{"model":"gemini-3.7-flash"}`))
	req.Header.Set("x-goog-api-key", "secret-key-value")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "interaction.created") {
		t.Fatalf("body not forwarded: %q", rec.Body.String())
	}

	logged, err := os.ReadFile(capture.Name())
	if err != nil {
		t.Fatal(err)
	}
	text := string(logged)
	if !strings.Contains(text, `{"model":"gemini-3.7-flash"}`) {
		t.Fatalf("request body missing from capture: %s", text)
	}
	if !strings.Contains(text, "X-Goog-Api-Key") && !strings.Contains(text, "x-goog-api-key") {
		t.Fatalf("header names missing from capture: %s", text)
	}
	if strings.Contains(text, "secret-key-value") {
		t.Fatal("capture wrote a header value")
	}
}

func TestProxyReportsUpstreamTransportFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	upstream.Close()

	capture, err := os.CreateTemp(t.TempDir(), "capture")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{capture: capture, upstream: upstream.URL, client: &http.Client{}}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1beta/interactions", strings.NewReader("{}")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
	logged, err := os.ReadFile(capture.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "TRANSPORT ERROR") {
		t.Fatalf("capture missing transport error: %s", logged)
	}
}

func TestEnvOrPrefersTheProcessOverTheDefault(t *testing.T) {
	t.Setenv("PROXY_ADDR", "127.0.0.1:9")
	if got := envOr("PROXY_ADDR", "127.0.0.1:8899"); got != "127.0.0.1:9" {
		t.Fatalf("got %q", got)
	}
	if got := envOr("PROXY_CAPTURE_UNSET_FOR_TEST", "gemini-wire.log"); got != "gemini-wire.log" {
		t.Fatalf("got %q", got)
	}
}

// A key sent as a query parameter reaches the upstream, and only the capture
// forgets it. A bare token with no "=" is a parameter name as far as the parser
// is concerned and a key as far as the caller is concerned, so no part of the
// query is kept.
func TestCaptureDropsTheQueryAndStillForwardsIt(t *testing.T) {
	var sawQuery atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery.Store(r.URL.RawQuery)
		_, _ = io.WriteString(w, "{}")
	}))
	defer upstream.Close()
	capture, err := os.CreateTemp(t.TempDir(), "capture")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{capture: capture, upstream: upstream.URL, client: upstream.Client()}
	for _, query := range []string{"key=query-key-value&alt=sse", "bare-key-token-value"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1beta/interactions?"+query, strings.NewReader("{}")))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		if got, _ := sawQuery.Load().(string); got != query {
			t.Fatalf("upstream saw query %q, want %q forwarded untouched", got, query)
		}
	}
	logged, err := os.ReadFile(capture.Name())
	if err != nil {
		t.Fatal(err)
	}
	text := string(logged)
	for _, leaked := range []string{"query-key-value", "bare-key-token-value", "alt=sse"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("capture wrote part of the query %q: %s", leaked, text)
		}
	}
	if strings.Count(text, "POST .../interactions ") != 2 {
		t.Fatalf("capture lost the request label: %s", text)
	}
}

// net/http's error for a failed request carries the full URL, query included.
func TestTransportFailureLeavesNoKeyInTheCaptureOrTheReply(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()
	capture, err := os.CreateTemp(t.TempDir(), "capture")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{capture: capture, upstream: dead.URL, client: &http.Client{}}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1beta/interactions?key=query-key-value", strings.NewReader("{}")))
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "query-key-value") {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	logged, err := os.ReadFile(capture.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "query-key-value") || !strings.Contains(string(logged), "TRANSPORT ERROR") {
		t.Fatalf("capture: %s", logged)
	}
}
