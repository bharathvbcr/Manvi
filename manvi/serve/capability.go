package serve

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/llm/local"
)

// maxProbeTimeoutMS bounds capability.probe's deadline.
//
// A probe is one or two HTTP round trips against a server on loopback, so ten
// minutes is already far past generous. The ceiling is not about the network:
// a probe holds the local lane (see lanes.go), so a host's timeout_ms is the
// length of time one probe can delay every other local call on this sidecar. The
// unbounded field turned a single 1<<31 into a 596-hour stall that no host
// could distinguish from a crashed harness — and that outcome is reachable by
// a host typo, not only by a hostile caller.
const maxProbeTimeoutMS = 10 * 60 * 1000

// ProbeParams asks what a server can do with a model.
type ProbeParams struct {
	// BaseURL is the server's OpenAI-compatible API root. Empty means the
	// local adapter's default.
	BaseURL string `json:"base_url,omitempty"`
	// Model is the model to describe. Required: dimensions are per-model, and
	// a server's models do not share a context window.
	Model string `json:"model"`
	// DeclaredContextWindow is the host's own fallback, used only when the
	// server publishes nothing. Zero means the adapter's default.
	//
	// It is a *fallback*, never a ceiling: a host that sends 8192 and probes a
	// server reporting 262144 gets 262144. Any other rule would make this
	// field a way to silently under-use a model, which is the precise defect
	// this operation exists to remove.
	DeclaredContextWindow int `json:"declared_context_window,omitempty"`
	// MaxOutputTokens caps one response. Zero means the adapter's default.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// TrustDeclared skips interrogating the server and reports the declaration
	// back. For an operator who restricted the window on purpose.
	TrustDeclared bool `json:"trust_declared,omitempty"`
	// AssumeModelServed accepts the model without discovery, for a server that
	// does not implement a model listing at all.
	AssumeModelServed bool `json:"assume_model_served,omitempty"`
	// TimeoutMS bounds the probe. Zero means the adapter's default. A host
	// calls this on the path to assembling a request, so an unreachable server
	// must fail fast rather than hang the turn. Refused above
	// maxProbeTimeoutMS, and refused when negative.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// ProbeResult is what could be established, and where each answer came from.
//
// Source is the field that makes this operation worth having. A window that
// was read off the server and one that was typed into a setting produce the
// same request and completely different confidence, and a host that cannot
// tell them apart cannot tell an operator whether a mid-turn truncation was
// their configuration or their server.
type ProbeResult struct {
	Model         string `json:"model"`
	ContextWindow int    `json:"context_window"`
	// Source is "declared", "ollama:/api/show", "vllm:/v1/models", or
	// "llama.cpp:/props".
	Source string `json:"source"`
	// Discovered reports whether Source is a server rather than a setting.
	Discovered bool `json:"discovered"`
	// Describe is the provenance rendered for a human, so every host does not
	// re-derive the same sentence from the two fields above.
	Describe string `json:"describe"`
	// MaxOutputTokens is the effective per-response cap; zero means none was
	// stated.
	MaxOutputTokens int `json:"max_output_tokens"`

	// CapabilitiesKnown reports whether the three flags below mean anything.
	// False is not "no capabilities" — it is "the server published none", and
	// a host that treats the two the same will refuse a tool-capable model
	// served by a server that simply does not advertise.
	CapabilitiesKnown bool `json:"capabilities_known"`
	SupportsTools     bool `json:"supports_tools"`
	SupportsVision    bool `json:"supports_vision"`
	SupportsReasoning bool `json:"supports_reasoning"`
	// Embedding reports a model the server described as embedding-only. Such a
	// model answers a listing beside every chat model and fails at /api/chat,
	// so a host that offers it wastes an operator on a failure whose cause is
	// the model rather than their configuration.
	Embedding bool `json:"embedding"`

	// Served lists what the server does serve. Populated only when the
	// requested model was refused, so a host can name the alternatives instead
	// of reporting a dead end.
	Served []string `json:"served,omitempty"`
}

// probe answers OpCapabilityProbe.
func (s *Server) probe(ctx context.Context, raw json.RawMessage) (any, *Error) {
	var p ProbeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, badRequest("capability.probe params: %v", err)
	}
	if strings.TrimSpace(p.Model) == "" {
		return nil, badRequest("capability.probe requires a model")
	}
	if err := checkTokenCount("declared_context_window", p.DeclaredContextWindow); err != nil {
		return nil, err
	}
	if err := checkTokenCount("max_output_tokens", p.MaxOutputTokens); err != nil {
		return nil, err
	}
	if p.TimeoutMS < 0 {
		return nil, badRequest(
			"timeout_ms is negative (%d); a negative deadline is not a fast probe, "+
				"it is an unstated one", p.TimeoutMS)
	}
	if p.TimeoutMS > maxProbeTimeoutMS {
		return nil, badRequest(
			"timeout_ms is %d, past the %d ms ceiling; a probe holds the local lane, "+
				"so this value is how long one probe may delay every other local call, "+
				"and a sidecar that accepted 1<<31 would spend 596 hours indistinguishable "+
				"from one that hung", p.TimeoutMS, maxProbeTimeoutMS)
	}

	cfg := local.Config{
		BaseURL:              p.BaseURL,
		ContextWindow:        p.DeclaredContextWindow,
		MaxOutputTokens:      p.MaxOutputTokens,
		TrustDeclaredContext: p.TrustDeclared,
		AssumeModelServed:    p.AssumeModelServed,
		// SupportsTools starts true so a server that publishes no capability
		// list is not described as tool-less. Discovery narrows it when the
		// server does publish; see capabilityFor, which ANDs the two.
		SupportsTools: true,
	}
	if p.TimeoutMS > 0 {
		cfg.DiscoveryTimeout = time.Duration(p.TimeoutMS) * time.Millisecond
	}

	lease := s.probes.lease(cfg)
	result, perr, keep := probeWith(ctx, lease.adapter, cfg, p)
	if perr != nil && perr.Code == ErrNotServed && lease.hit {
		// The listing this refusal was read from is up to a TTL old, and the
		// one thing an operator does about "not served" is pull the model and
		// probe again. Answering that second probe from the listing taken
		// before the pull would refuse a model the server now serves, for as
		// long as the entry lives — so a refusal from a reused adapter is
		// asked again of a fresh one before it is believed.
		s.probes.settle(cfg, lease, false)
		lease = s.probes.fresh(cfg)
		result, perr, keep = probeWith(ctx, lease.adapter, cfg, p)
	}
	s.probes.settle(cfg, lease, keep)
	if perr != nil {
		return nil, perr
	}
	return result, nil
}

// probeWith runs one probe against adapter, and reports whether the adapter
// is worth keeping for the next probe of the same configuration.
//
// Only an adapter whose every answer came from the server is kept. The
// adapter caches failures and fallbacks for its TTL on purpose — on the
// harness path that stops a fan-out from re-dialling a dead server sixty-four
// times — but a host retrying a probe is asking for a fresh look, and the
// errors below promise one: unreachable is retryable because "start the
// server and probe again" is the fix. A kept adapter would answer that retry
// with the failure it cached a moment ago. The same holds for a window that
// fell back to the declaration while the server was being asked: a probe
// that timed out mid-load must not pin "declared" onto the next minute of
// answers.
func probeWith(ctx context.Context, adapter *local.Adapter, cfg local.Config, p ProbeParams) (ProbeResult, *Error, bool) {
	// Discovery first, so "the server does not serve this model" is reported
	// as itself rather than as a declared-window fallback that then fails at
	// request time.
	if !p.AssumeModelServed {
		served, err := adapter.Models(ctx)
		if err != nil {
			var undiscoverable *local.ErrUndiscoverable
			if errors.As(err, &undiscoverable) {
				return ProbeResult{}, &Error{
					Code:    ErrUnreachable,
					Message: undiscoverable.Error(),
					// The server may simply not be up yet — a host that pulls a
					// model or starts Ollama and retries will succeed.
					Retryable: true,
				}, false
			}
			return ProbeResult{}, &Error{Code: ErrUnreachable, Message: err.Error(), Retryable: true}, false
		}
		if !slices.Contains(served, p.Model) {
			return ProbeResult{}, &Error{
				Code: ErrNotServed,
				Message: (&local.ErrNotServed{
					BaseURL: cfg.BaseURL, Model: p.Model, Served: served,
				}).Error(),
			}, false
		}
	}

	dims := adapter.Dimensions(ctx, p.Model)
	cap, _ := adapter.Capability(p.Model)
	// A declared window is the server's answer only when the operator asked
	// for the declaration; otherwise it is what a probe that got nothing back
	// falls to, and see above for why that is not kept.
	fellBack := dims.Source == local.SourceDeclared && !p.TrustDeclared && !p.AssumeModelServed

	return ProbeResult{
		Model:             p.Model,
		ContextWindow:     dims.ContextWindow,
		Source:            string(dims.Source),
		Discovered:        dims.Discovered(),
		Describe:          dims.Describe(),
		MaxOutputTokens:   cap.MaxOutputTokens,
		CapabilitiesKnown: dims.CapabilitiesKnown,
		SupportsTools:     dims.SupportsTools,
		SupportsVision:    dims.SupportsVision,
		SupportsReasoning: dims.SupportsReasoning,
		Embedding:         dims.Embedding(),
	}, nil, !fellBack
}

// maxProbeAdapters bounds the adapters kept between probes. A host probes the
// handful of servers and settings its operator has configured; sixteen is far
// past that, and the bound is what keeps a host that probes a fresh base URL
// per call from growing this without limit.
const maxProbeAdapters = 16

// probeAdapters keeps the local adapter a probe built, so the next probe of
// the same configuration reuses what the adapter already discovered.
//
// Building one per probe threw away exactly the caching the adapter exists to
// do: every probe re-fetched the model listing and re-ran the dimension probe
// chain, one after another, on the local lane every other probe and scan
// waits behind. A host that probes on the path to each request it assembles
// paid those round trips on every request.
//
// The objection the per-probe adapter answered is a stale answer: a window
// discovered against a base URL the operator has since changed. Two things
// keep that out. The key is the whole local.Config, so a changed base URL —
// or any other changed setting, since every field changes what the adapter
// does — is a different entry rather than an old one. And an entry lives for
// the adapter's DiscoveryTTL, counted from before its first request, so a
// reused answer is never older than the freshness the harness already accepts
// for the same server on its own request path (see local.DefaultDiscoveryTTL).
// What that leaves is a change made behind an unchanged URL — a server
// restarted with a larger window — which a probe reports within the TTL
// rather than at once, exactly as the harness itself would.
//
// Failures and fallbacks are not kept at all; see probeWith. That includes a
// server that publishes no window, so it still pays the full chain per probe:
// its "declared" cannot be told apart from a probe that timed out.
type probeAdapters struct {
	mu      sync.Mutex
	entries map[string]probeLease
	// now is injected for tests, so expiry is asserted rather than slept
	// through.
	now func() time.Time
}

// probeLease is one adapter handed to one probe.
type probeLease struct {
	adapter *local.Adapter
	// expires is fixed when the adapter is built, before its first request,
	// so an entry can never outlive the discovery it would be reused for.
	expires time.Time
	// hit records that the adapter was reused rather than built.
	hit bool
}

// probeKey is the identity of a configuration. JSON rather than the struct
// itself because Config holds pointers and a slice, so two equal settings are
// not ==, and because encoding every exported field means a field added to
// Config later is part of the key without anyone remembering to add it.
func probeKey(cfg local.Config) (string, bool) {
	key, err := json.Marshal(cfg)
	if err != nil {
		// Only a non-finite float gets here. Such a config is never kept,
		// which costs it the old per-probe behaviour and nothing else.
		return "", false
	}
	return string(key), true
}

func (c *probeAdapters) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// lease returns the live adapter kept for cfg, or a fresh one.
func (c *probeAdapters) lease(cfg local.Config) probeLease {
	if key, ok := probeKey(cfg); ok {
		c.mu.Lock()
		entry, found := c.entries[key]
		if found && c.clock().Before(entry.expires) {
			c.mu.Unlock()
			entry.hit = true
			return entry
		}
		if found {
			delete(c.entries, key)
		}
		c.mu.Unlock()
	}
	return c.fresh(cfg)
}

// fresh builds an adapter without consulting what is kept.
func (c *probeAdapters) fresh(cfg local.Config) probeLease {
	ttl := cfg.DiscoveryTTL
	if ttl <= 0 {
		ttl = local.DefaultDiscoveryTTL
	}
	return probeLease{adapter: local.New(cfg, noCredential), expires: c.clock().Add(ttl)}
}

// settle keeps lease for the next probe of cfg, or drops it.
func (c *probeAdapters) settle(cfg local.Config, lease probeLease, keep bool) {
	key, ok := probeKey(cfg)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !keep {
		// Only this adapter: anything else under the key is a newer answer.
		if entry, found := c.entries[key]; found && entry.adapter == lease.adapter {
			delete(c.entries, key)
		}
		return
	}
	if lease.hit {
		return
	}
	if c.entries == nil {
		c.entries = map[string]probeLease{}
	}
	now := c.clock()
	if _, replacing := c.entries[key]; !replacing && len(c.entries) >= maxProbeAdapters {
		// Expired entries go first because they are already worthless; only
		// if that is not enough does the one closest to expiry go.
		for k, entry := range c.entries {
			if !now.Before(entry.expires) {
				delete(c.entries, k)
			}
		}
		for len(c.entries) >= maxProbeAdapters {
			oldestKey, oldest := "", time.Time{}
			for k, entry := range c.entries {
				if oldestKey == "" || entry.expires.Before(oldest) {
					oldestKey, oldest = k, entry.expires
				}
			}
			delete(c.entries, oldestKey)
		}
	}
	lease.hit = false
	c.entries[key] = lease
}

// noCredential is the resolver for a server that needs none.
//
// A local server on loopback is the case this whole adapter exists for, and it
// authenticates nothing. Returning an absent secret rather than an error keeps
// the adapter's own "is a key present" branch working without inventing one.
func noCredential() (credentials.Secret, error) {
	return credentials.NewSecret("", "local: no credential required"), nil
}
