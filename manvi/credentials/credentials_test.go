package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

const probe = "sk-ant-do-not-log-me-0123456789"

func resolverWith(env map[string]string) *Resolver {
	r := NewResolver()
	r.lookup = func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
	return r
}

// TestASecretNeverRendersItself walks every route a value can take out of a Go
// program as text. This is the whole point of the type: a redaction that holds
// under %s and fails under %#v is not a redaction.
func TestASecretNeverRendersItself(t *testing.T) {
	s := NewSecret(probe, "ANTHROPIC_API_KEY")

	renderings := map[string]string{
		"String":   s.String(),
		"%s":       fmt.Sprintf("%s", s),
		"%v":       fmt.Sprintf("%v", s),
		"%+v":      fmt.Sprintf("%+v", s),
		"%#v":      fmt.Sprintf("%#v", s),
		"%q":       fmt.Sprintf("%q", s),
		"%d":       fmt.Sprintf("%d", s),
		"Sprint":   fmt.Sprint(s),
		"Sprintln": fmt.Sprintln(s),
		"error":    fmt.Errorf("request failed with %v", s).Error(),
	}

	// The same checks with the secret nested inside a struct, which is how it
	// actually travels — a config printed with %+v is the realistic leak.
	type config struct {
		Provider string
		Key      Secret
		BaseURL  string
	}
	c := config{Provider: "anthropic", Key: s, BaseURL: "https://api.anthropic.com"}
	renderings["struct %v"] = fmt.Sprintf("%v", c)
	renderings["struct %+v"] = fmt.Sprintf("%+v", c)
	renderings["struct %#v"] = fmt.Sprintf("%#v", c)
	renderings["pointer %+v"] = fmt.Sprintf("%+v", &c)

	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	renderings["json.Marshal"] = string(encoded)

	indented, err := json.MarshalIndent(map[string]any{"cfg": c, "key": s}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	renderings["json.MarshalIndent"] = string(indented)

	for name, text := range renderings {
		if strings.Contains(text, probe) {
			t.Errorf("%s leaked the credential: %s", name, text)
		}
	}

	if s.Reveal() != probe {
		t.Fatal("Reveal must return the credential; it is the one way out")
	}
	if s.Source() != "ANTHROPIC_API_KEY" || s.Len() != len(probe) {
		t.Fatalf("source/len = %q/%d", s.Source(), s.Len())
	}
}

// TestASecretCannotBeDecodedFromADocument: accepting one would mean a document
// somewhere holds a key, which is the arrangement this package exists to avoid.
func TestASecretCannotBeDecodedFromADocument(t *testing.T) {
	var s Secret
	if err := json.Unmarshal([]byte(`"sk-whatever"`), &s); err == nil {
		t.Fatal("a Secret was decoded from JSON")
	}
	if s.Present() {
		t.Fatal("a refused decode left a value behind")
	}
}

// TestResolutionOrderAndDiagnostics pins the parts an operator relies on when
// it does not work.
func TestResolutionOrderAndDiagnostics(t *testing.T) {
	r := resolverWith(map[string]string{"XAI_API_KEY": "first", "GROK_API_KEY": "second"})
	got, err := r.Resolve("xai")
	if err != nil {
		t.Fatal(err)
	}
	if got.Reveal() != "first" || got.Source() != "XAI_API_KEY" {
		t.Fatalf("resolved %q from %q, want the first listed variable to win", got.Reveal(), got.Source())
	}

	// The fallback name is honoured, because an operator with a working CLI
	// should not have to learn a second name for the same key.
	r = resolverWith(map[string]string{"GROK_API_KEY": "second"})
	if got, err = r.Resolve("xai"); err != nil || got.Reveal() != "second" {
		t.Fatalf("fallback variable: %q %v", got.Reveal(), err)
	}

	// Set-but-empty is a misconfiguration, not a credential: treating it as
	// present produces a 401 that looks like a bad key rather than a missing one.
	r = resolverWith(map[string]string{"XAI_API_KEY": "   ", "GROK_API_KEY": "real-key"})
	if got, err = r.Resolve("xai"); err != nil || got.Reveal() != "real-key" {
		t.Fatalf("empty variable was treated as a credential: %q %v", got.Reveal(), err)
	}

	// Surrounding whitespace is stripped: a key pasted with a trailing newline
	// is the single commonest way this fails, and it fails as "invalid key".
	r = resolverWith(map[string]string{"ANTHROPIC_API_KEY": "  " + probe + "\n"})
	if got, err = r.Resolve("anthropic"); err != nil || got.Reveal() != probe {
		t.Fatalf("whitespace was not trimmed: %q %v", got.Reveal(), err)
	}

	r = resolverWith(nil)
	_, err = r.Resolve("gemini")
	var missing *ErrMissing
	if !errors.As(err, &missing) {
		t.Fatalf("err = %v, want *ErrMissing", err)
	}
	for _, name := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		if !strings.Contains(missing.Error(), name) {
			t.Errorf("the error must name %s so an operator knows what to set: %v", name, missing)
		}
	}
	if _, err := r.Resolve("no-such-provider"); err == nil {
		t.Fatal("an unregistered provider must error rather than report no credential")
	}
}

// TestStatusesDescribeWithoutDisclosing is what `harness doctor` prints.
func TestStatusesDescribeWithoutDisclosing(t *testing.T) {
	r := resolverWith(map[string]string{"ANTHROPIC_API_KEY": probe})
	statuses := r.Statuses()

	encoded, err := json.Marshal(statuses)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), probe) {
		t.Fatalf("a status report disclosed the credential: %s", encoded)
	}

	var anthropic Status
	for _, s := range statuses {
		if s.Provider == "anthropic" {
			anthropic = s
		}
	}
	if !anthropic.Present || anthropic.Source != "ANTHROPIC_API_KEY" || anthropic.Length != len(probe) {
		t.Fatalf("anthropic status = %+v, want present with a source and a length", anthropic)
	}
	// Providers are listed in a stable order so doctor output is diffable.
	for i := 1; i < len(statuses); i++ {
		if statuses[i-1].Provider >= statuses[i].Provider {
			t.Fatalf("statuses are not sorted: %+v", statuses)
		}
	}
}

// TestScrubberCatchesWhatTheTypeCannot covers the realistic leak: a provider
// echoing the key back inside an error body the harness then logs.
func TestScrubberCatchesWhatTheTypeCannot(t *testing.T) {
	r := resolverWith(map[string]string{"ANTHROPIC_API_KEY": probe})
	s := NewScrubber()
	s.WatchAll(r)
	if s.Count() != 1 {
		t.Fatalf("scrubbing %d values, want 1", s.Count())
	}

	body := fmt.Sprintf(`{"error":{"message":"invalid x-api-key: %s"}}`, probe)
	cleaned := s.Clean(body)
	if strings.Contains(cleaned, probe) {
		t.Fatalf("the scrubber missed an echoed key: %s", cleaned)
	}
	if !strings.Contains(cleaned, Redacted) {
		t.Fatalf("the scrubber removed without marking: %s", cleaned)
	}

	// A short value is not watched: scrubbing three-character strings would
	// replace ordinary prose and destroy the diagnostics it is meant to protect.
	s2 := NewScrubber()
	s2.Watch(NewSecret("abc", "SHORT"))
	if s2.Count() != 0 {
		t.Fatal("a too-short value was watched")
	}
	if got := s2.Clean("abc is a common substring"); got != "abc is a common substring" {
		t.Fatalf("clean = %q, want untouched", got)
	}

	// An absent secret is not watched, so the empty string is never a target —
	// which would otherwise replace between every character.
	s3 := NewScrubber()
	s3.Watch(Secret{})
	if got := s3.Clean("hello"); got != "hello" {
		t.Fatalf("clean = %q after watching an absent secret", got)
	}
}

// TestScrubberIsSafeUnderConcurrency: it is called from the log writer and the
// terminal renderer at once.
func TestScrubberIsSafeUnderConcurrency(t *testing.T) {
	s := NewScrubber()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Watch(NewSecret(fmt.Sprintf("secret-value-number-%02d", i), "ENV"))
			if got := s.Clean("secret-value-number-07 appeared"); strings.Contains(got, "number-07") {
				// Only meaningful once that value has been watched; the check
				// that matters is that this races without corrupting state.
				_ = got
			}
		}(i)
	}
	wg.Wait()
	if s.Count() != 32 {
		t.Fatalf("watching %d values, want 32", s.Count())
	}
	if strings.Contains(s.Clean("secret-value-number-07 appeared"), "secret-value-number-07") {
		t.Fatal("a watched value survived Clean")
	}
}

// TestOverridesDoNotEscapeToTheEnvironment: an in-process credential is how a
// test or an embedding program supplies one, and it must behave identically.
func TestOverridesDoNotEscapeToTheEnvironment(t *testing.T) {
	r := resolverWith(nil)
	r.Set("anthropic", NewSecret(probe, "in-process"))
	got, err := r.Resolve("anthropic")
	if err != nil || got.Reveal() != probe {
		t.Fatalf("override not honoured: %q %v", got.Reveal(), err)
	}
	if fmt.Sprintf("%+v", r.Statuses()) == "" || strings.Contains(fmt.Sprintf("%+v", r.Statuses()), probe) {
		t.Fatal("statuses disclosed an in-process credential")
	}
}
