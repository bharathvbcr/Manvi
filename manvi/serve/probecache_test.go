package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm/local"
)

// BenchmarkProbeRepeated is a host probing the same model on the path to every
// request it assembles — the use the field comments on ProbeParams describe.
// requests/op is the number that matters: each one is a round trip made while
// every other call on this sidecar waits behind it.
func BenchmarkProbeRepeated(b *testing.B) {
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"data":[{"id":"qwen3:8b"}]}`)
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"model_info":{"qwen3.context_length":40960},"capabilities":["tools","completion"]}`)
	})
	stub := httptest.NewServer(mux)
	b.Cleanup(stub.Close)

	raw, err := json.Marshal(ProbeParams{BaseURL: stub.URL + "/v1", Model: "qwen3:8b"})
	if err != nil {
		b.Fatal(err)
	}
	s := New(io.Discard, hostOpts())
	b.ReportAllocs()
	for b.Loop() {
		if _, perr := s.probe(context.Background(), raw); perr != nil {
			b.Fatal(perr)
		}
	}
	b.ReportMetric(float64(hits.Load())/float64(b.N), "requests/op")
}

// countingStub is an Ollama-shaped server that counts every request, so a test
// can tell a reused answer from a fresh one by whether the server was asked.
type countingStub struct {
	*httptest.Server
	hits atomic.Int64
	// listing, when set, replaces the model listing body.
	listing atomic.Pointer[string]
	// show, when false, makes /api/show a 404 — a server that publishes no
	// window, so dimensions fall back to the declaration.
	show atomic.Bool
}

func newCountingStub(t *testing.T, model string) *countingStub {
	t.Helper()
	c := &countingStub{}
	c.show.Store(true)
	listing := fmt.Sprintf(`{"data":[{"id":%q}]}`, model)
	c.listing.Store(&listing)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		fmt.Fprint(w, *c.listing.Load())
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		if !c.show.Load() {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"model_info":{"qwen3.context_length":40960},"capabilities":["tools","completion"]}`)
	})
	c.Server = httptest.NewServer(mux)
	t.Cleanup(c.Close)
	return c
}

func (c *countingStub) setListing(body string) { c.listing.Store(&body) }

func probeOnce(t *testing.T, s *Server, p ProbeParams) (ProbeResult, *Error) {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	got, perr := s.probe(context.Background(), raw)
	if perr != nil {
		return ProbeResult{}, perr
	}
	return got.(ProbeResult), nil
}

// fakeClock is a probe-cache clock a test moves by hand.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newProbeServer() (*Server, *fakeClock) {
	s := New(io.Discard, hostOpts())
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s.probes.now = clock.now
	return s, clock
}

func TestProbeReusesTheAdapterForTheSameConfiguration(t *testing.T) {
	stub := newCountingStub(t, "qwen3:8b")
	s, _ := newProbeServer()
	p := ProbeParams{BaseURL: stub.URL + "/v1", Model: "qwen3:8b"}

	first, perr := probeOnce(t, s, p)
	if perr != nil {
		t.Fatalf("probe failed: %+v", perr)
	}
	asked := stub.hits.Load()
	if asked == 0 {
		t.Fatal("the first probe asked the server nothing")
	}
	second, perr := probeOnce(t, s, p)
	if perr != nil {
		t.Fatalf("probe failed: %+v", perr)
	}
	if got := stub.hits.Load(); got != asked {
		t.Errorf("a repeated probe of one configuration made %d more requests, want 0", got-asked)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("the reused answer differs from the fresh one:\n%+v\n%+v", first, second)
	}
}

// The objection the per-probe adapter answered: a window discovered against a
// base URL the operator has since changed.
func TestProbeMissesWhenTheBaseURLChanges(t *testing.T) {
	a := newCountingStub(t, "qwen3:8b")
	b := newCountingStub(t, "qwen3:8b")
	s, _ := newProbeServer()

	if _, perr := probeOnce(t, s, ProbeParams{BaseURL: a.URL + "/v1", Model: "qwen3:8b"}); perr != nil {
		t.Fatalf("probe failed: %+v", perr)
	}
	if _, perr := probeOnce(t, s, ProbeParams{BaseURL: b.URL + "/v1", Model: "qwen3:8b"}); perr != nil {
		t.Fatalf("probe failed: %+v", perr)
	}
	if b.hits.Load() == 0 {
		t.Error("a probe of a new base URL was answered without asking that server")
	}
}

func TestProbeMissesWhenAnyOtherSettingChanges(t *testing.T) {
	stub := newCountingStub(t, "qwen3:8b")
	s, _ := newProbeServer()
	base := ProbeParams{BaseURL: stub.URL + "/v1", Model: "qwen3:8b"}
	if _, perr := probeOnce(t, s, base); perr != nil {
		t.Fatalf("probe failed: %+v", perr)
	}
	for _, changed := range []ProbeParams{
		{BaseURL: base.BaseURL, Model: base.Model, DeclaredContextWindow: 8192},
		{BaseURL: base.BaseURL, Model: base.Model, MaxOutputTokens: 4096},
		{BaseURL: base.BaseURL, Model: base.Model, TimeoutMS: 1500},
		{BaseURL: base.BaseURL, Model: base.Model, TrustDeclared: true},
	} {
		before := stub.hits.Load()
		if _, perr := probeOnce(t, s, changed); perr != nil {
			t.Fatalf("probe %+v failed: %+v", changed, perr)
		}
		if stub.hits.Load() == before {
			t.Errorf("probe %+v was answered by the adapter built for a different configuration", changed)
		}
	}
}

func TestProbeKeptAdaptersExpireWithTheDiscoveryTTL(t *testing.T) {
	stub := newCountingStub(t, "qwen3:8b")
	s, clock := newProbeServer()
	p := ProbeParams{BaseURL: stub.URL + "/v1", Model: "qwen3:8b"}
	if _, perr := probeOnce(t, s, p); perr != nil {
		t.Fatalf("probe failed: %+v", perr)
	}

	clock.t = clock.t.Add(local.DefaultDiscoveryTTL - time.Second)
	before := stub.hits.Load()
	if _, perr := probeOnce(t, s, p); perr != nil {
		t.Fatalf("probe failed: %+v", perr)
	}
	if stub.hits.Load() != before {
		t.Error("a probe inside the TTL asked the server again")
	}

	clock.t = clock.t.Add(2 * time.Second)
	if _, perr := probeOnce(t, s, p); perr != nil {
		t.Fatalf("probe failed: %+v", perr)
	}
	if stub.hits.Load() == before {
		t.Error("a probe past the TTL was answered from the expired adapter")
	}
}

// Unreachable is retryable because starting the server and probing again is
// the fix. The adapter caches a failure for its TTL, so keeping it would turn
// that retry into the same failure for a minute.
func TestProbeDoesNotKeepAFailure(t *testing.T) {
	stub := newCountingStub(t, "qwen3:8b")
	s, _ := newProbeServer()
	p := ProbeParams{BaseURL: stub.URL + "/v1", Model: "qwen3:8b"}

	stub.setListing(`not a listing`)
	if _, perr := probeOnce(t, s, p); perr == nil || perr.Code != ErrUnreachable {
		t.Fatalf("probe of a broken listing = %+v, want %s", perr, ErrUnreachable)
	}
	stub.setListing(`{"data":[{"id":"qwen3:8b"}]}`)
	if _, perr := probeOnce(t, s, p); perr != nil {
		t.Fatalf("the retry after the server recovered failed: %+v", perr)
	}
}

// Not served is fixed by pulling the model, and the probe after the pull must
// see it even though an adapter for this configuration was kept from before.
func TestProbeSeesAModelPulledSinceTheKeptListing(t *testing.T) {
	stub := newCountingStub(t, "qwen3:8b")
	s, _ := newProbeServer()
	if _, perr := probeOnce(t, s, ProbeParams{BaseURL: stub.URL + "/v1", Model: "qwen3:8b"}); perr != nil {
		t.Fatalf("probe failed: %+v", perr)
	}
	stub.setListing(`{"data":[{"id":"qwen3:8b"},{"id":"llama3.1:70b"}]}`)
	got, perr := probeOnce(t, s, ProbeParams{BaseURL: stub.URL + "/v1", Model: "llama3.1:70b"})
	if perr != nil {
		t.Fatalf("a model pulled after the kept listing was refused: %+v", perr)
	}
	if got.Model != "llama3.1:70b" {
		t.Errorf("model = %q", got.Model)
	}

	// And a model that really is absent is still refused as itself.
	_, perr = probeOnce(t, s, ProbeParams{BaseURL: stub.URL + "/v1", Model: "absent"})
	if perr == nil || perr.Code != ErrNotServed || perr.Retryable {
		t.Errorf("absent model = %+v, want a non-retryable %s", perr, ErrNotServed)
	}
}

// A window that fell back to the declaration is what a probe that got nothing
// back reports, and the next probe must ask again rather than repeat it.
func TestProbeDoesNotKeepADeclaredFallback(t *testing.T) {
	stub := newCountingStub(t, "qwen3:8b")
	stub.show.Store(false)
	s, _ := newProbeServer()
	p := ProbeParams{BaseURL: stub.URL + "/v1", Model: "qwen3:8b", DeclaredContextWindow: 8192}
	got, perr := probeOnce(t, s, p)
	if perr != nil {
		t.Fatalf("probe failed: %+v", perr)
	}
	if got.Discovered {
		t.Fatalf("stub without /api/show reported a discovered window: %+v", got)
	}
	stub.show.Store(true)
	got, perr = probeOnce(t, s, p)
	if perr != nil {
		t.Fatalf("probe failed: %+v", perr)
	}
	if !got.Discovered || got.ContextWindow != 40960 {
		t.Errorf("the probe after the server began answering = %+v, want the discovered 40960", got)
	}

	// The operator's own declaration is the server's answer for this config,
	// so that one is kept.
	trust := ProbeParams{BaseURL: stub.URL + "/v1", Model: "qwen3:8b", DeclaredContextWindow: 8192, TrustDeclared: true}
	if _, perr := probeOnce(t, s, trust); perr != nil {
		t.Fatalf("probe failed: %+v", perr)
	}
	before := stub.hits.Load()
	if _, perr := probeOnce(t, s, trust); perr != nil {
		t.Fatalf("probe failed: %+v", perr)
	}
	if stub.hits.Load() != before {
		t.Error("a trusted declaration was not reused")
	}
}

// The key is the whole local.Config. Every field changes what the adapter
// does, so a field the key ignored would let two configurations share an
// adapter built for one of them. This walks the fields rather than listing
// them, so a field added to Config later is held to the same rule.
func TestProbeKeyCoversEveryConfigField(t *testing.T) {
	base := local.Config{}
	baseKey, ok := probeKey(base)
	if !ok {
		t.Fatal("the zero config has no key")
	}
	typ := reflect.TypeOf(base)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		changed := base
		v := reflect.ValueOf(&changed).Elem().Field(i)
		if !v.CanSet() {
			t.Errorf("local.Config.%s is unexported, so the JSON key cannot see it", field.Name)
			continue
		}
		switch v.Kind() {
		case reflect.String:
			v.SetString("x")
		case reflect.Int, reflect.Int64:
			v.SetInt(7)
		case reflect.Bool:
			v.SetBool(true)
		case reflect.Pointer:
			elem := reflect.New(field.Type.Elem())
			switch elem.Elem().Kind() {
			case reflect.Float64:
				elem.Elem().SetFloat(0.5)
			case reflect.Int:
				elem.Elem().SetInt(3)
			default:
				t.Fatalf("local.Config.%s: no test value for %s", field.Name, field.Type)
			}
			v.Set(elem)
		case reflect.Slice:
			v.Set(reflect.ValueOf([]string{"</s>"}))
		default:
			t.Fatalf("local.Config.%s: no test value for %s", field.Name, field.Type)
		}
		if field.Tag.Get("json") == "-" {
			t.Errorf("local.Config.%s is excluded from JSON, so the key cannot see it", field.Name)
		}
		key, ok := probeKey(changed)
		if !ok {
			t.Fatalf("local.Config.%s: no key", field.Name)
		}
		if key == baseKey {
			t.Errorf("changing local.Config.%s does not change the probe key", field.Name)
		}
	}

	// Two configs that are equal but not == — separate pointers to the same
	// value — must share a key, or the cache never hits for a config that
	// sets one.
	one, two := 0.7, 0.7
	k1, _ := probeKey(local.Config{Temperature: &one})
	k2, _ := probeKey(local.Config{Temperature: &two})
	if k1 != k2 {
		t.Error("equal configs built from separate pointers have different keys")
	}
}

func TestProbeKeptAdaptersAreBounded(t *testing.T) {
	stub := newCountingStub(t, "qwen3:8b")
	s, _ := newProbeServer()
	for i := 0; i < maxProbeAdapters*3; i++ {
		p := ProbeParams{BaseURL: stub.URL + "/v1", Model: "qwen3:8b", DeclaredContextWindow: 1024 + i}
		if _, perr := probeOnce(t, s, p); perr != nil {
			t.Fatalf("probe failed: %+v", perr)
		}
	}
	if n := len(s.probes.entries); n > maxProbeAdapters {
		t.Errorf("%d adapters kept, bound is %d", n, maxProbeAdapters)
	}
}
