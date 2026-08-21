package serve

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"manvi/credentials"
	"manvi/llm/local"
)

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
	// must fail fast rather than hang the turn.
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

	// The local adapter is built per probe rather than cached. A probe is one
	// or two HTTP calls against a loopback server, and caching adapters keyed
	// by config is how a host ends up reading a window discovered against a
	// base URL the operator has since changed.
	adapter := local.New(cfg, noCredential)

	// Discovery first, so "the server does not serve this model" is reported
	// as itself rather than as a declared-window fallback that then fails at
	// request time.
	if !p.AssumeModelServed {
		served, err := adapter.Models(ctx)
		if err != nil {
			var undiscoverable *local.ErrUndiscoverable
			if errors.As(err, &undiscoverable) {
				return nil, &Error{
					Code:    ErrUnreachable,
					Message: undiscoverable.Error(),
					// The server may simply not be up yet — a host that pulls a
					// model or starts Ollama and retries will succeed.
					Retryable: true,
				}
			}
			return nil, &Error{Code: ErrUnreachable, Message: err.Error(), Retryable: true}
		}
		if !slices.Contains(served, p.Model) {
			return nil, &Error{
				Code: ErrNotServed,
				Message: (&local.ErrNotServed{
					BaseURL: cfg.BaseURL, Model: p.Model, Served: served,
				}).Error(),
			}
		}
	}

	dims := adapter.Dimensions(ctx, p.Model)
	cap, _ := adapter.Capability(p.Model)

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
	}, nil
}

// noCredential is the resolver for a server that needs none.
//
// A local server on loopback is the case this whole adapter exists for, and it
// authenticates nothing. Returning an absent secret rather than an error keeps
// the adapter's own "is a key present" branch working without inventing one.
func noCredential() (credentials.Secret, error) {
	return credentials.NewSecret("", "local: no credential required"), nil
}
