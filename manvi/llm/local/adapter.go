package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"manvi/credentials"
	"manvi/llm"
	"manvi/llm/openaicompat"
)

// Adapter drives an OpenAI-compatible server on the operator's machine.
type Adapter struct {
	*openaicompat.Adapter
	cfg Config

	// mu guards the discovery cache. Capability is reached from the agent loop
	// and from every fan-out sub-agent, so this is genuinely concurrent rather
	// than defensively locked.
	mu       sync.Mutex
	served   map[string]struct{}
	fetched  time.Time
	lastErr  error
	inflight bool
	// now is injected for tests, so TTL behaviour is asserted rather than
	// slept through.
	now func() time.Time

	// dimMu guards the discovered-dimensions cache, which is keyed by model
	// because one server can serve several with different windows.
	dimMu sync.Mutex
	dims  map[string]dimEntry
	// dimInflight names the models a probe is currently running for, so a cold
	// fan-out coalesces onto one probe instead of one per caller. It is the
	// same bargain a.inflight strikes for model discovery, kept in the same
	// shape so this package has one idiom for it rather than two.
	dimInflight map[string]bool
	// listedWindows holds max_model_len values seen on the model listing, so
	// the vLLM answer costs no extra request.
	listedWindows map[string]int
}

// New builds an adapter for the configured server.
//
// resolve may return a missing credential without that being fatal: most local
// servers ignore authorisation entirely, and requiring a key to talk to a
// process on loopback would be ceremony that protects nothing. When a
// credential is present it is sent, because some operators front their server
// with a proxy that does check.
func New(cfg Config, resolve func() (credentials.Secret, error)) *Adapter {
	cfg = cfg.withDefaults()
	a := &Adapter{cfg: cfg, now: time.Now}
	a.Adapter = openaicompat.New(openaicompat.Options{
		Name:             Name,
		BaseURL:          cfg.BaseURL,
		DefaultBaseURL:   DefaultBaseURL,
		DefaultMaxTokens: cfg.MaxOutputTokens,
		Validate:         a.validate,
		// Only sent when the operator declared that their server understands
		// it. Capability.Validate has already refused an effort level on a
		// server declared not to support reasoning, so by the time a request is
		// built this flag and the catalogue agree.
		SendReasoningEffort:    cfg.SupportsReasoning,
		StallTimeout:           cfg.StallTimeout,
		AssumeReasoningPrefill: cfg.AssumeReasoningPrefill,
		Header: func() (http.Header, error) {
			h := http.Header{}
			secret, err := resolve()
			if err != nil {
				var missing *credentials.ErrMissing
				if errors.As(err, &missing) {
					// Absent is the normal case on loopback. Send no
					// Authorization header at all rather than an empty bearer,
					// which some servers reject as malformed where they would
					// have accepted no header.
					return h, nil
				}
				return nil, err
			}
			if secret.Present() {
				h.Set("Authorization", "Bearer "+secret.Reveal())
			}
			return h, nil
		},
	})
	return a
}

// Capability describes a model this adapter serves.
//
// The bool is false both for a model the server does not serve and for a server
// that could not be reached. Those are different conditions and the caller that
// needs to tell them apart is Stream, which reports the distinction through
// validate; Capability keeps llm.Provider's contract, under which false means
// only "this adapter does not serve it".
func (a *Adapter) Capability(model string) (llm.Capability, bool) {
	if strings.TrimSpace(model) == "" {
		return llm.Capability{}, false
	}
	if a.cfg.AssumeModelServed {
		return a.capabilityFor(context.Background(), model), true
	}
	served, err := a.discover(context.Background())
	if err != nil {
		return llm.Capability{}, false
	}
	if _, ok := served[model]; !ok {
		return llm.Capability{}, false
	}
	return a.capabilityFor(context.Background(), model), true
}

// capabilityFor merges what the server published with what the operator
// declared.
//
// Discovery wins on the context window, because a number read from the server
// beats a number typed into a setting — under-declaring it is the difference
// between compacting at 5% of a model's capacity and using it. Declared
// capabilities win where the operator turned something *off*, since that is a
// deliberate restriction rather than an unknown, and a harness that overruled
// it would be arguing with its operator about their own machine.
func (a *Adapter) capabilityFor(ctx context.Context, model string) llm.Capability {
	cap := a.cfg.baseCapability(model)
	dims := a.dimensionsFor(ctx, model)
	if dims.ContextWindow > 0 {
		cap.ContextWindow = dims.ContextWindow
		// The discovered window is the binding constraint, so the declared
		// output cap is re-clamped against it. A discovered window *smaller*
		// than the declared cap is the common case, not an exotic one: a 262k
		// model served with -c 8192 meets the shipped 16384-token default every
		// time llama.cpp starts.
		cap.MaxOutputTokens = outputCapFor(a.cfg.MaxOutputTokens, dims.ContextWindow)
	}
	if dims.CapabilitiesKnown {
		cap.SupportsTools = cap.SupportsTools && dims.SupportsTools
		cap.SupportsImages = dims.SupportsVision
		if !a.cfg.SupportsReasoning && dims.SupportsReasoning {
			// Discovering that a server understands thinking does not mean it
			// accepts the reasoning_effort field, which is a separate wire
			// question the operator still declares. Effort levels stay off.
			cap.SupportsReasoning = false
		}
	}
	return cap
}

// BaseURL is the address this adapter actually talks to.
//
// It is not always the configured one: when the operator left llm.local.base_url
// at its shipped guess, ResolveEndpoint may have found a server elsewhere. A
// caller reporting what a run will do has to be able to say which.
func (a *Adapter) BaseURL() string { return a.cfg.BaseURL }

// Dimensions reports what was established about a model and where each answer
// came from, for `manvi doctor` and `manvi probe`.
func (a *Adapter) Dimensions(ctx context.Context, model string) Dimensions {
	return a.dimensionsFor(ctx, model)
}

// Models reports what the server currently serves, for `manvi providers` and
// for a refusal that can name the alternatives.
func (a *Adapter) Models(ctx context.Context) ([]string, error) {
	served, err := a.discover(ctx)
	if err != nil {
		return nil, err
	}
	return sortedCopy(served), nil
}

// Stream begins a model call, applying configured local sampling parameters when unset.
func (a *Adapter) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	if req.Temperature == nil && a.cfg.Temperature != nil {
		req.Temperature = a.cfg.Temperature
	}
	if req.TopP == nil && a.cfg.TopP != nil {
		req.TopP = a.cfg.TopP
	}
	if req.MinP == nil && a.cfg.MinP != nil {
		req.MinP = a.cfg.MinP
	}
	if req.RepetitionPenalty == nil && a.cfg.RepetitionPenalty != nil {
		req.RepetitionPenalty = a.cfg.RepetitionPenalty
	}
	if req.TopK == nil && a.cfg.TopK != nil {
		req.TopK = a.cfg.TopK
	}
	if req.PresencePenalty == nil && a.cfg.PresencePenalty != nil {
		req.PresencePenalty = a.cfg.PresencePenalty
	}
	if req.FrequencyPenalty == nil && a.cfg.FrequencyPenalty != nil {
		req.FrequencyPenalty = a.cfg.FrequencyPenalty
	}
	if req.Seed == nil && a.cfg.Seed != nil {
		req.Seed = a.cfg.Seed
	}
	if len(req.Stop) == 0 && len(a.cfg.Stop) > 0 {
		req.Stop = append([]string(nil), a.cfg.Stop...)
	}
	// Validated here, before the default output bound is resolved, and again by
	// the wire adapter on the way out. Resolving the bound means asking what the
	// model's window is, and a request naming no model — or one this server does
	// not serve — has no window to ask about: doing it the other way round
	// contacts the server on behalf of a request that is already refused, and
	// reports a probe failure in place of the naming error the operator needs.
	if err := a.validate(req); err != nil {
		return nil, err
	}
	if req.MaxTokens <= 0 {
		// openaicompat.Options.DefaultMaxTokens is fixed when the adapter is
		// constructed, from the declared cap, and discovery happens later — so
		// a request that names no MaxTokens would ship max_tokens: 16384 to a
		// server started with an 8192-token window for the whole life of the
		// process. The bound has to be applied here, where the discovered
		// window is known, rather than left to a value frozen before it was.
		if cap := a.capabilityFor(ctx, req.Model); cap.MaxOutputTokens > 0 {
			req.MaxTokens = cap.MaxOutputTokens
		}
	}
	return a.Adapter.Stream(ctx, req)
}

// validate is the pre-send check. Unlike Capability it distinguishes the two
// failure modes, because this is where an operator reads the error.
func (a *Adapter) validate(req llm.Request) error {
	if strings.TrimSpace(req.Model) == "" {
		return errors.New("local: no model named; set llm.local.model or MANVI_MODEL")
	}
	if a.cfg.AssumeModelServed {
		return a.capabilityFor(context.Background(), req.Model).Validate(req)
	}
	served, err := a.discover(context.Background())
	if err != nil {
		return err
	}
	if _, ok := served[req.Model]; !ok {
		return &ErrNotServed{BaseURL: a.cfg.BaseURL, Model: req.Model, Served: sortedCopy(served)}
	}
	return a.capabilityFor(context.Background(), req.Model).Validate(req)
}

type modelList struct {
	Data []struct {
		ID string `json:"id"`
		// MaxModelLen is vLLM's addition to the model card. It is read here,
		// on the listing request the adapter already makes, rather than by a
		// second request to the same endpoint.
		MaxModelLen int `json:"max_model_len"`
	} `json:"data"`
}

// discover returns the served model set, refreshing it when the cache is older
// than the TTL.
//
// A failure is cached for the same TTL as a success. Without that, a down
// server turns every Capability call on a fan-out into its own connection
// attempt, and the harness spends a turn timing out in parallel instead of
// failing once.
func (a *Adapter) discover(ctx context.Context) (map[string]struct{}, error) {
	a.mu.Lock()
	fresh := a.now().Sub(a.fetched) < a.cfg.DiscoveryTTL
	if fresh && (a.served != nil || a.lastErr != nil) {
		served, err := a.served, a.lastErr
		a.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return served, nil
	}
	// Holding the lock across the request would serialise every caller behind
	// one network round trip, but releasing it lets several racing callers each
	// issue their own. inflight makes the losers wait for the winner's result
	// instead, which is the point of caching a slow answer.
	if a.inflight {
		a.mu.Unlock()
		return a.awaitInflight(ctx)
	}
	a.inflight = true
	a.mu.Unlock()

	served, err := a.fetchModels(ctx)

	a.mu.Lock()
	a.inflight = false
	a.fetched = a.now()
	a.served, a.lastErr = served, err
	a.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return served, nil
}

// awaitInflight waits for whichever caller is already discovering, bounded by
// the caller's context and by the discovery timeout, so a wedged request cannot
// strand every other goroutine behind it.
func (a *Adapter) awaitInflight(ctx context.Context) (map[string]struct{}, error) {
	deadline := time.NewTimer(a.cfg.DiscoveryTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, &ErrUndiscoverable{BaseURL: a.cfg.BaseURL, Err: ctx.Err()}
		case <-deadline.C:
			return nil, &ErrUndiscoverable{
				BaseURL: a.cfg.BaseURL,
				Err:     fmt.Errorf("timed out after %s waiting for an in-flight model discovery", a.cfg.DiscoveryTimeout),
			}
		case <-tick.C:
			a.mu.Lock()
			done := !a.inflight
			served, err := a.served, a.lastErr
			a.mu.Unlock()
			if done {
				if err != nil {
					return nil, err
				}
				if served != nil {
					return served, nil
				}
			}
		}
	}
}

func (a *Adapter) fetchModels(ctx context.Context) (map[string]struct{}, error) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.DiscoveryTimeout)
	defer cancel()

	resp, err := a.Client().Get(ctx, openaicompat.ModelsPath)
	if err != nil {
		return nil, &ErrUndiscoverable{BaseURL: a.cfg.BaseURL, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded: a server answering this endpoint with an unbounded stream must
	// not be able to exhaust the harness.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelListBytes))
	if err != nil {
		return nil, &ErrUndiscoverable{BaseURL: a.cfg.BaseURL, Err: err}
	}

	var list modelList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, &ErrUndiscoverable{
			BaseURL: a.cfg.BaseURL,
			Err:     fmt.Errorf("the model listing was not the documented shape: %w", err),
		}
	}

	served := make(map[string]struct{}, len(list.Data))
	windows := map[string]int{}
	for _, m := range list.Data {
		id := strings.TrimSpace(m.ID)
		if m.MaxModelLen > 0 && id != "" {
			windows[id] = m.MaxModelLen
		}
		if id == "" {
			// An entry with no id names no model. Skipping it is right;
			// admitting it would let the empty string match a request whose
			// model was never set.
			continue
		}
		served[id] = struct{}{}
	}
	a.dimMu.Lock()
	a.listedWindows = windows
	a.dimMu.Unlock()
	if len(served) == 0 {
		return nil, &ErrUndiscoverable{
			BaseURL: a.cfg.BaseURL,
			Err:     errors.New("the server listed no models; it is reachable but has nothing loaded"),
		}
	}
	return served, nil
}

// ReplayableOn answers the ReasoningReplayer question for this adapter.
//
// Always false, including for the very model that produced the reasoning. The
// OpenAI-compatible wire has no field that carries a thinking block back, so
// there is no such thing as replaying it here — and the wire adapter refuses a
// message containing one, which would end the turn on the step after the model
// first thought out loud.
func (a *Adapter) ReplayableOn(fromModel, toModel string) bool {
	return ReplayableOn(fromModel, toModel)
}
