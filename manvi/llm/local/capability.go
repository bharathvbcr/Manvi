// Package local drives an OpenAI-compatible server running on the operator's
// own machine — Ollama, MLX, vLLM, llama.cpp and anything else that speaks the
// same chat-completions wire.
//
// The hard problem this package solves is not the wire, which is shared with
// llm/openaicompat. It is capability. Every other adapter here ships a
// transcribed catalogue, because a hosted provider publishes a fixed model list
// with documented context windows. A local server has neither: the model set is
// whatever the operator has pulled, and it changes without notice.
//
// llm.Provider is explicit that an adapter "must not guess: an unknown model is
// a false return, not a permissive default". So this adapter does not invent a
// catalogue, and it does not accept anything either. It splits the question in
// two:
//
//   - Which models exist is *discovered*, by asking the server's /v1/models
//     endpoint. That is the server's own answer, not a guess, and a model it
//     does not list is refused.
//   - How large a context those models have is *declared* by the operator, via
//     the llm.local.* settings, because the OpenAI model listing carries no
//     such field and there is nowhere honest to read it from. The settings are
//     documented as declarations about the operator's own server, so a wrong
//     one is a configuration error with a name rather than a silent default.
//
// When discovery cannot run, the adapter fails closed and says so in those
// terms. A server that is down and a model that is absent are different
// diagnoses, and reporting the first as the second sends an operator to edit
// config when they should be starting a process.
package local

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"manvi/llm"
)

// Name is the adapter's stable identifier, and therefore the provenance value
// that decides whether adapter state is portable.
const Name = "local"

// DefaultBaseURL is where local servers conventionally listen. It matches the
// llm.local.base_url flag default.
const DefaultBaseURL = "http://127.0.0.1:8000/v1"

// Defaults for the operator-declared dimensions.
//
// These are deliberately modest rather than optimistic. Declaring a context
// window larger than the server's produces a request the server truncates or
// refuses deep into a turn; declaring a smaller one costs only that the harness
// compacts earlier than it strictly had to. Between a silent wrong answer and
// an early correct one, this picks the early correct one.
const (
	DefaultContextWindow   = 32768
	DefaultMaxOutputTokens = 16384
	// DefaultDiscoveryTTL bounds how stale the model list may be. A local
	// server's model set changes when an operator pulls a model, which is a
	// human-scale event, so a short TTL costs one cheap request per minute and
	// removes the class of failure where a freshly pulled model stays invisible
	// until the harness restarts.
	DefaultDiscoveryTTL = 60 * time.Second
	// DefaultDiscoveryTimeout bounds the discovery request itself. Capability
	// is called on the synchronous path to assembling a request, so an
	// unreachable server must fail fast rather than hang a turn.
	DefaultDiscoveryTimeout = 5 * time.Second
	// maxModelListBytes bounds the model listing read. A server that answers
	// this endpoint with an unbounded stream must not be able to exhaust the
	// harness's memory; 4 MiB is far beyond any real listing.
	maxModelListBytes = 4 << 20
)

// ReasoningEfforts is the set accepted when an operator declares that their
// server understands reasoning_effort. It mirrors the OpenAI-compatible
// vocabulary rather than inventing one.
var ReasoningEfforts = []string{"none", "low", "medium", "high"}

// Config is what an operator declares about their own server.
//
// Every field here is a statement the harness cannot verify by asking, which is
// exactly why each is an explicit setting rather than a default buried in code.
type Config struct {
	// BaseURL is the server's API root. Empty means DefaultBaseURL.
	BaseURL string
	// ContextWindow is the model's total token budget. Zero means
	// DefaultContextWindow.
	ContextWindow int
	// MaxOutputTokens caps one response. Zero means DefaultMaxOutputTokens.
	MaxOutputTokens int
	// SupportsTools declares whether the server implements tool calling. It
	// defaults to true at the flag layer because a coding harness against a
	// server without tools cannot do anything at all, so the useful default is
	// the one that works and fails loudly if wrong.
	SupportsTools bool
	// SupportsReasoning declares whether the server accepts reasoning_effort.
	// Off by default: servers that do not understand the field differ in
	// whether they ignore it or reject the whole request, and the second is a
	// turn lost to a parameter nobody asked for.
	SupportsReasoning bool
	// TrustDeclaredContext skips asking the server for its context window and
	// capabilities, using the declared values alone.
	//
	// Off by default, because the servers that publish these answer correctly
	// and the declared default is small enough to waste most of a large model's
	// window. It exists for the operator who has deliberately declared
	// something other than what the server reports — serving a 262k model but
	// holding history to 32k to bound memory, for instance — where discovery
	// would silently overrule a decision that was made on purpose.
	TrustDeclaredContext bool
	// AssumeModelServed accepts the configured model without discovery.
	//
	// This is the escape hatch for a server that does not implement /v1/models
	// at all. It is off by default and named for what it is: with it on, the
	// harness is taking the operator's word instead of the server's, and a
	// wrong model id becomes a failure at request time rather than at assembly.
	AssumeModelServed bool
	// Temperature sets default sampling temperature for the local model.
	Temperature *float64
	// TopP sets default nucleus sampling threshold for the local model.
	TopP *float64
	// MinP sets default min_p sampling threshold for the local model.
	MinP *float64
	// RepetitionPenalty sets default repetition penalty for the local model.
	RepetitionPenalty *float64
	// TopK caps the candidate set. Qwen3 ships top_k 20 beside its weights.
	TopK *int
	// PresencePenalty and FrequencyPenalty are the OpenAI-vocabulary penalties.
	PresencePenalty  *float64
	FrequencyPenalty *float64
	// Seed makes a run reproducible on a server that honours it.
	Seed *int
	// Stop sets default stop sequences for the local model.
	Stop []string
	// StallTimeout bounds the gap between streamed bytes. Zero means the
	// package default; negative disables the watchdog.
	StallTimeout time.Duration
	// AssumeReasoningPrefill declares that the chat template opens a thinking
	// tag in the prompt, so generation starts inside it.
	AssumeReasoningPrefill bool
	// DiscoveryTTL and DiscoveryTimeout override the defaults above.
	DiscoveryTTL     time.Duration
	DiscoveryTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.ContextWindow <= 0 {
		c.ContextWindow = DefaultContextWindow
	}
	if c.MaxOutputTokens <= 0 {
		c.MaxOutputTokens = DefaultMaxOutputTokens
	}
	if c.DiscoveryTTL <= 0 {
		c.DiscoveryTTL = DefaultDiscoveryTTL
	}
	if c.DiscoveryTimeout <= 0 {
		c.DiscoveryTimeout = DefaultDiscoveryTimeout
	}
	// Config is taken by value, but a slice field is a window onto the caller's
	// array. Without this copy, a config value the caller still holds — or
	// reuses to build a second adapter — can change what a later turn sends,
	// from outside the adapter and with nothing in the adapter's own state
	// having changed.
	if len(c.Stop) > 0 {
		c.Stop = append([]string(nil), c.Stop...)
	}
	return c
}

// baseCapability builds the baseline descriptor for a model from configuration.
// It is only ever called for such a model, so it never invents one.
func (c Config) baseCapability(model string) llm.Capability {
	cap := llm.Capability{
		Provider:      Name,
		Model:         model,
		ContextWindow: c.ContextWindow,
		// A local server streams by definition of this adapter: the wire it
		// speaks is the streaming one, and a server that refused to stream
		// would fail at the first request rather than be mis-described here.
		SupportsStreaming: true,
		SupportsTools:     c.SupportsTools,
		SupportsReasoning: c.SupportsReasoning,
	}
	cap.MaxOutputTokens = outputCapFor(c.MaxOutputTokens, c.ContextWindow)
	if c.SupportsReasoning {
		cap.EffortLevels = append([]string(nil), ReasoningEfforts...)
	}
	return cap
}

// outputCapFor resolves the cap on one response against the window that
// response has to fit inside.
//
// The tempting shortcut is to drop a cap that exceeds the window, on the
// grounds that such a cap refuses nothing the server would accept. It is wrong
// in the direction that costs a turn: llm.Capability.Validate reads a zero as
// "no stated cap", so an 8192-token server paired with the shipped
// 16384-token output default ends up validating a 16384-token request against
// an 8192-token window, and the truncation lands mid-generation on the server
// instead of at assembly with a message naming the number. So the cap is
// clamped to what the window can hold, never dropped.
//
// The clamp is half the window rather than all of it, because output shares the
// window with the prompt that produced it: a response permitted to fill the
// window leaves nowhere to ask from. Half is the ratio this harness already
// works to — the shipped defaults are a 32768-token window against a
// 16384-token cap, and agent.Loop's budget holds its output reservation to
// window/2 for the same reason — so an operator who changed neither setting
// sees no change from this.
func outputCapFor(declared, window int) int {
	if window <= 0 {
		return declared
	}
	room := window / 2
	if room <= 0 {
		// A one-token window cannot be halved, and returning zero here would
		// mean "unbounded", which is the one answer this function exists to
		// avoid.
		room = window
	}
	if declared <= 0 || declared > room {
		return room
	}
	return declared
}

// ErrUndiscoverable reports that the model list could not be read, which is a
// different condition from a model being absent and must not be collapsed into
// it.
type ErrUndiscoverable struct {
	BaseURL string
	Err     error
}

func (e *ErrUndiscoverable) Error() string {
	return fmt.Sprintf("local: cannot reach %s to discover which models it serves: %v — "+
		"this is not a negative result, the check did not run. Start the server, correct "+
		"llm.local.base_url, or set llm.local.assume_model_served to take the configured "+
		"model on trust", e.BaseURL, e.Err)
}

func (e *ErrUndiscoverable) Unwrap() error { return e.Err }

// ErrNotServed reports that the server answered and does not serve the model.
type ErrNotServed struct {
	BaseURL string
	Model   string
	Served  []string
}

func (e *ErrNotServed) Error() string {
	served := "nothing at all"
	if len(e.Served) > 0 {
		shown := e.Served
		// A local cache can hold hundreds of models; a refusal that prints all
		// of them buries its own first line.
		const limit = 12
		if len(shown) > limit {
			served = fmt.Sprintf("%s and %d more", strings.Join(shown[:limit], ", "), len(shown)-limit)
		} else {
			served = strings.Join(shown, ", ")
		}
	}
	return fmt.Sprintf("local: the server at %s does not serve model %q (it serves: %s)",
		e.BaseURL, e.Model, served)
}

// ReplayableOn reports whether adapter state produced by one model can be sent
// to another.
//
// Local servers publish no mechanism for replaying model-internal reasoning
// state across a turn boundary, and the set of engines behind this adapter is
// open-ended, so the adapter claims none. Returning false unconditionally is
// the fail-closed answer: the neutral layer strips reasoning blocks and reports
// the drop, which is a visible, explainable loss rather than a silent one.
func ReplayableOn(fromModel, toModel string) bool { return false }

// sortedCopy returns a stable, de-duplicated view of a discovered model set, so
// error messages and listings do not reorder between calls.
func sortedCopy(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
