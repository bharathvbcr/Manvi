package local

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ollamaServer mimics the endpoints Ollama actually exposes, including the
// architecture-prefixed context_length key and the capability list.
func ollamaServer(t *testing.T, model string, contextLength int, caps []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": model, "object": "model"}},
			})
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model_info":   map[string]any{"qwen3_5.context_length": contextLength},
				"capabilities": caps,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestContextWindowIsDiscoveredFromOllama(t *testing.T) {
	const model = "qwen3.8:27b-mlx"
	srv := ollamaServer(t, model, 262144, []string{"completion", "vision", "tools", "thinking"})

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 32768, SupportsTools: true}, noCredential)

	dims := a.Dimensions(context.Background(), model)
	if dims.ContextWindow != 262144 {
		t.Fatalf("ContextWindow = %d, want 262144 — the declared 32768 wastes 94%% of the model",
			dims.ContextWindow)
	}
	if !dims.Discovered() || dims.Source != SourceOllama {
		t.Fatalf("Source = %q, want %q", dims.Source, SourceOllama)
	}
	if !strings.Contains(dims.Describe(), "ollama") {
		t.Errorf("Describe() hides the provenance: %q", dims.Describe())
	}

	cap, ok := a.Capability(model)
	if !ok {
		t.Fatal("the model was not served")
	}
	if cap.ContextWindow != 262144 {
		t.Fatalf("capability window = %d; discovery did not reach the budget", cap.ContextWindow)
	}
	if !cap.SupportsImages {
		t.Error("the server declared vision and the capability does not")
	}
}

func TestDeclaredWindowIsUsedWhenTheServerPublishesNone(t *testing.T) {
	// The MLX server: /v1/models and /health, nothing else. The declaration is
	// genuinely the only source, and that must be said rather than implied.
	const model = "mlx-community/Qwen3.8-27B-4bit"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": model}},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 40000, SupportsTools: true}, noCredential)
	dims := a.Dimensions(context.Background(), model)

	if dims.ContextWindow != 40000 {
		t.Fatalf("ContextWindow = %d, want the declared 40000", dims.ContextWindow)
	}
	if dims.Discovered() {
		t.Fatal("a declared window was reported as discovered")
	}
	if !strings.Contains(dims.Describe(), "declared") {
		t.Errorf("Describe() = %q; an operator must be able to tell a read value from a typed one",
			dims.Describe())
	}
}

func TestVLLMWindowComesFromTheListingWithoutASecondRequest(t *testing.T) {
	const model = "Qwen/Qwen3-27B"
	var listCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			listCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": model, "max_model_len": 131072}},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 32768, SupportsTools: true}, noCredential)
	dims := a.Dimensions(context.Background(), model)

	if dims.ContextWindow != 131072 || dims.Source != SourceVLLM {
		t.Fatalf("dims = %+v, want 131072 from vLLM", dims)
	}
	if listCalls != 1 {
		t.Fatalf("the listing was fetched %d times; the window rides on the request already made", listCalls)
	}
}

func TestLlamaCPPServedWindowBeatsTheModelCard(t *testing.T) {
	// A 262k-capable model served with -c 8192 has an 8192-token window. The
	// served value is the binding constraint and must win.
	const model = "qwen3"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		case "/props":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"default_generation_settings": map[string]any{"n_ctx": 8192},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 262144, SupportsTools: true}, noCredential)
	dims := a.Dimensions(context.Background(), model)

	if dims.ContextWindow != 8192 {
		t.Fatalf("ContextWindow = %d, want the 8192 the server was started with", dims.ContextWindow)
	}
	if dims.Source != SourceLlamaCPP {
		t.Fatalf("Source = %q", dims.Source)
	}
}

func TestDiscoveryNeverWidensWhatTheOperatorTurnedOff(t *testing.T) {
	const model = "qwen3.8:27b-mlx"
	srv := ollamaServer(t, model, 262144, []string{"tools", "thinking", "vision"})

	// The operator declared no tool support. Discovery finding that the server
	// has tools must not re-enable them: turning something off is a decision,
	// not an absence of information.
	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 32768, SupportsTools: false}, noCredential)
	cap, ok := a.Capability(model)
	if !ok {
		t.Fatal("model not served")
	}
	if cap.SupportsTools {
		t.Fatal("discovery overruled an operator who deliberately disabled tools")
	}
	// reasoning_effort is a separate wire question from "the model thinks", so
	// discovering thinking must not start sending the field.
	if cap.SupportsReasoning {
		t.Fatal("discovering a thinking model turned on reasoning_effort, which is a different claim")
	}
}

func TestTrustDeclaredContextSkipsTheProbeEntirely(t *testing.T) {
	const model = "m"
	var showCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			showCalls++
		}
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 12345,
		SupportsTools: true, TrustDeclaredContext: true}, noCredential)
	dims := a.Dimensions(context.Background(), model)

	if dims.ContextWindow != 12345 || dims.Discovered() {
		t.Fatalf("dims = %+v, want the declared value", dims)
	}
	if showCalls != 0 {
		t.Fatalf("the server was probed %d times despite the operator opting out", showCalls)
	}
}

func TestAnUnreachableProbeFallsBackRatherThanFailing(t *testing.T) {
	const model = "m"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
			return
		}
		// Everything else hangs up mid-response, which is worse than a 404.
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 9999, SupportsTools: true}, noCredential)
	dims := a.Dimensions(context.Background(), model)
	if dims.ContextWindow != 9999 {
		t.Fatalf("a broken probe did not fall back to the declaration: %+v", dims)
	}
}

func TestGarbageProbeResponsesAreIgnored(t *testing.T) {
	const model = "m"
	for _, body := range []string{
		`not json at all`,
		`{"model_info":{"x.context_length":"not a number"}}`,
		`{"model_info":{"x.context_length":0}}`,
		`{"model_info":null,"capabilities":null}`,
		`{"default_generation_settings":{"n_ctx":-5}}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
				return
			}
			_, _ = w.Write([]byte(body))
		}))
		a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 4321, SupportsTools: true}, noCredential)
		dims := a.Dimensions(context.Background(), model)
		if dims.ContextWindow != 4321 {
			t.Errorf("body %q produced window %d; garbage must not become a budget", body, dims.ContextWindow)
		}
		srv.Close()
	}
}

// TestAnImplausibleDiscoveredWindowIsNotBelieved. capability.go argues at
// length for a modest *declared* default because over-declaring a window
// produces a request the server truncates deep into a turn — and then this file
// accepted any positive *discovered* number without bound. A ten-billion-token
// budget is one the harness never compacts against, so every request overflows
// the real server mid-turn: exactly the failure the declared default avoids.
func TestAnImplausibleDiscoveredWindowIsNotBelieved(t *testing.T) {
	const model = "m"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model_info": map[string]any{"x.context_length": 10000000000},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 32768, SupportsTools: true}, noCredential)
	dims := a.Dimensions(context.Background(), model)
	if dims.ContextWindow != 32768 {
		t.Fatalf("ContextWindow = %d; a ten-billion-token claim must fall back to the declaration",
			dims.ContextWindow)
	}
	if dims.Discovered() {
		t.Fatalf("Source = %q; a rejected number must not be reported as discovered", dims.Source)
	}
	// Silently clamping to a number nobody chose would leave an operator unable
	// to tell a believed window from a rejected one.
	desc := dims.Describe()
	if !strings.Contains(desc, "10000000000") {
		t.Fatalf("Describe() = %q; it must name what the server claimed", desc)
	}
}

// TestAPlausibleLargeWindowIsStillBelieved guards the ceiling from being set so
// low that it rejects a real model. Qwen3's 262144 is an ordinary local window.
func TestAPlausibleLargeWindowIsStillBelieved(t *testing.T) {
	const model = "m"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model_info": map[string]any{"qwen3_5.context_length": 262144},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 32768, SupportsTools: true}, noCredential)
	if got := a.Dimensions(context.Background(), model).ContextWindow; got != 262144 {
		t.Fatalf("ContextWindow = %d, want the discovered 262144", got)
	}
}

// TestTheTextContextLengthKeyWinsOverAComponentKey. A vision-language model
// carries a second ".context_length" under its vision sub-namespace, and the
// old suffix match broke on whichever key Go's map iteration reached first: the
// same server gave the same model a 262144-token budget in one process and a
// 1024-token one in the next, both reported as authoritative.
func TestTheTextContextLengthKeyWinsOverAComponentKey(t *testing.T) {
	const model = "qwen3vl"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model_info": map[string]any{
					"general.architecture":          "qwen3vl",
					"qwen3vl.context_length":        262144,
					"qwen3vl.vision.context_length": 1024,
				},
				"capabilities": []string{"completion", "vision"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Fresh adapters, because the cache would hide a per-process choice behind
	// one lucky first answer.
	for i := 0; i < 40; i++ {
		a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 32768, SupportsTools: true}, noCredential)
		if got := a.Dimensions(context.Background(), model).ContextWindow; got != 262144 {
			t.Fatalf("run %d: ContextWindow = %d, want the text window 262144 rather than the vision sub-key",
				i, got)
		}
	}
}

// TestTheContextLengthKeyIsChosenDeterministicallyWithoutAnArchitecture. Not
// every server publishes general.architecture, so the tie-break must still be
// deterministic — and must still prefer the top-level key over a component one.
func TestTheContextLengthKeyIsChosenDeterministicallyWithoutAnArchitecture(t *testing.T) {
	const model = "m"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model_info": map[string]any{
					"llava.context_length":        131072,
					"llava.vision.context_length": 1024,
					"llava.audio.context_length":  512,
					"clip.vision.context_length":  768,
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	for i := 0; i < 40; i++ {
		a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 32768, SupportsTools: true}, noCredential)
		if got := a.Dimensions(context.Background(), model).ContextWindow; got != 131072 {
			t.Fatalf("run %d: ContextWindow = %d, want the top-level 131072", i, got)
		}
	}
}

// mlxHealthBody is the shape the MLX server actually returns, captured from
// mlx_vlm.server serving mlx-community/Qwen3.8-27B-4bit.
func mlxHealthBody(model string, loaded, effective int, configured any) map[string]any {
	return map[string]any{
		"status":                      "healthy",
		"loaded_model":                model,
		"loaded_adapter":              nil,
		"loaded_context_size":         loaded,
		"configured_context_limit":    configured,
		"effective_context_limit":     effective,
		"loaded_tool_parser":          "qwen3_coder",
		"continuous_batching_enabled": true,
		"apc_enabled":                 true,
	}
}

func mlxServer(t *testing.T, model string, loaded, effective int, configured any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		case "/health":
			_ = json.NewEncoder(w).Encode(mlxHealthBody(model, loaded, effective, configured))
		default:
			// Everything the other probes ask for. The MLX server answers none
			// of it, which is the whole reason this probe had to exist.
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMLXWindowIsDiscoveredFromHealth covers the server the package doc said
// could not be asked.
//
// The doc claimed the MLX server exposes "only /v1/models and /health", and
// concluded that the declaration was therefore the only source. The first half
// is right and the second does not follow: /health publishes the window. Left
// undiscovered, a 262k model ran against the 32k default — the harness
// compacting history it had eight times the room for.
func TestMLXWindowIsDiscoveredFromHealth(t *testing.T) {
	const model = "mlx-community/Qwen3.8-27B-4bit"
	srv := mlxServer(t, model, 262144, 262144, nil)

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 32768, SupportsTools: true}, noCredential)
	dims := a.Dimensions(context.Background(), model)

	if dims.ContextWindow != 262144 {
		t.Fatalf("ContextWindow = %d, want the 262144 the server reports", dims.ContextWindow)
	}
	if dims.Source != SourceMLX {
		t.Fatalf("Source = %q, want %q", dims.Source, SourceMLX)
	}
}

// TestMLXConfiguredLimitBeatsTheLoadedSize is the same rule llama.cpp gets: an
// operator who capped the server below the weights meant it, and that cap is
// the binding constraint.
func TestMLXConfiguredLimitBeatsTheLoadedSize(t *testing.T) {
	const model = "m"
	srv := mlxServer(t, model, 262144, 16384, 16384)

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 32768}, noCredential)
	dims := a.Dimensions(context.Background(), model)

	if dims.ContextWindow != 16384 {
		t.Fatalf("ContextWindow = %d, want the 16384 the server was capped to", dims.ContextWindow)
	}
}

// TestMLXHealthForAnotherModelIsNotBorrowed guards the one way this probe could
// lie. /health describes whichever model is loaded now; a request for a
// different one must fall back to the declaration rather than inherit a window
// that was never measured for it.
func TestMLXHealthForAnotherModelIsNotBorrowed(t *testing.T) {
	srv := mlxServer(t, "some-other-model", 262144, 262144, nil)

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 32768}, noCredential)
	dims := a.Dimensions(context.Background(), "the-one-i-asked-for")

	if dims.ContextWindow != 32768 {
		t.Fatalf("ContextWindow = %d, want the declared 32768", dims.ContextWindow)
	}
	if dims.Source != SourceDeclared {
		t.Fatalf("Source = %q, want the declaration", dims.Source)
	}
}
