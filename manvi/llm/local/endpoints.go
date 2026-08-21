package local

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"manvi/credentials"
	"manvi/llm/transport"
)

// Finding a local server is a discovery problem, not a configuration one.
//
// The rest of this package takes a base URL as given and asks the server behind
// it what it serves. That is the right shape once an operator has told the
// harness where their server is, and the wrong shape for the moment before they
// have: llm.local.base_url ships pointing at vLLM's port, so an operator whose
// Ollama is running — the common case, and the one the README names first —
// meets "cannot reach http://127.0.0.1:8000/v1" while a server sits answering
// on 11434. Nothing in that message says a port was guessed, so the operator
// reads a broken harness rather than an unset setting.
//
// The candidates below are the documented default listen addresses of the
// runtimes this adapter is built for. Probing them is cheap in exactly the case
// that matters: every one is loopback, so a port with nothing behind it refuses
// the connection immediately rather than timing out.
//
// What this does not do is guess *which* server an operator meant when several
// answer. Picking one would be the "permissive default" llm.Provider forbids,
// one layer up. Unambiguous is resolved; ambiguous is reported with the setting
// that settles it.

// Runtime names the server implementation behind an endpoint.
//
// It is established by asking, never by which port answered. Ports are a
// convention an operator overrides freely — llama.cpp and mlx_lm.server share
// 8080 by default — so a runtime read off a port number would be a label the
// harness invented, printed with the same confidence as one it was told.
type Runtime string

const (
	// RuntimeOllama is identified by /api/version, which no other runtime here
	// serves.
	RuntimeOllama Runtime = "ollama"
	// RuntimeLlamaCPP is identified by /props, the same endpoint the context
	// window is read from.
	RuntimeLlamaCPP Runtime = "llama.cpp"
	// RuntimeVLLM is identified by max_model_len on the model listing, which is
	// vLLM's own addition to the OpenAI model card.
	RuntimeVLLM Runtime = "vllm"
	// RuntimeLMStudio is identified by its native /api/v1/models surface.
	RuntimeLMStudio Runtime = "lm-studio"
	// RuntimeUnidentified is a server that answered /v1/models and nothing
	// else this harness knows how to ask. It is a working server, and saying
	// so is more honest than naming whichever runtime conventionally holds the
	// port it answered on.
	RuntimeUnidentified Runtime = "openai-compatible"
)

// Endpoint is one address worth trying, and why.
type Endpoint struct {
	// BaseURL is the OpenAI-compatible API root.
	BaseURL string
	// Convention names the runtimes documented to listen here. It is a note
	// for an operator reading a scan that found nothing, not an identification
	// — what actually answers is established by probing.
	Convention string
}

// WellKnownEndpoints lists the loopback addresses local runtimes listen on by
// default, in the order they are tried.
//
// Ollama leads because it is the one an operator is most likely to already have
// running, and because it is the case the shipped default gets wrong. The
// shipped default follows it so that a machine running both still resolves to
// the configured address rather than to whatever answered first.
func WellKnownEndpoints() []Endpoint {
	return []Endpoint{
		{BaseURL: "http://127.0.0.1:11434/v1", Convention: "Ollama"},
		{BaseURL: DefaultBaseURL, Convention: "vLLM"},
		{BaseURL: "http://127.0.0.1:1234/v1", Convention: "LM Studio"},
		{BaseURL: "http://127.0.0.1:8080/v1", Convention: "llama.cpp, mlx_lm.server, llamafile"},
		{BaseURL: "http://127.0.0.1:1337/v1", Convention: "Jan"},
	}
}

// ServedModel is one model a scan found, with whatever the server said about it.
type ServedModel struct {
	ID string
	Dimensions
}

// Usable reports whether this model could drive a coding turn.
//
// A server that published no capabilities yields true, because unknown is not a
// negative result: the MLX server says nothing about any of its models, and
// treating silence as "cannot" would hide every model it serves. What this does
// exclude is a model the server positively described as unable — an embedding
// model, or a chat model served without tool support — since offering those
// sends an operator to debug a configuration that was never the problem.
func (m ServedModel) Usable() bool {
	if !m.CapabilitiesKnown {
		return true
	}
	return m.SupportsCompletion && m.SupportsTools
}

// Why explains a model a scan will not recommend, and is empty for one it will.
func (m ServedModel) Why() string {
	switch {
	case !m.CapabilitiesKnown || m.Usable():
		return ""
	case !m.SupportsCompletion:
		return "embedding model — it does not generate text"
	default:
		return "no tool support — a coding turn cannot call a tool"
	}
}

// Server is one local server a scan reached.
type Server struct {
	BaseURL string
	Runtime Runtime
	// Version is what the server reported, when it reports one. Only Ollama
	// does, so this is usually empty and never load-bearing.
	Version string
	Models  []ServedModel
}

// Usable returns the models on this server that could drive a coding turn.
func (s Server) Usable() []ServedModel {
	out := make([]ServedModel, 0, len(s.Models))
	for _, m := range s.Models {
		if m.Usable() {
			out = append(out, m)
		}
	}
	return out
}

// Serves reports whether this server lists a model by that exact id.
func (s Server) Serves(model string) bool {
	for _, m := range s.Models {
		if m.ID == model {
			return true
		}
	}
	return false
}

// Describe renders the server's identity for an operator.
func (s Server) Describe() string {
	name := string(s.Runtime)
	if s.Version != "" {
		name += " " + s.Version
	}
	return fmt.Sprintf("%s (%s)", s.BaseURL, name)
}

// ScanOptions bounds a scan.
type ScanOptions struct {
	// Endpoints overrides the well-known list. Used by tests, and by an
	// operator scanning an address this harness does not ship a guess for.
	Endpoints []Endpoint
	// Timeout bounds each endpoint's probe. Zero means DefaultScanTimeout.
	Timeout time.Duration
	// Credential is sent to each candidate, for the operator who fronts their
	// server with a proxy that checks one. Nil sends none.
	Credential func() (credentials.Secret, error)
	// Capabilities asks each server about each model it serves. It costs one
	// request per model on Ollama, so it is off for the resolution path — which
	// only needs to know that a server answered — and on for the listing an
	// operator reads.
	Capabilities bool
}

// DefaultScanTimeout bounds one endpoint probe.
//
// Short, because every candidate is loopback: a port with nothing behind it
// refuses instantly, and a local server answers its own model listing in
// milliseconds. This is the ceiling for a server that accepted the connection
// and then went quiet, which must not be allowed to hold up a scan of four
// other addresses.
const DefaultScanTimeout = 2 * time.Second

// Scan reports every local server that answered, in the order the endpoints
// were tried.
//
// An endpoint that does not answer is absent from the result rather than
// present with an error, because a refused connection on a port the operator
// never mentioned is the expected answer from four candidates out of five. The
// failure that does need reporting — no endpoint answered at all — is visible
// as an empty result, and reported by the callers that can say what it means.
func Scan(ctx context.Context, opts ScanOptions) []Server {
	endpoints := opts.Endpoints
	if len(endpoints) == 0 {
		endpoints = WellKnownEndpoints()
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultScanTimeout
	}

	found := make([]*Server, len(endpoints))
	var wg sync.WaitGroup
	for i, ep := range endpoints {
		wg.Add(1)
		go func(i int, ep Endpoint) {
			defer wg.Done()
			found[i] = probeEndpoint(ctx, ep.BaseURL, timeout, opts)
		}(i, ep)
	}
	wg.Wait()

	out := make([]Server, 0, len(endpoints))
	for _, s := range found {
		if s != nil {
			out = append(out, *s)
		}
	}
	return out
}

// probeEndpoint asks one address what it serves, returning nil when it is not a
// local model server.
func probeEndpoint(ctx context.Context, baseURL string, timeout time.Duration, opts ScanOptions) *Server {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// A throwaway adapter rather than a second HTTP client: the model listing,
	// its bounded read, the credential header and every dimension probe are
	// already implemented here, and a scanner with its own copy of them would
	// drift from the one the turn actually uses.
	a := New(Config{
		BaseURL:          baseURL,
		SupportsTools:    true,
		DiscoveryTimeout: timeout,
	}, credentialOrNone(opts.Credential))

	// One attempt, no backoff. The transport retries a refused connection four
	// times over roughly fifteen seconds, which is right for the server an
	// operator configured and wrong for four speculative ports they never
	// mentioned: on loopback a refusal is immediate and definite, and retrying
	// it turns a scan that should cost milliseconds into one that burns its
	// whole timeout on addresses with nothing behind them. This is the same
	// argument Client.Probe makes for the server-native endpoints.
	a.Client().Retry = transport.RetryPolicy{MaxAttempts: 1}

	srv, err := a.Survey(ctx, opts.Capabilities)
	if err != nil {
		return nil
	}
	return &srv
}

// Survey reports what this adapter's own server is serving, with the runtime
// identified and — when asked — every model's dimensions filled in.
//
// It is the same answer Scan builds for a discovered endpoint, produced by the
// same code, so `manvi local` describes an operator's configured server and a
// found one identically instead of having two notions of what a server is.
//
// withCapabilities costs one request per model on Ollama, which is worth it for
// a listing an operator reads and not for a reachability check.
func (a *Adapter) Survey(ctx context.Context, withCapabilities bool) (Server, error) {
	ids, err := a.Models(ctx)
	if err != nil {
		return Server{}, err
	}

	srv := Server{BaseURL: a.cfg.BaseURL, Models: make([]ServedModel, len(ids))}
	for i, id := range ids {
		srv.Models[i] = ServedModel{ID: id}
	}
	srv.Runtime, srv.Version = a.identify(ctx)

	if withCapabilities {
		var wg sync.WaitGroup
		for i := range srv.Models {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				srv.Models[i].Dimensions = a.Dimensions(ctx, srv.Models[i].ID)
			}(i)
		}
		wg.Wait()
	}
	return srv, nil
}

// credentialOrNone adapts an absent resolver to the shape New expects.
func credentialOrNone(resolve func() (credentials.Secret, error)) func() (credentials.Secret, error) {
	if resolve != nil {
		return resolve
	}
	return func() (credentials.Secret, error) { return credentials.Secret{}, nil }
}

// ollamaVersion is the useful part of Ollama's /api/version.
type ollamaVersion struct {
	Version string `json:"version"`
}

// identify establishes which runtime answered, by asking rather than by
// inferring from the port.
//
// Ordered cheapest-first: the vLLM marker was already read off the model
// listing this adapter fetched, so it costs nothing, and each remaining probe
// is one request that most runtimes answer with 404.
func (a *Adapter) identify(ctx context.Context) (Runtime, string) {
	a.dimMu.Lock()
	listedWindows := len(a.listedWindows)
	a.dimMu.Unlock()
	if listedWindows > 0 {
		return RuntimeVLLM, ""
	}

	base, ok := a.nativeRoot()
	if !ok {
		return RuntimeUnidentified, ""
	}

	if body, err := a.getBoundedAbsolute(ctx, base+"/api/version"); err == nil {
		var v ollamaVersion
		if json.Unmarshal(body, &v) == nil && v.Version != "" {
			return RuntimeOllama, v.Version
		}
	}
	if _, ok := a.probeLlamaCPP(ctx); ok {
		return RuntimeLlamaCPP, ""
	}
	// LM Studio's native surface is asked for existence only. Its documented
	// shape has changed across releases — /api/v0/models became /api/v1/models
	// in 0.4.0 — and identifying a server by a field this harness has never
	// seen from a live instance would be transcription passed off as
	// observation. That the endpoint answers at all is enough to name it.
	if _, err := a.getBoundedAbsolute(ctx, base+"/api/v1/models"); err == nil {
		return RuntimeLMStudio, ""
	}
	return RuntimeUnidentified, ""
}

// Resolution is the outcome of deciding which local server to talk to.
type Resolution struct {
	// BaseURL is the address to use. It is always set: when nothing is found,
	// it is the declared one, so the adapter's own diagnosis is what an
	// operator reads rather than a second message about the same failure.
	BaseURL string
	// Declared reports that the operator named this address, so no scan ran.
	Declared bool
	// Configured is the address that was in force before the scan. It is kept
	// so the report can name it: BaseURL has by then been moved to whatever
	// was found, and a message that named the shipped constant instead would
	// quietly become a lie the day that constant changes.
	Configured string
	// Found is the server behind BaseURL, when a scan established one.
	Found *Server
	// Ambiguous holds every server a scan reached when more than one did and
	// none was the configured address. BaseURL is left at the declared value
	// in that case: the harness will not pick between an operator's servers.
	Ambiguous []Server
	// MatchedModel is set when several servers answered and the operator's own
	// model setting is what singled one out.
	MatchedModel string
	// Scanned is the endpoints that were tried, for a report that can say the
	// check ran and found nothing.
	Scanned []Endpoint
}

// Note renders what the resolution did, for an operator who otherwise cannot
// tell an address that was configured from one that was found.
func (r Resolution) Note() string {
	switch {
	case r.Declared:
		return ""
	case len(r.Ambiguous) > 1:
		var names []string
		for _, s := range r.Ambiguous {
			names = append(names, s.Describe())
		}
		return fmt.Sprintf(
			"%d local servers are running (%s) and none is the configured %s — "+
				"set %s to the one you mean; until then requests go to the configured address",
			len(r.Ambiguous), strings.Join(names, ", "), r.Configured, BaseURLSetting)
	case r.MatchedModel != "":
		return fmt.Sprintf("using the %s server at %s — of the local servers running, it is the only one serving %q",
			r.Found.Runtime, r.Found.BaseURL, r.MatchedModel)
	case r.Found != nil && r.Found.BaseURL != r.Configured:
		return fmt.Sprintf("using the %s server found at %s (%s is unset and nothing is serving its default %s)",
			r.Found.Runtime, r.Found.BaseURL, BaseURLSetting, r.Configured)
	default:
		return ""
	}
}

// BaseURLSetting names the flag that pins an endpoint. It lives here so the
// messages this package writes can name it without importing the flag
// catalogue, which imports nothing from llm.
const BaseURLSetting = "llm.local.base_url"

// ResolveOptions is what a resolution is given to work with.
type ResolveOptions struct {
	// Declared is the configured address. Empty means DefaultBaseURL.
	Declared string
	// DeclaredByOperator reports that Declared came from a person rather than
	// from this harness's shipped guess. It is the only thing that decides
	// whether a scan runs at all.
	DeclaredByOperator bool
	// Model is the model id the operator named, if they named one. It is not
	// the model a run will use — that is not known until a server is picked —
	// only the one explicitly written down, which is why a discovered model
	// never appears here: it presupposes the server this is helping to choose.
	//
	// It settles the case where several servers answer. Naming a model is an
	// operator decision, and if exactly one running server has that model then
	// the operator has already chosen; the harness is reading their setting,
	// not picking for them.
	Model string
	// Endpoints overrides the well-known list. Production leaves it nil; tests
	// set it so the rules below are exercised rather than reimplemented beside
	// them.
	Endpoints []Endpoint
	// Timeout bounds each probe. Zero means DefaultScanTimeout.
	Timeout time.Duration
	// Credential is sent to each candidate. Nil sends none.
	Credential func() (credentials.Secret, error)
}

// ResolveEndpoint decides which local server a run will talk to.
//
// declaredByOperator is the whole hinge. An address someone typed is used as
// typed and never second-guessed — probing alternatives would mean the harness
// arguing with its operator about their own machine, which is the same line
// dimension discovery already refuses to cross. Only the shipped guess is
// treated as a guess.
//
// The resolution rules, in order:
//
//   - Nothing answered: keep the declared address, so the failure an operator
//     reads is the adapter's own, naming the address and what to do about it.
//   - The declared address answered: use it. A machine running both Ollama and
//     vLLM resolves to the configured one, not to whichever replied first.
//   - Exactly one other answered: use it. This is the case the shipped default
//     gets wrong, and it is unambiguous.
//   - Several answered and none was the declared address: pick nothing, and
//     report them. Choosing between an operator's servers on their behalf is a
//     guess, and this package does not make those.
func ResolveEndpoint(ctx context.Context, opts ResolveOptions) Resolution {
	declared := opts.Declared
	if declared == "" {
		declared = DefaultBaseURL
	}
	if opts.DeclaredByOperator {
		return Resolution{BaseURL: declared, Configured: declared, Declared: true}
	}

	endpoints := opts.Endpoints
	if len(endpoints) == 0 {
		endpoints = WellKnownEndpoints()
	}
	res := Resolution{BaseURL: declared, Configured: declared, Scanned: endpoints}
	servers := Scan(ctx, ScanOptions{
		Endpoints:  endpoints,
		Timeout:    opts.Timeout,
		Credential: opts.Credential,
	})
	switch {
	case len(servers) == 0:
		return res
	case len(servers) == 1:
		res.BaseURL = servers[0].BaseURL
		found := servers[0]
		res.Found = &found
		return res
	}
	// Several answered. The operator's own model setting is allowed to settle
	// it, and nothing else is.
	//
	// What used to settle it was whether one of them happened to sit on the
	// shipped default's port — which is the harness's guess, not anyone's
	// preference, and dressing a guess up as a configured choice is exactly the
	// permissive default this package refuses elsewhere. It also made the
	// ambiguity report unreachable on any machine with something on that port,
	// and picked a server publishing no capabilities over one publishing all of
	// them, for no better reason than the number 8000.
	if opts.Model != "" {
		var matched []Server
		for i := range servers {
			if servers[i].Serves(opts.Model) {
				matched = append(matched, servers[i])
			}
		}
		if len(matched) == 1 {
			res.BaseURL = matched[0].BaseURL
			res.Found = &matched[0]
			res.MatchedModel = opts.Model
			return res
		}
	}
	res.Ambiguous = servers
	return res
}

// SoleUsableModel names the model to run when a server leaves no choice.
//
// The second return is why there was a choice, phrased for an operator who has
// to make it. A harness that picked one of six models and said nothing would be
// choosing which weights an operator's work runs against, which is not a
// default anything should hold.
func SoleUsableModel(s Server) (string, string) {
	usable := s.Usable()
	switch len(usable) {
	case 0:
		if len(s.Models) == 0 {
			return "", fmt.Sprintf("%s serves no models", s.BaseURL)
		}
		var reasons []string
		for _, m := range s.Models {
			if why := m.Why(); why != "" {
				reasons = append(reasons, fmt.Sprintf("%s (%s)", m.ID, why))
			}
		}
		sort.Strings(reasons)
		return "", fmt.Sprintf("%s serves no model that can drive a coding turn: %s",
			s.BaseURL, strings.Join(reasons, ", "))
	case 1:
		return usable[0].ID, ""
	default:
		names := make([]string, 0, len(usable))
		for _, m := range usable {
			names = append(names, m.ID)
		}
		sort.Strings(names)
		const limit = 12
		suffix := ""
		if len(names) > limit {
			suffix = fmt.Sprintf(" and %d more", len(names)-limit)
			names = names[:limit]
		}
		return "", fmt.Sprintf("%s serves %d usable models: %s%s",
			s.BaseURL, len(usable), strings.Join(names, ", "), suffix)
	}
}
