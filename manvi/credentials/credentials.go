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

	"github.com/awnumar/memguard"
)

// memguard.CatchInterrupt and memguard.Purge are deliberately not called here.
//
// CatchInterrupt installs memguard's own signal handler, and this harness
// already owns signals: the TUI catches SIGWINCH to re-measure the terminal and
// SIGTSTP/SIGCONT to hand the terminal back and take it again, and a second
// handler that exits the process on a signal would break the suspend path
// outright. Purge would be correct at a clean exit, and buys little: the pages
// are mlocked, so the kernel reclaims and zeroes them at process death whether
// or not they were wiped first, and a harness that dies on a panic never
// reaches a deferred Purge anyway.
//
// The protection this package claims is therefore about the *running* process —
// no plaintext on the Go heap, nothing pageable to swap, nothing in a core dump
// taken while the harness is alive — and not about exit.

// Redacted is what a secret renders as everywhere.
const Redacted = "[redacted]"

// Secret holds a credential. The zero value is "not present".
//
// The value is sealed in a memguard Enclave rather than held as a string: it
// is encrypted at rest, and the plaintext exists only inside an mlocked buffer
// for the duration of a Reveal, which wipes it on the way out. What that buys
// is narrow and worth stating exactly, because a protection nobody can
// describe is a protection nobody can rely on:
//
//   - A Go string cannot be wiped. It lives on the heap until the collector
//     takes it, it can be paged to swap, and it appears in a core dump. That
//     was the shape a credential had here for its whole lifetime, and it is
//     the shape it no longer has.
//
//   - What this does NOT fix: keys arrive from os.LookupEnv, so the plaintext
//     is already in this process's environment block before Secret exists —
//     readable from /proc/<pid>/environ, from `ps e`, and in the same core
//     dump. Sealing the harness's copy does not reach the origin, and nothing
//     inside this package can. An operator who needs that closed has to stop
//     passing keys through the environment, which is a deployment decision
//     rather than a code one.
//
// Enclave is a pointer, so a Secret stays copyable and its zero value stays
// "absent".
type Secret struct {
	sealed *memguard.Enclave
	// source names where it came from, for diagnostics. Safe to print.
	source string
	// length is cached because reporting it must not require an Open. It is
	// the one fact about a credential this package prints.
	length int
	// err is why a supplied value could not be sealed. Such a secret is
	// present — a credential was given — but every Reveal returns err, so the
	// failure surfaces where the key is used rather than as a missing key.
	err error
}

// NewSecret seals a value read from source.
//
// The caller hands in a string it cannot wipe, which is unavoidable at this
// boundary — os.LookupEnv has no other shape. That copy is the last unsealed
// one; everything downstream holds the enclave.
//
// Sealing can fail when the process cannot get locked memory (ErrLockedMemory).
// NewSecret has no error to return, so the failure is carried in the Secret and
// returned by Reveal and RevealBytes, which every caller already checks. The
// value is not kept anywhere in that case. Resolve reports the same failure
// directly.
func NewSecret(value, source string) Secret {
	s, err := seal(value, source)
	if err != nil {
		return Secret{source: source, length: len(value), err: err}
	}
	return s
}

// seal is NewSecret with the failure returned.
func seal(value, source string) (Secret, error) {
	if value == "" {
		return Secret{source: source}, nil
	}
	enclave, err := guarded(enclavePages, func() *memguard.Enclave {
		return memguard.NewEnclave([]byte(value))
	}, nil)
	if err != nil {
		return Secret{}, fmt.Errorf("credentials: sealing the credential from %s: %w", source, err)
	}
	return Secret{sealed: enclave, source: source, length: len(value)}, nil
}

// opened is the result of Enclave.Open, carried through guarded.
type opened struct {
	buf *memguard.LockedBuffer
	err error
}

// open decrypts the enclave into guarded memory. The caller destroys the
// buffer.
func (s Secret) open() (*memguard.LockedBuffer, error) {
	if s.err != nil {
		return nil, s.err
	}
	res, err := guarded(pagesFor(s.length)+enclavePages, func() opened {
		buf, err := s.sealed.Open()
		return opened{buf: buf, err: err}
	}, func(late opened) {
		if late.buf != nil {
			late.buf.Destroy()
		}
	})
	if err == nil {
		err = res.err
	}
	if err != nil {
		return nil, fmt.Errorf("credentials: opening the sealed credential from %s: %w", s.source, err)
	}
	return res.buf, nil
}

// Present reports whether a credential was found.
func (s Secret) Present() bool { return s.sealed != nil || s.err != nil }

// Source names where the credential came from — an environment variable name,
// never its contents.
func (s Secret) Source() string { return s.source }

// Reveal returns the credential itself.
//
// Every call is a place the value can escape, so there should be very few, and
// each should hand the result straight to the thing that consumes it. Assigning
// it to a variable that later reaches a formatted string undoes the type.
//
// It returns an error rather than an empty string on failure. Opening a sealed
// enclave can fail — a wiped or tampered container is exactly the case worth
// hearing about — and an empty key produces a 401 that reads like a bad
// credential, which sends whoever is holding the pager to the wrong place.
// Every call site is already inside a function that returns an error.
func (s Secret) Reveal() (string, error) {
	if !s.Present() {
		return "", nil
	}
	buf, err := s.open()
	if err != nil {
		return "", err
	}
	defer buf.Destroy()
	// string(buf.Bytes()) copies. LockedBuffer.String is a zero-copy view into
	// the guarded page, and Destroy unmaps that page — returning the view
	// hands the caller a string over freed memory, which faults on the next
	// read rather than at the mistake. The copy is the unavoidable cost of a
	// signature that returns a string, and it is why RevealBytes exists.
	return string(buf.Bytes()), nil
}

// RevealBytes runs fn with the credential in guarded memory, and wipes it
// afterwards whatever fn does.
//
// This is the form to prefer. Reveal has to materialise a Go string, which
// cannot be wiped and outlives the call; the bytes handed to fn live in an
// mlocked buffer that is destroyed on return. fn must not retain the slice —
// it is unmapped before this returns. Reveal remains for the callers that hand
// the value to an API taking a string, which is what http.Header.Set is.
func (s Secret) RevealBytes(fn func([]byte) error) error {
	if !s.Present() {
		return fn(nil)
	}
	buf, err := s.open()
	if err != nil {
		return err
	}
	defer buf.Destroy()
	return fn(buf.Bytes())
}

// Len returns the credential's length, which is safe to report and is often
// enough to diagnose a truncated or padded key without printing it.
func (s Secret) Len() int { return s.length }

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
func (s Secret) Format(f fmt.State, verb rune) { _, _ = fmt.Fprint(f, s.String()) }

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
	// Provider is the adapter name, matching llm.Provider.Name — or, for
	// AnthropicFallback, the name of a second credential an adapter holds.
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
	// Related names variables that are not this provider's key but travel
	// with it: the account or workspace the key acts in, or another bearer
	// form the vendor's own tooling reads. Resolve never consults them. They
	// are here so that ProviderEnv, the set no child process may inherit, is
	// derived from this catalogue rather than hand-listed beside it.
	Related []string
}

// AnthropicWorkspaceEnv names the workspace an Anthropic key that is not
// scoped to one acts in. It is not a secret, but it routes the key's spend,
// so it is withheld from children with the key.
const AnthropicWorkspaceEnv = "ANTHROPIC_WORKSPACE_ID"

// AnthropicFallback is the requirement for a second Anthropic key, in another
// organisation, that the anthropic adapter moves a request to when the primary
// key's organisation has run out of prepaid credit. Credit lives in an
// organisation and cannot be pooled across two, so two funded organisations
// are two keys.
//
// It is a requirement of its own, not a second variable on "anthropic",
// because Resolve's "first variable set wins" would then pick one key and
// never both. Being in the catalogue is what withholds it from children
// (ProviderEnv) and arms the scrubber against it (WatchAll). It is not an
// adapter: buildProvider constructs no provider by this name.
const AnthropicFallback = "anthropic-fallback"

// AnthropicFallbackKeyEnv and AnthropicFallbackWorkspaceEnv carry the fallback
// key and the workspace it acts in. The workspace is the fallback's own: the
// primary's ANTHROPIC_WORKSPACE_ID belongs to the primary's organisation and
// is a 404 under any other key.
const (
	AnthropicFallbackKeyEnv       = "ANTHROPIC_API_KEY_FALLBACK"
	AnthropicFallbackWorkspaceEnv = "ANTHROPIC_WORKSPACE_ID_FALLBACK"
)

// ProviderEnv names every environment variable that carries a provider
// credential or scopes one — each requirement's EnvVars and Related — sorted,
// each once.
//
// It is the one answer to "what must a child of this process never see".
// Every child environment is either built from an allowlist that a test
// checks against this set, or is this process's environment with this set
// removed.
func ProviderEnv() []string {
	seen := map[string]bool{}
	var names []string
	for _, req := range DefaultRequirements() {
		for _, list := range [][]string{req.EnvVars, req.Related} {
			for _, name := range list {
				if !seen[name] {
					seen[name] = true
					names = append(names, name)
				}
			}
		}
	}
	sort.Strings(names)
	return names
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
			// ANTHROPIC_AUTH_TOKEN is the bearer form the vendor SDK and
			// Claude Code read for a gateway. This harness does not use it,
			// but a child that inherited it would bill the account it names.
			Related: []string{AnthropicWorkspaceEnv, "ANTHROPIC_AUTH_TOKEN"},
		},
		{
			Provider: AnthropicFallback,
			EnvVars:  []string{AnthropicFallbackKeyEnv},
			Doc:      "a key from a second Console organisation, used when the first is out of credit (https://console.anthropic.com/settings/keys)",
			Related:  []string{AnthropicFallbackWorkspaceEnv},
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
		// An override NewSecret could not seal is reported here, where the
		// caller expects a resolution failure, rather than handed back as a
		// credential that fails only when it is revealed.
		if override.err != nil {
			return Secret{}, override.err
		}
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
			return seal(strings.TrimSpace(value), name)
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
	// EnvVars and Doc are set when the credential is missing: the variables
	// any one of which would satisfy it, and where a key is issued. They are
	// the structured form of Detail, for a display that lays them out rather
	// than printing one sentence.
	EnvVars []string `json:"env_vars,omitempty"`
	Doc     string   `json:"doc,omitempty"`
}

// providers names every registered provider, sorted.
func (r *Resolver) providers() []string {
	r.mu.RLock()
	names := make([]string, 0, len(r.requirements))
	for name := range r.requirements {
		names = append(names, name)
	}
	r.mu.RUnlock()
	sort.Strings(names)
	return names
}

// Statuses reports every registered provider, sorted, for `harness doctor`.
func (r *Resolver) Statuses() []Status {
	names := r.providers()
	out := make([]Status, 0, len(names))
	for _, name := range names {
		secret, err := r.Resolve(name)
		status := Status{Provider: name}
		switch {
		case err != nil:
			status.Detail = err.Error()
			var missing *ErrMissing
			if errors.As(err, &missing) {
				status.EnvVars = missing.EnvVars
				status.Doc = missing.Doc
			}
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
	mu sync.RWMutex
	// values are held in guarded, mlocked buffers rather than as strings.
	//
	// This matters more than the Secret type does. A Scrubber keeps every
	// credential it watches for the lifetime of the process — that is its job —
	// so before this it held the longest-lived plaintext copy in the program,
	// and sealing Secret while this held strings would have been ceremony.
	//
	// Clean runs on every rendered line, every appended log payload and every
	// tool result, so opening an enclave per call was not an option. It does
	// not have to: LockedBuffer.String is a view into the guarded page, not a
	// copy, so the match reads the plaintext in place. The buffers are frozen
	// (read-only) and destroyed by Purge at exit.
	values []*memguard.LockedBuffer
	// unarmed is why a value this scrubber was asked to watch is not among
	// values — it could not be opened, or there was no locked memory to hold
	// it — or why values themselves are gone. Once set it stays set: the
	// scrubber can no longer say a text is free of every credential it was
	// given, so Clean withholds text rather than pass it on as cleaned.
	unarmed error
}

// minScrubLen is the shortest value Watch keeps; see Watch.
const minScrubLen = 8

// Withheld begins what Clean returns in place of a text once the scrubber is
// unarmed. It is followed by the reason, which names a source and an errno and
// never a value.
const Withheld = "[withheld: the credential scrubber is not armed"

// errScrubberWiped is the reason when a watched buffer has been destroyed
// under the scrubber, which only a memguard Purge does.
var errScrubberWiped = errors.New("credentials: a watched value was wiped by memguard")

// markUnarmed records the first reason the scrubber stopped covering
// everything it was given. Callers hold s.mu for writing.
func (s *Scrubber) markUnarmed(err error) {
	if s.unarmed == nil {
		s.unarmed = err
	}
}

// NewScrubber returns an empty scrubber.
func NewScrubber() *Scrubber { return &Scrubber{} }

// Watch adds a secret's value to the set that gets removed.
//
// It returns nothing, and its callers re-arm on every use, so a value it cannot
// take is not reported here: it leaves the scrubber unarmed, and Clean says so
// in place of every text from then on.
func (s *Scrubber) Watch(secret Secret) {
	if !secret.Present() {
		return
	}
	// Very short values are not scrubbed. A three-character "key" would match
	// ordinary prose and turn every log line into redaction marks, which
	// destroys the diagnostics without protecting anything worth protecting.
	if secret.Len() < minScrubLen {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// RevealBytes rather than Reveal: the value goes from one guarded buffer
	// straight into another without a Go string in between. A secret that
	// cannot be opened, or a value with no locked memory to hold it
	// (ErrLockedMemory), is never held in pageable memory instead; it unarms
	// the scrubber.
	err := secret.RevealBytes(func(raw []byte) error {
		for _, existing := range s.values {
			if existing.EqualTo(raw) {
				return nil
			}
		}
		// Allocate-and-copy, not NewBufferFromBytes. That constructor *moves*:
		// it wipes its source, and the source here is the buffer Enclave.Open
		// returned, which memguard hands back read-only. Writing to it is a
		// SIGBUS on a guarded page — a fault at the wipe, not at the mistake.
		buf, err := guarded(pagesFor(len(raw)), func() *memguard.LockedBuffer {
			return memguard.NewBuffer(len(raw))
		}, func(late *memguard.LockedBuffer) { late.Destroy() })
		if err != nil {
			return err
		}
		buf.Copy(raw)
		buf.Freeze()
		s.values = append(s.values, buf)
		// Longest first, so a key that contains another key's prefix is
		// replaced whole rather than leaving a tail behind.
		sort.Slice(s.values, func(i, j int) bool { return s.values[i].Size() > s.values[j].Size() })
		return nil
	})
	if err != nil {
		s.markUnarmed(fmt.Errorf("credentials: the scrubber could not watch the value from %s: %w", secret.Source(), err))
	}
}

// WatchValue watches a raw value that is not a provider key — an environment
// variable forwarded to a child process, say. A value that is already watched,
// or too short to scrub, is skipped before it is sealed, so callers may re-arm
// on every use without paying for the same enclave twice.
func (s *Scrubber) WatchValue(value, source string) {
	if len(value) < minScrubLen {
		return
	}
	raw := []byte(value)
	s.mu.RLock()
	for _, existing := range s.values {
		if existing.EqualTo(raw) {
			s.mu.RUnlock()
			return
		}
	}
	s.mu.RUnlock()
	s.Watch(NewSecret(value, source))
}

// WatchAll adds every credential a resolver can currently produce.
//
// A provider with no credential is skipped. One whose credential is set but
// cannot be resolved — it could not be sealed — unarms the scrubber: the value
// is still in this process's environment, where a child or an error body can
// echo it, and the scrubber does not hold it.
func (s *Scrubber) WatchAll(r *Resolver) {
	for _, name := range r.providers() {
		secret, err := r.Resolve(name)
		var missing *ErrMissing
		switch {
		case errors.As(err, &missing):
			continue
		case err != nil:
			s.mu.Lock()
			s.markUnarmed(err)
			s.mu.Unlock()
			continue
		}
		s.Watch(secret)
	}
}

// Clean returns text with every watched credential replaced.
//
// An unarmed scrubber returns Withheld and the reason instead of the text,
// because the text may carry a value it was never able to watch. So does one
// whose buffers memguard has destroyed: a destroyed buffer reads as the empty
// string, and replacing "" would put a redaction mark between every character
// of the text while removing nothing.
func (s *Scrubber) Clean(text string) string {
	s.mu.RLock()
	values, unarmed := s.values, s.unarmed
	s.mu.RUnlock()
	if unarmed != nil {
		return withheld(unarmed)
	}
	for _, v := range values {
		// v.String() is a view into the guarded page, so the credential is
		// never copied onto the heap to be searched for.
		needle := v.String()
		if needle == "" {
			s.mu.Lock()
			s.markUnarmed(errScrubberWiped)
			unarmed = s.unarmed
			s.mu.Unlock()
			return withheld(unarmed)
		}
		text = strings.ReplaceAll(text, needle, Redacted)
	}
	return text
}

func withheld(reason error) string { return Withheld + ": " + reason.Error() + "]" }

// MaxLen is the length in bytes of the longest value being scrubbed, zero when
// there is none. A caller that searches a stream for these values in windows
// needs it: a window must overlap the next by one byte less than this for a
// value that straddles the edge to be found whole. values is sorted longest
// first (see Watch), so it is the first one's size.
func (s *Scrubber) MaxLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.values) == 0 {
		return 0
	}
	return s.values[0].Size()
}

// Count reports how many distinct values are being scrubbed, so a run report
// can say the backstop is armed without describing what it holds.
func (s *Scrubber) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.values)
}
