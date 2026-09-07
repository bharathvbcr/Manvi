package serve

import (
	"context"
	"encoding/json"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm/local"
)

// local.scan lists the model servers running on this machine.
//
// The discovery already exists — `local.Scan` probes the well-known endpoints
// concurrently and identifies each runtime *by asking it*, never by assuming
// whichever runtime conventionally holds the port that answered. None of it was
// reachable from a host, because `capability.probe` requires already knowing
// the base URL and model, which is the answer rather than the question.

// ScanParams is one discovery sweep.
type ScanParams struct {
	// TimeoutMS bounds each endpoint's probe. Zero means the scanner's own
	// default.
	//
	// Bounded above for the reason every timeout on this plane is: dispatch is
	// serial, so this is how long one scan may hold every other call behind it.
	TimeoutMS int `json:"timeout_ms,omitempty"`
	// Capabilities asks each server about each model it serves. It costs one
	// request per model on Ollama, so a host that only needs to know which
	// servers answered leaves it off.
	Capabilities bool `json:"capabilities,omitempty"`
	// Endpoints overrides the well-known list, for an operator whose server is
	// somewhere this harness ships no guess for.
	Endpoints []string `json:"endpoints,omitempty"`
}

// ScanModel is one model a server reports.
type ScanModel struct {
	ID string `json:"id"`
	// ContextWindow is zero when the server did not report one.
	//
	// Zero means *unreported*, not "no context". `ContextWindowSource` says
	// which, because a model listed with a window of 0 reads as broken and one
	// listed without a window reads as unknown, and only the second is true.
	ContextWindow int `json:"context_window,omitempty"`
	// ContextWindowSource names where the window came from, including the case
	// where one was read off the server and refused as implausible.
	ContextWindowSource string `json:"context_window_source,omitempty"`
	// ImplausibleWindow is a window the scanner read and rejected. Non-zero
	// only when it was: a refusal that is reportable rather than silent.
	ImplausibleWindow int `json:"implausible_window,omitempty"`

	// CapabilitiesKnown reports whether the three flags below mean anything.
	//
	// This is the field that keeps them honest. Without it, "does not support
	// tools" and "nobody asked" are the same `false`, and a host would render a
	// perfectly capable model as incapable — or, worse, run a tool-calling turn
	// against one that cannot and blame the configuration.
	CapabilitiesKnown bool `json:"capabilities_known"`
	SupportsTools     bool `json:"supports_tools,omitempty"`
	SupportsReasoning bool `json:"supports_reasoning,omitempty"`
	SupportsVision    bool `json:"supports_vision,omitempty"`
	// SupportsCompletion reports that the model generates text at all. Tracked
	// apart from the rest because a local cache holds models that do not: an
	// embedding model answers /v1/models beside every chat model, and offering
	// it as something to run a turn on wastes an operator's time on a failure
	// whose cause is the model, not their configuration.
	SupportsCompletion bool `json:"supports_completion,omitempty"`
}

// ScanServer is one server that answered.
type ScanServer struct {
	BaseURL string `json:"base_url"`
	// Runtime as the server identified itself. `openai-compatible` means it
	// answered /v1/models and nothing else this harness knows how to ask —
	// which is a working server, and saying so is more honest than naming a
	// runtime from the port.
	Runtime string `json:"runtime"`
	// Version only Ollama reports. Empty elsewhere, and never load-bearing.
	Version string      `json:"version,omitempty"`
	Models  []ScanModel `json:"models"`
}

// ScanResult is the sweep.
type ScanResult struct {
	Servers []ScanServer `json:"servers"`
	// Scanned is how many endpoints were probed, so a host can tell "nothing is
	// running" from "we only looked in one place".
	Scanned int `json:"scanned"`
	// Capabilities records whether per-model questions were asked. Without it a
	// model list with no context windows is indistinguishable from a scan that
	// never asked for them.
	Capabilities bool `json:"capabilities"`
}

// maxScanTimeoutMS bounds one endpoint probe. Serial dispatch means this is
// also how long every other request waits.
const maxScanTimeoutMS = 30_000

func (s *Server) localScan(ctx context.Context, raw json.RawMessage) (any, *Error) {
	var p ScanParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, badRequest("local.scan params: %v", err)
		}
	}
	if p.TimeoutMS < 0 {
		return nil, badRequest(
			"timeout_ms is negative (%d); a negative deadline is not a fast scan, "+
				"it is an unstated one", p.TimeoutMS)
	}
	if p.TimeoutMS > maxScanTimeoutMS {
		return nil, badRequest(
			"timeout_ms is %d, past the %d ms ceiling; dispatch is serial by design, "+
				"so this value is how long one scan may hold every other call behind it",
			p.TimeoutMS, maxScanTimeoutMS)
	}

	opts := local.ScanOptions{
		Capabilities: p.Capabilities,
	}
	if p.TimeoutMS > 0 {
		opts.Timeout = time.Duration(p.TimeoutMS) * time.Millisecond
	}
	for _, raw := range p.Endpoints {
		opts.Endpoints = append(opts.Endpoints, local.Endpoint{BaseURL: raw})
	}

	scanned := len(opts.Endpoints)
	if scanned == 0 {
		scanned = len(local.WellKnownEndpoints())
	}

	found := local.Scan(ctx, opts)
	out := ScanResult{
		Servers:      make([]ScanServer, 0, len(found)),
		Scanned:      scanned,
		Capabilities: p.Capabilities,
	}
	for _, server := range found {
		models := make([]ScanModel, 0, len(server.Models))
		for _, m := range server.Models {
			models = append(models, ScanModel{
				ID:                  m.ID,
				ContextWindow:       m.ContextWindow,
				ContextWindowSource: string(m.Source),
				ImplausibleWindow:   m.ImplausibleWindow,
				CapabilitiesKnown:   m.CapabilitiesKnown,
				SupportsTools:       m.SupportsTools,
				SupportsReasoning:   m.SupportsReasoning,
				SupportsVision:      m.SupportsVision,
				SupportsCompletion:  m.SupportsCompletion,
			})
		}
		out.Servers = append(out.Servers, ScanServer{
			BaseURL: server.BaseURL,
			Runtime: string(server.Runtime),
			Version: server.Version,
			Models:  models,
		})
	}
	return out, nil
}
