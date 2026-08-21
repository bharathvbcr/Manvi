package local

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// listingServer answers only the OpenAI-compatible model listing, which is what
// the MLX server does and therefore the floor every scan must handle.
func listingServer(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	entries := make([]map[string]any, 0, len(models))
	for _, m := range models {
		entries = append(entries, map[string]any{"id": m, "object": "model"})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": entries})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ollamaFleet answers the three endpoints Ollama actually serves, for a set of
// models each with its own capability list.
func ollamaFleet(t *testing.T, version string, caps map[string][]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			entries := make([]map[string]any, 0, len(caps))
			for id := range caps {
				entries = append(entries, map[string]any{"id": id, "object": "model"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": entries})
		case "/api/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": version})
		case "/api/show":
			var req struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			list, ok := caps[req.Model]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model_info":   map[string]any{"qwen3_5.context_length": 262144},
				"capabilities": list,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func endpointsOf(urls ...string) []Endpoint {
	out := make([]Endpoint, 0, len(urls))
	for _, u := range urls {
		out = append(out, Endpoint{BaseURL: u + "/v1"})
	}
	return out
}

// TestAnEmbeddingModelIsNotOfferedAsSomethingToRun is the case that motivated
// reading capabilities at all. An embedding model answers /v1/models beside
// every chat model, and a listing that presents it as a candidate sends an
// operator to debug a configuration that was never the problem.
func TestAnEmbeddingModelIsNotOfferedAsSomethingToRun(t *testing.T) {
	srv := ollamaFleet(t, "0.32.13", map[string][]string{
		"qwen3.8:27b-mlx":         {"completion", "tools", "thinking"},
		"nomic-embed-text:latest": {"embedding"},
	})

	servers := Scan(context.Background(), ScanOptions{
		Endpoints: endpointsOf(srv.URL), Capabilities: true, Timeout: 10 * time.Second,
	})
	if len(servers) != 1 {
		t.Fatalf("found %d servers, want 1", len(servers))
	}
	usable := servers[0].Usable()
	if len(usable) != 1 || usable[0].ID != "qwen3.8:27b-mlx" {
		t.Fatalf("usable = %v, want only the chat model", usable)
	}

	model, why := SoleUsableModel(servers[0])
	if model != "qwen3.8:27b-mlx" {
		t.Fatalf("SoleUsableModel = %q (%s), want the one model that can run a turn", model, why)
	}
	for _, m := range servers[0].Models {
		if m.ID != "nomic-embed-text:latest" {
			continue
		}
		if !m.Embedding() {
			t.Error("the embedding model was not recognised as one")
		}
		if !strings.Contains(m.Why(), "embedding") {
			t.Errorf("Why() = %q; a rejected model must say why", m.Why())
		}
	}
}

// TestAModelServedWithoutToolsIsRejectedWithItsReason keeps the two ways a
// model can be unusable distinct. Both end a coding turn; they send the
// operator to different places.
func TestAModelServedWithoutToolsIsRejectedWithItsReason(t *testing.T) {
	srv := ollamaFleet(t, "0.32.13", map[string][]string{
		"textonly:7b": {"completion"},
	})
	servers := Scan(context.Background(), ScanOptions{
		Endpoints: endpointsOf(srv.URL), Capabilities: true, Timeout: 10 * time.Second,
	})
	if len(servers) != 1 {
		t.Fatalf("found %d servers, want 1", len(servers))
	}
	if len(servers[0].Usable()) != 0 {
		t.Fatal("a model served without tool support was offered for a coding turn")
	}
	_, why := SoleUsableModel(servers[0])
	if !strings.Contains(why, "tool") {
		t.Errorf("why = %q, want it to name tool support", why)
	}
}

// TestSilenceIsNotATNegativeCapabilityResult guards the MLX case. That server
// publishes nothing about any model, and treating "did not say" as "cannot"
// would hide every model it serves.
func TestSilenceIsNotANegativeCapabilityResult(t *testing.T) {
	srv := listingServer(t, "mlx-community/Qwen3.8-27B-4bit")
	servers := Scan(context.Background(), ScanOptions{
		Endpoints: endpointsOf(srv.URL), Capabilities: true, Timeout: 10 * time.Second,
	})
	if len(servers) != 1 {
		t.Fatalf("found %d servers, want 1", len(servers))
	}
	if len(servers[0].Usable()) != 1 {
		t.Fatal("a server that published no capabilities had its only model hidden")
	}
	if servers[0].Models[0].CapabilitiesKnown {
		t.Error("capabilities were reported as known when the server said nothing")
	}
	if servers[0].Models[0].Why() != "" {
		t.Errorf("Why() = %q; an unchecked model must not carry a rejection",
			servers[0].Models[0].Why())
	}
}

// TestRuntimeIsEstablishedByAskingNotByPort is the whole reason identification
// exists as a probe. httptest servers listen on an arbitrary port, so a runtime
// read off a port number could not possibly be right here — which is exactly
// the situation of an operator who moved theirs.
func TestRuntimeIsEstablishedByAskingNotByPort(t *testing.T) {
	ollama := ollamaFleet(t, "0.32.13", map[string][]string{"qwen3.8:27b-mlx": {"completion", "tools"}})
	bare := listingServer(t, "some-model")

	servers := Scan(context.Background(), ScanOptions{
		Endpoints: endpointsOf(ollama.URL, bare.URL), Timeout: 10 * time.Second,
	})
	if len(servers) != 2 {
		t.Fatalf("found %d servers, want 2", len(servers))
	}
	if servers[0].Runtime != RuntimeOllama {
		t.Errorf("runtime = %q, want %q", servers[0].Runtime, RuntimeOllama)
	}
	if servers[0].Version != "0.32.13" {
		t.Errorf("version = %q, want the one the server reported", servers[0].Version)
	}
	if servers[1].Runtime != RuntimeUnidentified {
		t.Errorf("runtime = %q; a server this harness cannot identify must say so "+
			"rather than be labelled with whatever conventionally holds its port",
			servers[1].Runtime)
	}
}

// TestVLLMIsIdentifiedFromTheListingItAlreadyFetched keeps identification free
// for the one runtime that marks its own model card.
func TestVLLMIsIdentifiedFromTheListingItAlreadyFetched(t *testing.T) {
	var nativeRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			nativeRequests++
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "Qwen/Qwen3.8-27B", "max_model_len": 262144}},
		})
	}))
	t.Cleanup(srv.Close)

	servers := Scan(context.Background(), ScanOptions{
		Endpoints: endpointsOf(srv.URL), Timeout: 10 * time.Second,
	})
	if len(servers) != 1 || servers[0].Runtime != RuntimeVLLM {
		t.Fatalf("servers = %+v, want one identified as vllm", servers)
	}
	if nativeRequests != 0 {
		t.Errorf("%d native probes were issued; vLLM is identifiable from the "+
			"listing already fetched and must cost no extra request", nativeRequests)
	}
}

// TestADeadEndpointIsAbsentRatherThanAnError is what makes scanning five
// addresses acceptable: four of them are expected to refuse.
func TestADeadEndpointIsAbsentRatherThanAnError(t *testing.T) {
	live := listingServer(t, "a-model")
	servers := Scan(context.Background(), ScanOptions{
		Endpoints: []Endpoint{
			{BaseURL: "http://127.0.0.1:1/v1"},
			{BaseURL: live.URL + "/v1"},
		},
		Timeout: 5 * time.Second,
	})
	if len(servers) != 1 || servers[0].BaseURL != live.URL+"/v1" {
		t.Fatalf("servers = %+v, want only the live one", servers)
	}
}

// TestScanPreservesEndpointOrder matters because resolution and the listing
// both read position as preference.
func TestScanPreservesEndpointOrder(t *testing.T) {
	first := listingServer(t, "first")
	second := listingServer(t, "second")
	servers := Scan(context.Background(), ScanOptions{
		Endpoints: endpointsOf(first.URL, second.URL), Timeout: 5 * time.Second,
	})
	if len(servers) != 2 {
		t.Fatalf("found %d servers, want 2", len(servers))
	}
	if servers[0].BaseURL != first.URL+"/v1" || servers[1].BaseURL != second.URL+"/v1" {
		t.Fatalf("order = %s, %s; a concurrent scan must still answer in endpoint order",
			servers[0].BaseURL, servers[1].BaseURL)
	}
}

// TestADeclaredAddressIsNeverSecondGuessed is the hinge of the whole feature.
// Probing alternatives to a value someone typed is the harness arguing with its
// operator about their own machine.
func TestADeclaredAddressIsNeverSecondGuessed(t *testing.T) {
	res := ResolveEndpoint(context.Background(), ResolveOptions{
		Declared: "http://127.0.0.1:9999/v1", DeclaredByOperator: true,
	})
	if res.BaseURL != "http://127.0.0.1:9999/v1" {
		t.Fatalf("BaseURL = %q, want the declared one unchanged", res.BaseURL)
	}
	if !res.Declared {
		t.Error("the resolution did not record that the operator declared it")
	}
	if len(res.Scanned) != 0 {
		t.Errorf("%d endpoints were scanned behind a declared address", len(res.Scanned))
	}
	if res.Note() != "" {
		t.Errorf("Note() = %q; there is nothing to report about an address that was configured", res.Note())
	}
}

// TestASingleFoundServerIsUsed is the case the shipped default gets wrong: an
// operator running Ollama meets a refusal about vLLM's port.
func TestASingleFoundServerIsUsed(t *testing.T) {
	srv := ollamaFleet(t, "0.32.13", map[string][]string{"qwen3.8:27b-mlx": {"completion", "tools"}})
	res := resolveOver(t, endpointsOf(srv.URL), DefaultBaseURL)
	if res.BaseURL != srv.URL+"/v1" {
		t.Fatalf("BaseURL = %q, want the one server that answered", res.BaseURL)
	}
	if res.Found == nil || res.Found.Runtime != RuntimeOllama {
		t.Fatalf("Found = %+v, want the identified server", res.Found)
	}
	if !strings.Contains(res.Note(), srv.URL) {
		t.Errorf("Note() = %q; an address that was found must not read like one that was configured", res.Note())
	}
}

// TestTheShippedDefaultIsNotATiebreaker replaces a test that asserted the
// opposite, and was wrong.
//
// It used to require that when several servers answered, one sitting on the
// configured address won. But this branch only runs when the operator has NOT
// configured an address — a declared one skips the scan entirely — so the value
// it was matching against was the harness's own shipped guess. The test
// therefore encoded "a guess outranks the other servers", which made the
// ambiguity report unreachable on any machine with something on port 8000, and
// on this author's machine picked a server publishing no capabilities over one
// publishing all of them for no better reason than the port number.
func TestTheShippedDefaultIsNotATiebreaker(t *testing.T) {
	onDefaultPort := listingServer(t, "model-a")
	other := listingServer(t, "model-b")

	// The declared value is the shipped default, as it always is here, and one
	// of the answering servers is standing in for it.
	res := ResolveEndpoint(context.Background(), ResolveOptions{
		Declared:  onDefaultPort.URL + "/v1",
		Endpoints: endpointsOf(onDefaultPort.URL, other.URL),
		Timeout:   5 * time.Second,
	})
	if res.Found != nil {
		t.Fatalf("resolved to %s; a guess was treated as a preference", res.Found.BaseURL)
	}
	if len(res.Ambiguous) != 2 {
		t.Fatalf("Ambiguous = %d, want both servers reported", len(res.Ambiguous))
	}
}

// TestANamedModelSettlesWhichServerToUse is the case that makes discovery
// usable on a machine running more than one server.
//
// The operator named a model and not an address. Exactly one running server has
// that model, so there is nothing left to choose — and what chooses is their own
// setting, not the harness.
func TestANamedModelSettlesWhichServerToUse(t *testing.T) {
	wrong := listingServer(t, "mlx-community/Qwen3.8-27B-4bit")
	right := ollamaFleet(t, "0.32.13", map[string][]string{
		"qwen3.8:27b-mlx": {"completion", "tools"},
	})

	res := ResolveEndpoint(context.Background(), ResolveOptions{
		Declared:  DefaultBaseURL,
		Model:     "qwen3.8:27b-mlx",
		Endpoints: endpointsOf(wrong.URL, right.URL),
		Timeout:   5 * time.Second,
	})
	if res.BaseURL != right.URL+"/v1" {
		t.Fatalf("BaseURL = %q, want the server that actually has the model", res.BaseURL)
	}
	if res.MatchedModel != "qwen3.8:27b-mlx" {
		t.Errorf("MatchedModel = %q; the report cannot say what settled it", res.MatchedModel)
	}
	if !strings.Contains(res.Note(), "qwen3.8:27b-mlx") {
		t.Errorf("Note() = %q, want it to name the model that decided", res.Note())
	}
}

// TestANamedModelOnSeveralServersIsStillAmbiguous keeps the rule narrow: it
// settles the question only when it actually settles it.
func TestANamedModelOnSeveralServersIsStillAmbiguous(t *testing.T) {
	a := listingServer(t, "shared:7b")
	b := listingServer(t, "shared:7b")
	res := ResolveEndpoint(context.Background(), ResolveOptions{
		Declared:  DefaultBaseURL,
		Model:     "shared:7b",
		Endpoints: endpointsOf(a.URL, b.URL),
		Timeout:   5 * time.Second,
	})
	if res.Found != nil {
		t.Fatalf("resolved to %s; two servers have this model and neither was chosen by anyone",
			res.Found.BaseURL)
	}
	if len(res.Ambiguous) != 2 {
		t.Errorf("Ambiguous = %d, want both reported", len(res.Ambiguous))
	}
}

// TestANamedModelNoServerHasChangesNothing keeps a typo in the model setting
// from silently steering the address too.
func TestANamedModelNoServerHasChangesNothing(t *testing.T) {
	a := listingServer(t, "a:7b")
	b := listingServer(t, "b:7b")
	res := ResolveEndpoint(context.Background(), ResolveOptions{
		Declared:  DefaultBaseURL,
		Model:     "typo:7b",
		Endpoints: endpointsOf(a.URL, b.URL),
		Timeout:   5 * time.Second,
	})
	if res.Found != nil {
		t.Fatalf("resolved to %s on the strength of a model nothing serves", res.Found.BaseURL)
	}
	if len(res.Ambiguous) != 2 {
		t.Errorf("Ambiguous = %d, want both reported", len(res.Ambiguous))
	}
}

// TestSeveralServersAndNoConfiguredOneIsReportedNotGuessed is the refusal that
// keeps this from being the permissive default llm.Provider forbids.
func TestSeveralServersAndNoConfiguredOneIsReportedNotGuessed(t *testing.T) {
	a := listingServer(t, "model-a")
	b := listingServer(t, "model-b")
	res := resolveOver(t, endpointsOf(a.URL, b.URL), DefaultBaseURL)
	if res.BaseURL != DefaultBaseURL {
		t.Fatalf("BaseURL = %q; the harness picked between an operator's servers", res.BaseURL)
	}
	if len(res.Ambiguous) != 2 {
		t.Fatalf("Ambiguous = %d, want both servers reported", len(res.Ambiguous))
	}
	note := res.Note()
	if !strings.Contains(note, BaseURLSetting) {
		t.Errorf("Note() = %q; an ambiguity an operator must settle has to name the setting that settles it", note)
	}
}

// TestNothingFoundKeepsTheDeclaredAddress leaves the adapter's own diagnosis as
// the one an operator reads, instead of a second message about the same failure.
func TestNothingFoundKeepsTheDeclaredAddress(t *testing.T) {
	res := resolveOver(t, []Endpoint{{BaseURL: "http://127.0.0.1:1/v1"}}, DefaultBaseURL)
	if res.BaseURL != DefaultBaseURL {
		t.Fatalf("BaseURL = %q, want the declared address", res.BaseURL)
	}
	if res.Found != nil {
		t.Error("a server was reported where none answered")
	}
	if len(res.Scanned) == 0 {
		t.Error("the scan is not recorded, so a report cannot say the check ran")
	}
}

// TestSoleUsableModelRefusesToChoose holds the line the model-resolution path
// depends on: picking which weights someone's work runs against is not a
// default worth holding.
func TestSoleUsableModelRefusesToChoose(t *testing.T) {
	srv := Server{BaseURL: "http://127.0.0.1:11434/v1", Models: []ServedModel{
		{ID: "b-model"}, {ID: "a-model"},
	}}
	model, why := SoleUsableModel(srv)
	if model != "" {
		t.Fatalf("SoleUsableModel = %q; it chose between two models", model)
	}
	if !strings.Contains(why, "a-model") || !strings.Contains(why, "b-model") {
		t.Errorf("why = %q, want it to name the alternatives", why)
	}
}

// TestSoleUsableModelSaysWhenThereIsNothingToRun distinguishes an empty server
// from one whose models are all the wrong kind.
func TestSoleUsableModelSaysWhenThereIsNothingToRun(t *testing.T) {
	empty := Server{BaseURL: "http://x/v1"}
	if _, why := SoleUsableModel(empty); !strings.Contains(why, "no models") {
		t.Errorf("why = %q, want it to report an empty server", why)
	}

	embeddingOnly := Server{BaseURL: "http://x/v1", Models: []ServedModel{{
		ID:         "nomic-embed-text",
		Dimensions: Dimensions{CapabilitiesKnown: true},
	}}}
	_, why := SoleUsableModel(embeddingOnly)
	if !strings.Contains(why, "embedding") {
		t.Errorf("why = %q, want the reason each model was rejected", why)
	}
}

// resolveOver runs the real resolution against a fixed endpoint list. It exists
// so these tests exercise ResolveEndpoint rather than a copy of its rules kept
// beside it, which would pass whatever the shipped function did.
func resolveOver(t *testing.T, endpoints []Endpoint, declared string) Resolution {
	t.Helper()
	return ResolveEndpoint(context.Background(), ResolveOptions{
		Declared:  declared,
		Endpoints: endpoints,
		Timeout:   5 * time.Second,
	})
}
