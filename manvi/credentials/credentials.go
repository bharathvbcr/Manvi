// Package credentials resolves provider API keys and keeps them out of
// everything that records or displays text.
//
// The harness writes an append-only session log, prints run reports, and
// renders errors into a terminal. Every one of those is a place a bearer token
// can escape to, and a token that reaches a log file is compromised whether or
// not anyone reads it. So the secret is not a string here. It is a type whose
// only way out is Reveal, which exists at exactly one call site — the moment an
// Authorization header is built — and which is never handed a value that has
// been through fmt.
//
// Two rules the rest of the harness depends on:
//
//   - Secret implements Stringer, Formatter, and both marshalers, and all of
//     them redact. A struct containing one can be logged with %+v, marshalled
//     into the session log, or printed in a panic, and the value does not
//     appear. Types that redact only under %s are the ones that leak.
//
//   - Keys come from the environment and from nowhere else. Not from a config
//     file the gate would then have to protect, and explicitly not from .env —
//     the policy layer refuses to write those paths, and a resolver that read
//     them would be the one component reintroducing the risk the gate removes.
package credentials

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// Redacted is what a secret renders as everywhere.
const Redacted = "[redacted]"

// Secret holds a credential. The zero value is "not present".
type Secret struct {
	value string
	// source names where it came from, for diagnostics. Safe to print.
	source string
}

// NewSecret wraps a value read from source.
func NewSecret(value, source string) Secret {
	return Secret{value: value, source: source}
}

// Present reports whether a credential was found.
func (s Secret) Present() bool { return s.value != "" }

// Source names where the credential came from — an environment variable name,
// never its contents.
func (s Secret) Source() string { return s.source }

// Reveal returns the credential itself.
//
// Every call is a place the value can escape, so there should be very few, and
// each should hand the result straight to the thing that consumes it. Assigning
// it to a variable that later reaches a formatted string undoes the type.
func (s Secret) Reveal() string { return s.value }

// Len returns the credential's length, which is safe to report and is often
// enough to diagnose a truncated or padded key without printing it.
func (s Secret) Len() int { return len(s.value) }

// String redacts.
func (s Secret) String() string {
	if !s.Present() {
		return "[absent]"
	}
	return Redacted
}

// Format redacts under every verb, including %v, %+v, %#v, and %q. Without
// this, a struct holding a Secret printed with %#v would render the field's
// contents directly.
func (s Secret) Format(f fmt.State, verb rune) { fmt.Fprint(f, s.String()) }

// GoString redacts under %#v.
func (s Secret) GoString() string { return "credentials.Secret(" + s.String() + ")" }

// MarshalJSON redacts, so a struct carrying a secret can be written to the
// session log without the log becoming a credential store.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// MarshalText redacts for every encoder that prefers it over MarshalJSON.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalJSON refuses. A credential arriving from a document is a credential
// stored in that document, and this package's premise is that none are.
func (s *Secret) UnmarshalJSON([]byte) error {
	return errors.New("credentials: a Secret cannot be decoded from a document; keys come from the environment")
}

// Requirement describes one provider's credential.
type Requirement struct {
	// Provider is the adapter name, matching llm.Provider.Name.
	Provider string
	// EnvVars are the environment variables consulted, in order. The first one
	// set wins, and the winner's name is recorded as the source.
	EnvVars []string
	// Doc points at where an operator gets the key.
	Doc string
	// Optional marks a provider that works without a credential, which is the
	// normal case for a server on loopback that ignores authorisation. Resolve
	// then returns an absent Secret and no error, and the adapter sends no
	// Authorization header rather than an empty bearer.
	//
	// It is per-provider rather than a global relaxation because the failure it
	// would otherwise cause is specific: a harness that treats "no key" as a
	// hard error cannot talk to the one class of endpoint that never wanted
	// one. Every provider that does require a key keeps refusing without it.
	Optional bool
}

// ErrMissing reports that no credential was found for a provider.
type ErrMissing struct {
	Provider string
	EnvVars  []string
	Doc      string
}

func (e *ErrMissing) Error() string {
	msg := fmt.Sprintf("no credential for %s: set one of %s", e.Provider, strings.Join(e.EnvVars, ", "))
	if e.Doc != "" {
		msg += " (" + e.Doc + ")"
	}
	return msg
}

// Resolver answers credential lookups.
type Resolver struct {
	mu           sync.RWMutex
	requirements map[string]Requirement
	// lookup is injected for tests; nil means os.LookupEnv.
	lookup func(string) (string, bool)
	// overrides hold credentials supplied in-process, which is how a test or an
	// embedding program provides one without touching the environment.
	overrides map[string]Secret
}

// NewResolver returns a resolver with the harness's provider requirements.
func NewResolver() *Resolver {
	r := &Resolver{requirements: map[string]Requirement{}, overrides: map[string]Secret{}}
	for _, req := range DefaultRequirements() {
		r.requirements[req.Provider] = req
	}
	return r
}

// DefaultRequirements is the harness's credential catalogue.
//
// The variable names are each vendor's own documented convention, and several
// are listed per provider because the surrounding tooling is not consistent:
// an operator who already has a working CLI should not have to discover a
// second name for the same key.
func DefaultRequirements() []Requirement {
	return []Requirement{
		{
			Provider: "anthropic",
			EnvVars:  []string{"ANTHROPIC_API_KEY"},
			Doc:      "https://console.anthropic.com/settings/keys",
		},
		{
			Provider: "xai",
			EnvVars:  []string{"XAI_API_KEY", "GROK_API_KEY"},
			Doc:      "https://console.x.ai",
		},
		{
			Provider: "gemini",
			EnvVars:  []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
			// A Google Cloud console API key, which is where this harness's
			// keys are issued. AI Studio hands out keys for the same endpoint,
			// but sending an operator there produces a key on a different
			// project from the one whose quota and restrictions they manage.
			// Either console's key travels in the same x-goog-api-key header,
			// so the variable names below are unchanged by the choice.
			Doc: "https://console.cloud.google.com/apis/credentials (a Cloud console API key for the Generative Language API)",
		},
		{
			Provider: "local",
			EnvVars:  []string{"LOCAL_API_KEY", "OPENAI_API_KEY"},
			Doc: "any value the local server accepts; most ignore it, so none is required. " +
				"OPENAI_API_KEY is honoured as a convenience, but only for a base URL on this " +
				"machine — a key set for another vendor's service is not sent to an arbitrary host",
			// A server on loopback that never checks a key must not be
			// unreachable because the harness insisted on one.
			Optional: true,
			// OPENAI_API_KEY is borrowed from a different vendor's tooling, and
			// llm.local.base_url does not have to name this machine. The
			// destination check that keeps those two facts from combining into
			// a credential leak lives in llm/local
			// (checkCredentialDestination), because this package knows which
			// variable answered and only the adapter knows where the request is
			// going. A variable added here that names someone else's service
			// must be added to that check too.
		},
	}
}

// Require registers or replaces a requirement.
func (r *Resolver) Require(req Requirement) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requirements[req.Provider] = req
}

// Set supplies a credential in-process, bypassing the environment.
func (r *Resolver) Set(provider string, secret Secret) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overrides[provider] = secret
}

// Resolve returns the credential for a provider, or *ErrMissing naming every
// variable that would have satisfied it. The error is deliberately specific:
// "authentication failed" sends an operator to the wrong place.
func (r *Resolver) Resolve(provider string) (Secret, error) {
	r.mu.RLock()
	req, known := r.requirements[provider]
	override, hasOverride := r.overrides[provider]
	lookup := r.lookup
	r.mu.RUnlock()

	if hasOverride && override.Present() {
		return override, nil
	}
	if !known {
		return Secret{}, fmt.Errorf("credentials: no requirement registered for provider %q", provider)
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	for _, name := range req.EnvVars {
		if value, ok := lookup(name); ok {
			// A variable that is set but empty is a misconfiguration, not a
			// credential. Treating it as present produces a 401 that looks like
			// a bad key rather than a missing one.
			if strings.TrimSpace(value) == "" {
				continue
			}
			return NewSecret(strings.TrimSpace(value), name), nil
		}
	}
	if req.Optional {
		// Absent, but not an error: the caller learns this from Secret.Present,
		// and an adapter for such a provider sends no auth header. The source
		// records why it is empty so a diagnostic does not read as a lookup
		// that silently found nothing.
		return NewSecret("", "not required"), nil
	}
	return Secret{}, &ErrMissing{Provider: provider, EnvVars: req.EnvVars, Doc: req.Doc}
}

// Status is what a provider's credential looks like from the outside: enough to
// diagnose, nothing that could be used.
type Status struct {
	Provider string `json:"provider"`
	Present  bool   `json:"present"`
	Source   string `json:"source,omitempty"`
	Length   int    `json:"length,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Statuses reports every registered provider, sorted, for `harness doctor`.
func (r *Resolver) Statuses() []Status {
	r.mu.RLock()
	names := make([]string, 0, len(r.requirements))
	for name := range r.requirements {
		names = append(names, name)
	}
	r.mu.RUnlock()
	sort.Strings(names)

	out := make([]Status, 0, len(names))
	for _, name := range names {
		secret, err := r.Resolve(name)
		status := Status{Provider: name}
		switch {
		case err != nil:
			status.Detail = err.Error()
		case secret.Present():
			status.Present = true
			status.Source = secret.Source()
			status.Length = secret.Len()
		default:
			// Resolved, but with nothing in it: an optional provider with no key
			// set. Reporting Present here would claim a credential that does not
			// exist, which is the kind of small lie a doctor command is for
			// catching rather than telling.
			status.Detail = "no credential set, and none is required for this provider"
		}
		out = append(out, status)
	}
	return out
}

// Scrubber removes known credential values from text on its way to a log, a
// terminal, or a model.
//
// It is the backstop, not the mechanism. The Secret type is what keeps
// credentials out of formatted output; this catches the cases the type cannot
// reach — a provider that echoes a key back inside an error body, a subprocess
// that prints its own environment, a stack trace assembled by something outside
// this program.
type Scrubber struct {
	mu     sync.RWMutex
	values []string
}

// NewScrubber returns an empty scrubber.
func NewScrubber() *Scrubber { return &Scrubber{} }

// Watch adds a secret's value to the set that gets removed.
func (s *Scrubber) Watch(secret Secret) {
	if !secret.Present() {
		return
	}
	// Very short values are not scrubbed. A three-character "key" would match
	// ordinary prose and turn every log line into redaction marks, which
	// destroys the diagnostics without protecting anything worth protecting.
	if secret.Len() < 8 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.values {
		if existing == secret.value {
			return
		}
	}
	s.values = append(s.values, secret.value)
	// Longest first, so a key that contains another key's prefix is replaced
	// whole rather than leaving a tail behind.
	sort.Slice(s.values, func(i, j int) bool { return len(s.values[i]) > len(s.values[j]) })
}

// WatchAll adds every credential a resolver can currently produce.
func (s *Scrubber) WatchAll(r *Resolver) {
	for _, status := range r.Statuses() {
		if !status.Present {
			continue
		}
		if secret, err := r.Resolve(status.Provider); err == nil {
			s.Watch(secret)
		}
	}
}

// Clean returns text with every watched credential replaced.
func (s *Scrubber) Clean(text string) string {
	s.mu.RLock()
	values := s.values
	s.mu.RUnlock()
	for _, v := range values {
		text = strings.ReplaceAll(text, v, Redacted)
	}
	return text
}

// Count reports how many distinct values are being scrubbed, so a run report
// can say the backstop is armed without describing what it holds.
func (s *Scrubber) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.values)
}
