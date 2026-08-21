package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Model dimensions are discovered where the server exposes them and declared
// where it does not.
//
// The package doc argues that a local server publishes no context window, so
// the operator must declare one. That is true of the OpenAI model listing and
// false of the servers this adapter actually talks to:
//
//   - Ollama's /api/show returns "<arch>.context_length" in model_info, and a
//     capabilities list naming tools, vision and thinking — three of the things
//     the flag catalogue currently asks an operator to assert by hand.
//   - vLLM puts max_model_len on each entry of /v1/models.
//   - llama.cpp's /props reports the n_ctx the server was actually started with,
//     which is the number that matters: a 262k model served with -c 8192 has an
//     8192-token window no matter what the weights say.
//   - the MLX server's /health carries effective_context_limit, which is that
//     same binding number, beside the loaded_context_size the weights allow.
//
// That last one was written off here as unanswerable — "only /v1/models and
// /health" was correct about the endpoints and wrong about the conclusion, and
// the cost of the mistake was a 262k model running against the 32k default and
// compacting history it had eight times the room for. Not every server answers
// even so. Hence: probe, fall back to the declaration, and always report which
// source won. A discovered
// window and a guessed one lead to the same request and to very different
// confidence, and an operator who cannot tell them apart cannot tell whether a
// mid-turn truncation was their configuration or their server.

// Source names where a dimension came from.
type Source string

const (
	// SourceDeclared is the operator's own setting.
	SourceDeclared Source = "declared"
	// SourceOllama is Ollama's /api/show.
	SourceOllama Source = "ollama:/api/show"
	// SourceVLLM is the max_model_len field on /v1/models.
	SourceVLLM Source = "vllm:/v1/models"
	// SourceLlamaCPP is llama.cpp's /props.
	SourceLlamaCPP Source = "llama.cpp:/props"
	// SourceMLX is the MLX server's /health.
	SourceMLX Source = "mlx:/health"
	// SourceDeclaredImplausible is the declaration, used because the server
	// published a window that cannot be real. It is a separate value from
	// SourceDeclared because the two lead an operator to different places: the
	// first means their server is reporting nonsense, the second means it
	// reports nothing at all.
	SourceDeclaredImplausible Source = "declared (the server's window was not believable)"
)

// Dimensions is what could be established about a served model.
type Dimensions struct {
	ContextWindow int
	// Source names where ContextWindow came from.
	Source Source
	// SupportsTools and SupportsReasoning are only set when the server said so;
	// Known reports whether they mean anything.
	SupportsTools     bool
	SupportsReasoning bool
	SupportsVision    bool
	// SupportsCompletion reports that the model generates text at all. It is
	// tracked separately from the rest because a local cache holds models that
	// do not: an embedding model answers /v1/models beside every chat model,
	// and offering it as something to run a turn on wastes an operator's time
	// on a failure whose cause is the model, not their configuration.
	SupportsCompletion bool
	CapabilitiesKnown  bool
	// ImplausibleWindow and RejectedSource record a window that was read off
	// the server and refused, so the refusal is reportable rather than silent.
	// Zero unless Source is SourceDeclaredImplausible.
	ImplausibleWindow int
	RejectedSource    Source
}

// Embedding reports a model the server described as embedding-only. False when
// the server published no capabilities, because unknown is not a negative.
func (d Dimensions) Embedding() bool {
	return d.CapabilitiesKnown && !d.SupportsCompletion
}

// Discovered reports whether the window came from the server rather than a
// setting.
func (d Dimensions) Discovered() bool {
	return d.Source != "" && d.Source != SourceDeclared && d.Source != SourceDeclaredImplausible
}

// Describe renders the provenance for an operator.
func (d Dimensions) Describe() string {
	if d.Source == SourceDeclaredImplausible {
		return fmt.Sprintf("%d tokens (declared; %s reported %d tokens, past the %d-token ceiling, "+
			"so it was not believed)", d.ContextWindow, d.RejectedSource, d.ImplausibleWindow,
			maxPlausibleContextWindow)
	}
	if !d.Discovered() {
		return fmt.Sprintf("%d tokens (declared; this server publishes no window)", d.ContextWindow)
	}
	return fmt.Sprintf("%d tokens (from %s)", d.ContextWindow, d.Source)
}

// ollamaShow is the useful part of Ollama's /api/show.
type ollamaShow struct {
	ModelInfo    map[string]json.RawMessage `json:"model_info"`
	Capabilities []string                   `json:"capabilities"`
}

// llamaProps is the useful part of llama.cpp's /props.
type llamaProps struct {
	DefaultGenerationSettings struct {
		NCtx int `json:"n_ctx"`
	} `json:"default_generation_settings"`
	NCtx int `json:"n_ctx"`
}

// maxDimEntries bounds the discovered-dimensions cache. It is keyed by a
// caller-supplied model string, and with assume_model_served on any string a
// caller passes reaches it, so a long-lived `manvi serve` process would
// otherwise grow this map for the life of the run. A machine serving more than
// a couple of hundred distinct models inside one TTL is not a real
// configuration; the bound exists for the unbounded-key case, not to ration a
// real server.
const maxDimEntries = 256

// maxPlausibleContextWindow bounds a window read off a server before it is
// believed.
//
// The package doc argues for a modest *declared* default because over-declaring
// a window produces a request the server truncates deep into a turn — and then
// trusted any positive *discovered* number without bound, which is the same
// mistake with the operator taken out of it. A model card carrying
// "context_length": 10000000000 yields a ten-billion-token budget the harness
// never compacts against, so every request overflows the real server mid-turn.
//
// The number: 2^24 is roughly 16.8M tokens, comfortably above the largest
// window any released open-weight model declares (Llama 4 Scout's 10,485,760),
// so no real server trips it, while a garbage or unit-confused field is orders
// of magnitude past it. A window above this is not clamped to some number
// nobody chose — it falls back to the operator's declaration, and Describe()
// says that is what happened.
const maxPlausibleContextWindow = 1 << 24

// maxProbeBytes bounds every discovery read. A server answering these endpoints
// with an unbounded stream must not be able to exhaust the harness.
const maxProbeBytes = 4 << 20

// probeDimensions asks the server what it can, in cheapest-first order, and
// stops at the first definite answer.
//
// Every probe failure is silent and non-fatal by design: these endpoints are
// server-specific, so a 404 is the expected answer from the two thirds of
// servers that are not the one being probed. What must never be silent is the
// *result*, which carries its own provenance.
func (a *Adapter) probeDimensions(ctx context.Context, model string) Dimensions {
	declared := a.declaredDimensions()

	// AssumeModelServed means the operator asked the harness not to interrogate
	// their server. Probing for dimensions would contradict that, so it implies
	// trusting the declaration here too.
	if a.cfg.TrustDeclaredContext || a.cfg.AssumeModelServed {
		return declared
	}

	ctx, cancel := context.WithTimeout(ctx, a.cfg.DiscoveryTimeout)
	defer cancel()

	if d, ok := a.probeVLLM(ctx, model); ok {
		return believable(d, declared)
	}
	if d, ok := a.probeOllama(ctx, model); ok {
		return believable(d, declared)
	}
	if d, ok := a.probeLlamaCPP(ctx); ok {
		return believable(d, declared)
	}
	if d, ok := a.probeMLX(ctx, model); ok {
		return believable(d, declared)
	}
	return declared
}

// declaredDimensions is what the operator said, which is the answer whenever
// the server will not give one.
func (a *Adapter) declaredDimensions() Dimensions {
	return Dimensions{ContextWindow: a.cfg.ContextWindow, Source: SourceDeclared}
}

// believable refuses a discovered window past maxPlausibleContextWindow and
// falls back to the declaration, keeping what was rejected so an operator can
// see it.
//
// What the same response said about tools, vision and thinking survives: those
// are separate fields answering separate questions, and discarding a model's
// vision capability because a number beside it was wrong would trade one silent
// misconfiguration for another.
func believable(d, declared Dimensions) Dimensions {
	if d.ContextWindow <= maxPlausibleContextWindow {
		return d
	}
	out := d
	out.ContextWindow = declared.ContextWindow
	out.ImplausibleWindow = d.ContextWindow
	out.RejectedSource = d.Source
	out.Source = SourceDeclaredImplausible
	return out
}

// probeVLLM reads the window off the model listing the adapter already
// fetched. It issues no request of its own: the listing and the window come
// from the same endpoint, and asking twice would double every startup.
func (a *Adapter) probeVLLM(ctx context.Context, model string) (Dimensions, bool) {
	if _, err := a.discover(ctx); err != nil {
		return Dimensions{}, false
	}
	a.dimMu.Lock()
	window := a.listedWindows[model]
	a.dimMu.Unlock()
	if window <= 0 {
		return Dimensions{}, false
	}
	return Dimensions{ContextWindow: window, Source: SourceVLLM}, true
}

func (a *Adapter) probeOllama(ctx context.Context, model string) (Dimensions, bool) {
	// /api/show sits beside the OpenAI-compatible surface, not under it: the
	// base URL ends in /v1 and this endpoint does not.
	base, ok := a.nativeRoot()
	if !ok {
		return Dimensions{}, false
	}
	payload, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return Dimensions{}, false
	}
	body, err := a.postBoundedAbsolute(ctx, base+"/api/show", payload)
	if err != nil {
		return Dimensions{}, false
	}
	var show ollamaShow
	if json.Unmarshal(body, &show) != nil {
		return Dimensions{}, false
	}

	window := contextLengthFrom(show.ModelInfo)
	if window == 0 {
		return Dimensions{}, false
	}

	d := Dimensions{ContextWindow: window, Source: SourceOllama, CapabilitiesKnown: true}
	for _, c := range show.Capabilities {
		switch c {
		case "tools":
			d.SupportsTools = true
		case "thinking":
			d.SupportsReasoning = true
		case "vision":
			d.SupportsVision = true
		case "completion":
			d.SupportsCompletion = true
		}
	}
	return d, true
}

// contextLengthFrom picks the text context window out of Ollama's model_info.
//
// The key is architecture-prefixed — "qwen3_5.context_length" — and the
// architecture is whatever the model happens to be, so it is matched by suffix
// rather than guessed. More than one key can match: a vision-language model
// carries "<arch>.vision.context_length" beside "<arch>.context_length", and
// the first is the window of the vision tower, not of the text model the
// harness budgets against. Ranging the map and taking the first suffix match
// therefore gave one server two answers — 262144 tokens in one process and 1024
// in the next, both reported as authoritative — because Go randomises map
// iteration order.
//
// Determinism alone would not fix it; picking 1024 every time is just as wrong.
// The rule, in order:
//
//  1. model_info names its own architecture in "general.architecture", and
//     "<that>.context_length" is the model's own statement of its text window.
//     Where the server publishes it, that is the answer and nothing else is
//     consulted.
//  2. Otherwise the shallowest key wins: an extra segment before
//     ".context_length" namespaces the value under a component — vision, audio,
//     a bundled encoder — and a component's window is not the model's.
//  3. Ties break on the lexicographically smaller key, so a card the first two
//     rules cannot separate still yields the same budget in every process.
func contextLengthFrom(info map[string]json.RawMessage) int {
	if arch, ok := stringField(info["general.architecture"]); ok {
		if n, ok := positiveInt(info[arch+".context_length"]); ok {
			return n
		}
	}
	bestKey, best := "", 0
	for key, raw := range info {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		n, ok := positiveInt(raw)
		if !ok {
			continue
		}
		if bestKey == "" || moreSpecificContextKey(key, bestKey) {
			bestKey, best = key, n
		}
	}
	return best
}

// moreSpecificContextKey reports whether candidate describes the text model
// more nearly than incumbent does: fewer namespace segments first, then the
// lexicographically smaller key so the choice never depends on map order.
func moreSpecificContextKey(candidate, incumbent string) bool {
	c, i := strings.Count(candidate, "."), strings.Count(incumbent, ".")
	if c != i {
		return c < i
	}
	return candidate < incumbent
}

func stringField(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || s == "" {
		return "", false
	}
	return s, true
}

func positiveInt(raw json.RawMessage) (int, bool) {
	var n int
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func (a *Adapter) probeLlamaCPP(ctx context.Context) (Dimensions, bool) {
	base, ok := a.nativeRoot()
	if !ok {
		return Dimensions{}, false
	}
	body, err := a.getBoundedAbsolute(ctx, base+"/props")
	if err != nil {
		return Dimensions{}, false
	}
	var props llamaProps
	if json.Unmarshal(body, &props) != nil {
		return Dimensions{}, false
	}
	window := props.DefaultGenerationSettings.NCtx
	if window <= 0 {
		window = props.NCtx
	}
	if window <= 0 {
		return Dimensions{}, false
	}
	// This is the context the server was actually started with, which may be
	// far below what the weights support. It is the binding constraint, so it
	// wins over anything the model card would say.
	return Dimensions{ContextWindow: window, Source: SourceLlamaCPP}, true
}

// mlxHealth is the part of the MLX server's /health this probe reads.
type mlxHealth struct {
	LoadedModel string `json:"loaded_model"`
	// EffectiveContextLimit is the binding window: loaded_context_size unless
	// the operator capped the server below it, in which case it is the cap.
	EffectiveContextLimit int `json:"effective_context_limit"`
	LoadedContextSize     int `json:"loaded_context_size"`
}

// probeMLX reads the window off the MLX server's /health.
//
// The model check is what keeps this honest. /health describes whichever model
// is loaded right now, and this server loads on demand — so a window read while
// a different model is resident was never measured for the one being asked
// about. When they disagree the declaration is the better answer, because a
// borrowed window is worse than an admitted guess: it is wrong and it looks
// discovered.
//
// Deliberately not cached, unlike the model listing. /health describes the
// model resident at the moment it is asked, and this server swaps models on
// demand — a cached answer would go stale exactly when it matters. The request
// is a localhost GET against a server the harness is already talking to.
func (a *Adapter) probeMLX(ctx context.Context, model string) (Dimensions, bool) {
	base, ok := a.nativeRoot()
	if !ok {
		return Dimensions{}, false
	}
	body, err := a.getBoundedAbsolute(ctx, base+"/health")
	if err != nil {
		return Dimensions{}, false
	}
	var health mlxHealth
	if json.Unmarshal(body, &health) != nil {
		return Dimensions{}, false
	}
	if health.LoadedModel == "" || health.LoadedModel != model {
		return Dimensions{}, false
	}
	window := health.EffectiveContextLimit
	if window <= 0 {
		window = health.LoadedContextSize
	}
	if window <= 0 {
		return Dimensions{}, false
	}
	return Dimensions{ContextWindow: window, Source: SourceMLX}, true
}

// nativeRoot strips the OpenAI-compatible suffix to reach a server's own API.
func (a *Adapter) nativeRoot() (string, bool) {
	base := strings.TrimRight(a.cfg.BaseURL, "/")
	trimmed := strings.TrimSuffix(base, "/v1")
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

func (a *Adapter) getBoundedAbsolute(ctx context.Context, url string) ([]byte, error) {
	resp, err := a.Client().Probe(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(io.LimitReader(resp.Body, maxProbeBytes))
}

func (a *Adapter) postBoundedAbsolute(ctx context.Context, url string, payload []byte) ([]byte, error) {
	resp, err := a.Client().Probe(ctx, "POST", url, payload)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(io.LimitReader(resp.Body, maxProbeBytes))
}

// dimensionsCached returns the discovered dimensions, probing at most once per
// TTL. The TTL is shared with model discovery because both answer the same
// question — what is this server serving right now — and a model pulled or
// restarted changes both together.
func (a *Adapter) dimensionsFor(ctx context.Context, model string) Dimensions {
	a.dimMu.Lock()
	if entry, ok := a.dims[model]; ok && a.now().Sub(entry.at) < a.cfg.DiscoveryTTL {
		a.dimMu.Unlock()
		return entry.dims
	}
	// Holding the lock across the probe would serialise every caller behind one
	// network round trip, but releasing it unguarded lets a cold fan-out issue
	// one probe per caller: 64 sub-agents calling Capability produced 64
	// identical /api/show posts, and twice that against a server shaped like
	// MLX, where the probe chain runs to the end before falling back. This is
	// not a startup-only cost — every DiscoveryTTL expiry makes every caller
	// miss at once. dimInflight makes the losers wait for the winner's answer,
	// exactly as a.inflight does for model discovery.
	if a.dimInflight[model] {
		a.dimMu.Unlock()
		return a.awaitDimensions(ctx, model)
	}
	if a.dimInflight == nil {
		a.dimInflight = map[string]bool{}
	}
	a.dimInflight[model] = true
	a.dimMu.Unlock()

	d := a.probeDimensions(ctx, model)

	a.dimMu.Lock()
	delete(a.dimInflight, model)
	a.rememberDimensionsLocked(model, d)
	a.dimMu.Unlock()
	return d
}

// awaitDimensions waits for whichever caller is already probing this model,
// bounded by the caller's context and by the discovery timeout, so a wedged
// probe cannot strand every other goroutine behind it.
//
// A wait that runs out returns the declaration rather than an error, because
// dimensions have no error channel: the declaration is what this adapter falls
// back to whenever the server will not answer, and it errs in the safe
// direction — a window too small compacts early, a window too large truncates
// mid-turn.
func (a *Adapter) awaitDimensions(ctx context.Context, model string) Dimensions {
	deadline := time.NewTimer(a.cfg.DiscoveryTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return a.declaredDimensions()
		case <-deadline.C:
			return a.declaredDimensions()
		case <-tick.C:
			a.dimMu.Lock()
			entry, cached := a.dims[model]
			inflight := a.dimInflight[model]
			a.dimMu.Unlock()
			if inflight {
				continue
			}
			if cached {
				return entry.dims
			}
			// The winner finished and left nothing to read, which the bound on
			// the cache makes possible. Re-probing here would reopen the
			// stampede this function exists to close, so the declaration is the
			// answer for this call.
			return a.declaredDimensions()
		}
	}
}

// rememberDimensionsLocked caches one probe result and keeps the cache bounded.
// Expired entries go first because they are already worthless; only if that is
// not enough does the oldest live entry go, which costs at most one re-probe on
// the next call for it.
func (a *Adapter) rememberDimensionsLocked(model string, d Dimensions) {
	if a.dims == nil {
		a.dims = map[string]dimEntry{}
	}
	if _, replacing := a.dims[model]; !replacing {
		now := a.now()
		if len(a.dims) >= maxDimEntries {
			for key, entry := range a.dims {
				if now.Sub(entry.at) >= a.cfg.DiscoveryTTL {
					delete(a.dims, key)
				}
			}
		}
		for len(a.dims) >= maxDimEntries {
			oldestKey, oldest := "", time.Time{}
			for key, entry := range a.dims {
				if oldestKey == "" || entry.at.Before(oldest) {
					oldestKey, oldest = key, entry.at
				}
			}
			delete(a.dims, oldestKey)
		}
	}
	a.dims[model] = dimEntry{dims: d, at: a.now()}
}

type dimEntry struct {
	dims Dimensions
	at   time.Time
}
