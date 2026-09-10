package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

	req := httptest.NewRequest(http.MethodPost, "/v1beta/interactions?alt=sse", strings.NewReader(`{"model":"gemini-3.7-flash"}`))
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
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1beta/interactions", strings.NewReader("{}")))
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
