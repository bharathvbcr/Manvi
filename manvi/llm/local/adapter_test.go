package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"manvi/credentials"
	"manvi/llm"
)

// server is a scriptable stand-in for a local model server. Each test drives
// the exact failure it is about rather than sharing one fixture, because the
// failures this adapter has to tell apart are precisely the ones a shared
// fixture would blur.
type server struct {
	*httptest.Server
	modelCalls atomic.Int64
	authSeen   chan string
}

func newServer(t *testing.T, models []string, status int) *server {
	t.Helper()
	s := &server{authSeen: make(chan string, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		s.modelCalls.Add(1)
		select {
		case s.authSeen <- r.Header.Get("Authorization"):
		default:
		}
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		var data []map[string]string
		for _, m := range models {
			data = append(data, map[string]string{"id": m, "object": "model"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.authSeen <- r.Header.Get("Authorization"):
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func cfgFor(s *server) Config {
	return Config{BaseURL: s.URL + "/v1", SupportsTools: true}
}

func noCredential() (credentials.Secret, error) {
	return credentials.Secret{}, &credentials.ErrMissing{Provider: Name, EnvVars: []string{"LOCAL_API_KEY"}}
}

// TestCapabilityComesFromTheServerNotFromAGuess is the premise of the package.
// llm.Provider forbids a permissive default, and a local server has no
// catalogue to transcribe, so the served set is asked for.
func TestCapabilityComesFromTheServerNotFromAGuess(t *testing.T) {
	s := newServer(t, []string{"qwen-local", "gemma-local"}, http.StatusOK)
	a := New(cfgFor(s), noCredential)

	if _, ok := a.Capability("qwen-local"); !ok {
		t.Fatal("a model the server lists must be served")
	}
	if _, ok := a.Capability("a-model-nobody-pulled"); ok {
		t.Fatal("a model the server does not list must not be invented")
	}
}

// TestAnUnreachableServerIsNotReportedAsAnAbsentModel is the distinction that
// decides where an operator goes next: restart a process, or fix a setting.
// Collapsing them sends them to the wrong one.
func TestAnUnreachableServerIsNotReportedAsAnAbsentModel(t *testing.T) {
	a := New(Config{BaseURL: "http://127.0.0.1:1/v1", SupportsTools: true}, noCredential)

	_, err := a.Stream(context.Background(), llm.Request{
		Model:    "anything",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("a request to a server that is not there must fail")
	}
	var undiscoverable *ErrUndiscoverable
	if !errors.As(err, &undiscoverable) {
		t.Fatalf("want ErrUndiscoverable, got %T: %v", err, err)
	}
	var notServed *ErrNotServed
	if errors.As(err, &notServed) {
		t.Fatal("a server that could not be asked must not be reported as one that answered")
	}
	if !strings.Contains(err.Error(), "the check did not run") {
		t.Fatalf("the error must say no check ran, got %q", err)
	}
}

// TestAModelTheServerDoesNotServeNamesTheAlternatives: the refusal is only
// useful if it says what is available instead.
func TestAModelTheServerDoesNotServeNamesTheAlternatives(t *testing.T) {
	s := newServer(t, []string{"qwen-local"}, http.StatusOK)
	a := New(cfgFor(s), noCredential)

	_, err := a.Stream(context.Background(), llm.Request{
		Model:    "typo-local",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}},
	})
	var notServed *ErrNotServed
	if !errors.As(err, &notServed) {
		t.Fatalf("want ErrNotServed, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "qwen-local") {
		t.Fatalf("the refusal must name what the server does serve, got %q", err)
	}
}

// TestAnEmptyListingIsAFailureNotAnEmptyCatalogue. A server that is up but has
// loaded nothing cannot serve a request, and reporting "no models" as a
// successful discovery would turn that into a per-model refusal that never
// mentions the real problem.
func TestAnEmptyListingIsAFailureNotAnEmptyCatalogue(t *testing.T) {
	s := newServer(t, nil, http.StatusOK)
	a := New(cfgFor(s), noCredential)

	if _, err := a.Models(context.Background()); err == nil {
		t.Fatal("an empty model list must be an error, not an empty success")
	}
}

// TestAMalformedListingIsReportedAsSuch guards the case where something other
// than a model server is listening on the port — a dev server, a proxy, an
// error page. Decoding failure must name the shape, not surface as "no models".
func TestAMalformedListingIsReportedAsSuch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>not a model server</html>")
	}))
	t.Cleanup(srv.Close)

	a := New(Config{BaseURL: srv.URL + "/v1", SupportsTools: true}, noCredential)
	_, err := a.Models(context.Background())
	if err == nil {
		t.Fatal("an HTML page is not a model listing")
	}
	if !strings.Contains(err.Error(), "documented shape") {
		t.Fatalf("the error must name the decoding fault, got %q", err)
	}
}

// TestDiscoveryIsCachedSoATurnDoesNotReDiscoverPerCall. Capability is consulted
// on the synchronous path to every request; without a cache a fan-out turns
// into one HTTP round trip per tool call.
func TestDiscoveryIsCachedSoATurnDoesNotReDiscoverPerCall(t *testing.T) {
	s := newServer(t, []string{"qwen-local"}, http.StatusOK)
	a := New(cfgFor(s), noCredential)

	for i := 0; i < 10; i++ {
		if _, ok := a.Capability("qwen-local"); !ok {
			t.Fatal("served model went missing between calls")
		}
	}
	if n := s.modelCalls.Load(); n != 1 {
		t.Fatalf("discovery ran %d times; the cache should have made it 1", n)
	}
}

// TestTheCacheExpires so a model pulled while the harness is running becomes
// visible without a restart.
func TestTheCacheExpires(t *testing.T) {
	s := newServer(t, []string{"qwen-local"}, http.StatusOK)
	a := New(cfgFor(s), noCredential)

	now := time.Now()
	a.now = func() time.Time { return now }

	if _, ok := a.Capability("qwen-local"); !ok {
		t.Fatal("first lookup failed")
	}
	now = now.Add(DefaultDiscoveryTTL + time.Second)
	if _, ok := a.Capability("qwen-local"); !ok {
		t.Fatal("second lookup failed")
	}
	if n := s.modelCalls.Load(); n != 2 {
		t.Fatalf("discovery ran %d times; an expired cache must refetch exactly once more", n)
	}
}

// TestAFailureIsCachedToo. Without this, a server that is down turns every
// Capability call on a fan-out into its own connection attempt, and the turn is
// spent timing out in parallel instead of failing once.
func TestAFailureIsCachedToo(t *testing.T) {
	s := newServer(t, nil, http.StatusInternalServerError)
	a := New(cfgFor(s), noCredential)

	for i := 0; i < 5; i++ {
		if _, ok := a.Capability("qwen-local"); ok {
			t.Fatal("a failing server must not report a model as served")
		}
	}
	// The transport retries a 500 within one discovery, so the assertion is
	// about discoveries, not requests: five lookups must not be five rounds of
	// retries.
	if n := s.modelCalls.Load(); n > int64(5) {
		t.Fatalf("the failure was not cached: %d model requests for 5 lookups", n)
	}
}

// TestConcurrentDiscoveryIssuesOneRequest. The agent loop and every fan-out
// sub-agent reach Capability, so this is genuinely concurrent. Run with -race.
func TestConcurrentDiscoveryIssuesOneRequest(t *testing.T) {
	s := newServer(t, []string{"qwen-local"}, http.StatusOK)
	a := New(cfgFor(s), noCredential)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := a.Capability("qwen-local"); !ok {
				t.Error("a served model was reported missing under concurrency")
			}
		}()
	}
	wg.Wait()
	if n := s.modelCalls.Load(); n != 1 {
		t.Fatalf("32 concurrent lookups issued %d discoveries, want 1", n)
	}
}

// TestNoCredentialSendsNoAuthorizationHeader. A loopback server that never
// checks a key must not be unreachable because the harness insisted on one, and
// an empty bearer is not the same as no header — some servers reject the first
// while accepting the second.
func TestNoCredentialSendsNoAuthorizationHeader(t *testing.T) {
	s := newServer(t, []string{"qwen-local"}, http.StatusOK)
	a := New(cfgFor(s), noCredential)

	if _, err := a.Models(context.Background()); err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	select {
	case got := <-s.authSeen:
		if got != "" {
			t.Fatalf("Authorization = %q; with no credential none must be sent", got)
		}
	default:
		t.Fatal("the server saw no request")
	}
}

// TestAPresentCredentialIsSent, because some operators front a local server
// with a proxy that does check.
func TestAPresentCredentialIsSent(t *testing.T) {
	s := newServer(t, []string{"qwen-local"}, http.StatusOK)
	a := New(cfgFor(s), func() (credentials.Secret, error) {
		return credentials.NewSecret("shhh", "LOCAL_API_KEY"), nil
	})

	if _, err := a.Models(context.Background()); err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	if got := <-s.authSeen; got != "Bearer shhh" {
		t.Fatalf("Authorization = %q, want the bearer token", got)
	}
}

// TestAssumeModelServedSkipsDiscovery is the escape hatch for a server with no
// /v1/models endpoint. It must not consult the server at all, or it would fail
// for exactly the servers it exists to support.
func TestAssumeModelServedSkipsDiscovery(t *testing.T) {
	s := newServer(t, nil, http.StatusNotFound)
	cfg := cfgFor(s)
	cfg.AssumeModelServed = true
	a := New(cfg, noCredential)

	if _, ok := a.Capability("whatever-the-operator-said"); !ok {
		t.Fatal("with assume_model_served the configured model is taken on trust")
	}
	if n := s.modelCalls.Load(); n != 0 {
		t.Fatalf("discovery ran %d times; the whole point is that it does not", n)
	}
}

// TestDeclaredOutputCapIsEnforced. The declared cap is the operator's statement
// about their server, and a request above it is refused at assembly rather than
// truncated mid-generation.
func TestDeclaredOutputCapIsEnforced(t *testing.T) {
	s := newServer(t, []string{"qwen-local"}, http.StatusOK)
	cfg := cfgFor(s)
	cfg.ContextWindow = 8192
	cfg.MaxOutputTokens = 1024
	a := New(cfg, noCredential)

	_, err := a.Stream(context.Background(), llm.Request{
		Model:     "qwen-local",
		MaxTokens: 4096,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("a request above the declared output cap must be refused")
	}
	if !strings.Contains(err.Error(), "caps output") {
		t.Fatalf("the refusal must name the cap, got %q", err)
	}
}

// TestReasoningEffortIsRefusedUnlessDeclared. Servers that do not understand
// reasoning_effort differ in whether they ignore it or reject the whole
// request, and the second loses a turn to a parameter nobody asked for.
func TestReasoningEffortIsRefusedUnlessDeclared(t *testing.T) {
	s := newServer(t, []string{"qwen-local"}, http.StatusOK)
	a := New(cfgFor(s), noCredential)

	_, err := a.Stream(context.Background(), llm.Request{
		Model:    "qwen-local",
		Effort:   "high",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("an effort level must be refused on a server not declared to support one")
	}

	cfg := cfgFor(s)
	cfg.SupportsReasoning = true
	declared := New(cfg, noCredential)
	if _, err := declared.Stream(context.Background(), llm.Request{
		Model:    "qwen-local",
		Effort:   "high",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}},
	}); err != nil {
		t.Fatalf("with reasoning declared, an effort level must be accepted: %v", err)
	}
}

// TestAnEmptyModelIsRefusedBeforeTheWire. An empty model name would otherwise
// reach the server as a request for the model called "", whose refusal says
// nothing about the setting that was never filled in.
func TestAnEmptyModelIsRefusedBeforeTheWire(t *testing.T) {
	s := newServer(t, []string{"qwen-local"}, http.StatusOK)
	a := New(cfgFor(s), noCredential)

	if _, ok := a.Capability("   "); ok {
		t.Fatal("whitespace is not a model name")
	}
	_, err := a.Stream(context.Background(), llm.Request{
		Model:    "",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}},
	})
	if err == nil || !strings.Contains(err.Error(), "no model named") {
		t.Fatalf("an unset model must name the setting, got %v", err)
	}
	if s.modelCalls.Load() != 0 {
		t.Fatal("an unset model must be caught before the server is contacted")
	}
}

// TestAnEntryWithNoIDIsSkipped. A listing entry naming no model must not admit
// the empty string into the served set, where it would match a request whose
// model was never configured.
func TestAnEntryWithNoIDIsSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[{"id":""},{"id":"real-model"}]}`)
	}))
	t.Cleanup(srv.Close)

	a := New(Config{BaseURL: srv.URL + "/v1", SupportsTools: true}, noCredential)
	models, err := a.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != "real-model" {
		t.Fatalf("models = %v; an entry with no id names no model", models)
	}
}

// TestReplayIsNeverClaimed. The set of engines behind this adapter is
// open-ended and none documents replaying reasoning state across a turn, so the
// fail-closed answer is the only honest one.
func TestReplayIsNeverClaimed(t *testing.T) {
	if ReplayableOn("a", "a") {
		t.Fatal("replayability must not be claimed, even for one model to itself")
	}
}

// TestSamplingParametersAppliedWhenUnset verifies MinP, Stop, Temperature, TopP,
// and RepetitionPenalty are populated from Config when not specified in Request.
func TestSamplingParametersAppliedWhenUnset(t *testing.T) {
	var captured wireCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []map[string]string{{"id": "qwen-27b", "object": "model"}},
			})
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			_ = json.NewDecoder(r.Body).Decode(&captured)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	temp := 0.2
	topP := 0.9
	minP := 0.05
	repPen := 1.10
	stopSeqs := []string{"<|im_end|>", "<|endoftext|>"}

	cfg := Config{
		BaseURL:           srv.URL + "/v1",
		SupportsTools:     true,
		Temperature:       &temp,
		TopP:              &topP,
		MinP:              &minP,
		RepetitionPenalty: &repPen,
		Stop:              stopSeqs,
	}

	adapter := New(cfg, noCredential)
	stream, err := adapter.Stream(context.Background(), llm.Request{
		Model: "qwen-27b",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "test"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	for {
		if _, err := stream.Next(); err == io.EOF {
			break
		}
	}

	if captured.Temperature == nil || *captured.Temperature != 0.2 {
		t.Errorf("expected Temperature 0.2, got %v", captured.Temperature)
	}
	if captured.TopP == nil || *captured.TopP != 0.9 {
		t.Errorf("expected TopP 0.9, got %v", captured.TopP)
	}
	if captured.MinP == nil || *captured.MinP != 0.05 {
		t.Errorf("expected MinP 0.05, got %v", captured.MinP)
	}
	if captured.RepetitionPenalty == nil || *captured.RepetitionPenalty != 1.10 {
		t.Errorf("expected RepetitionPenalty 1.10, got %v", captured.RepetitionPenalty)
	}
	if len(captured.Stop) != 2 || captured.Stop[0] != "<|im_end|>" || captured.Stop[1] != "<|endoftext|>" {
		t.Errorf("expected Stop sequences %v, got %v", stopSeqs, captured.Stop)
	}
}

type wireCapture struct {
	Model             string   `json:"model"`
	Temperature       *float64 `json:"temperature"`
	TopP              *float64 `json:"top_p"`
	MinP              *float64 `json:"min_p"`
	RepetitionPenalty *float64 `json:"repetition_penalty"`
	Stop              []string `json:"stop"`
}

// TestADiscoveredWindowBelowTheDeclaredCapStillCapsOutput is the llama.cpp
// case: a 262k model served with -c 8192 against the shipped 16384-token output
// default. Dropping the cap to zero there reads as "no stated cap" in
// llm.Capability.Validate, so a 16384-token request would be validated against
// an 8192-token window and truncated mid-generation by the server instead of
// refused at assembly.
func TestADiscoveredWindowBelowTheDeclaredCapStillCapsOutput(t *testing.T) {
	const model = "qwen-local"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		case "/props":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_generation_settings": map[string]any{"n_ctx": 8192}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// The shipped defaults: a 32768-token declaration and a 16384-token cap,
	// against a server started at 8192.
	a := New(Config{BaseURL: srv.URL + "/v1", SupportsTools: true}, noCredential)

	cap, ok := a.Capability(model)
	if !ok {
		t.Fatal("the server lists the model")
	}
	if cap.ContextWindow != 8192 {
		t.Fatalf("ContextWindow = %d, want the discovered 8192", cap.ContextWindow)
	}
	if cap.MaxOutputTokens <= 0 {
		t.Fatalf("MaxOutputTokens = %d; zero reads as 'no stated cap' and lets a "+
			"16384-token request through against an 8192-token window", cap.MaxOutputTokens)
	}
	if cap.MaxOutputTokens > 8192 {
		t.Fatalf("MaxOutputTokens = %d, above the discovered window", cap.MaxOutputTokens)
	}
	if err := cap.Validate(llm.Request{Model: model, MaxTokens: 16384}); err == nil {
		t.Fatal("a 16384-token output request against an 8192-token window must be refused")
	}
}

// TestAnUncappedRequestIsBoundedByTheDiscoveredWindow covers the half of the
// same defect that lives outside Capability: openaicompat.Options.DefaultMaxTokens
// is fixed at construction from the declared cap, so a request that names no
// MaxTokens shipped max_tokens: 16384 to a server started at 8192 no matter
// what discovery later learned.
func TestAnUncappedRequestIsBoundedByTheDiscoveredWindow(t *testing.T) {
	const model = "qwen-local"
	var sentMaxTokens atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		case "/props":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_generation_settings": map[string]any{"n_ctx": 8192}})
		case "/v1/chat/completions":
			var body struct {
				MaxTokens int `json:"max_tokens"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			sentMaxTokens.Store(int64(body.MaxTokens))
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL + "/v1", SupportsTools: true}, noCredential)
	stream, err := a.Stream(context.Background(), llm.Request{
		Model:    model,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for {
		if _, err := stream.Next(); err != nil {
			break
		}
	}
	got := sentMaxTokens.Load()
	if got <= 0 {
		t.Fatalf("no max_tokens reached the server (%d); an unbounded request is not something to send", got)
	}
	if got > 8192 {
		t.Fatalf("max_tokens = %d went to a server started with an 8192-token window", got)
	}
}

// TestTheDeclaredOutputCapIsClampedToTheDeclaredWindow is the same defect on
// the purely declared path: an operator who sets a 8192-token window and leaves
// the 16384-token output default gets no cap at all, because
// Config.baseCapability reads "cap >= window" as "no cap" rather than as "clamp".
func TestTheDeclaredOutputCapIsClampedToTheDeclaredWindow(t *testing.T) {
	cfg := Config{ContextWindow: 8192, MaxOutputTokens: 16384, SupportsTools: true}.withDefaults()
	cap := cfg.baseCapability("m")
	if cap.MaxOutputTokens <= 0 || cap.MaxOutputTokens > 8192 {
		t.Fatalf("MaxOutputTokens = %d, want a positive cap no larger than the 8192-token window",
			cap.MaxOutputTokens)
	}
}

// TestTheDimensionCacheIsBounded. dims is keyed by a caller-supplied model
// string and, with assume_model_served on, any string a caller passes reaches
// it. Without a bound, a long-lived server process grows the map for the life
// of the run.
func TestTheDimensionCacheIsBounded(t *testing.T) {
	cfg := Config{BaseURL: "http://127.0.0.1:1/v1", SupportsTools: true, AssumeModelServed: true}
	a := New(cfg, noCredential)
	for i := 0; i < maxDimEntries*3; i++ {
		a.Capability(fmt.Sprintf("model-%d", i))
	}
	a.dimMu.Lock()
	n := len(a.dims)
	a.dimMu.Unlock()
	if n > maxDimEntries {
		t.Fatalf("the dimension cache holds %d entries, above the %d bound", n, maxDimEntries)
	}
}
