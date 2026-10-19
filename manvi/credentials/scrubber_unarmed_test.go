package credentials

import (
	"errors"
	"strings"
	"testing"
)

// failProbeOnCall makes the n-th locked-memory probe from now fail, and every
// other one behave as it does in production.
func failProbeOnCall(t *testing.T, n int) {
	t.Helper()
	saved := lockedMemoryProbe
	calls := 0
	lockedMemoryProbe = func(pages int) error {
		calls++
		if calls == n {
			return errors.New("injected: no locked memory")
		}
		return saved(pages)
	}
	t.Cleanup(func() { lockedMemoryProbe = saved })
}

// assertWithheld fails unless text is the unarmed scrubber's notice, with no
// trace of the credential in it.
func assertWithheld(t *testing.T, text string) {
	t.Helper()
	if !strings.HasPrefix(text, Withheld) {
		t.Fatalf("Clean = %q, want the text withheld", text)
	}
	if strings.Contains(text, probe) {
		t.Fatalf("the withheld notice carried the credential: %q", text)
	}
}

// TestTheScrubbersOwnAllocationFailingUnarmsIt: the scrubber copies a value
// into a buffer of its own after opening the secret. When that allocation
// cannot get locked memory the value is not watched, and the scrubber must
// say so rather than pass text through as though it had been cleaned.
func TestTheScrubbersOwnAllocationFailingUnarmsIt(t *testing.T) {
	secret := NewSecret(probe, "ANTHROPIC_API_KEY")
	s := NewScrubber()
	failProbeOnCall(t, 2) // the first probe is the Open, the second the copy
	s.Watch(secret)
	if s.Count() != 0 {
		t.Fatalf("watching %d values after the allocation failed", s.Count())
	}
	assertWithheld(t, s.Clean("invalid x-api-key: "+probe))
}

// TestAnUnarmedScrubberStaysUnarmed: a value it could not take may already be
// in a text it is given later, so a later successful Watch does not restore
// it.
func TestAnUnarmedScrubberStaysUnarmed(t *testing.T) {
	s := NewScrubber()
	s.Watch(Secret{source: "in-process", length: len(probe), err: ErrLockedMemory})
	s.Watch(NewSecret("another-credential-value", "OTHER"))
	assertWithheld(t, s.Clean("ordinary text"))
}

// TestCleanWithholdsAfterMemguardWipesAWatchedValue: a memguard Purge destroys
// every guarded buffer, and a destroyed buffer reads as "". Replacing "" puts
// a redaction mark between every character and removes nothing, so the
// credential survives, interleaved. Clean must withhold instead.
func TestCleanWithholdsAfterMemguardWipesAWatchedValue(t *testing.T) {
	s := NewScrubber()
	s.Watch(NewSecret(probe, "ANTHROPIC_API_KEY"))
	if s.Count() != 1 {
		t.Fatalf("watching %d values, want 1", s.Count())
	}
	s.values[0].Destroy()
	got := s.Clean("echoed: " + probe)
	assertWithheld(t, got)
	if !strings.Contains(got, errScrubberWiped.Error()) {
		t.Fatalf("Clean = %q, want the wipe named as the reason", got)
	}
}

// TestAnOverrideThatCouldNotBeSealedFailsResolution: NewSecret carries a seal
// failure rather than returning it. Handed to Set, it must fail Resolve, not
// come back as a credential that fails only when an adapter reveals it.
func TestAnOverrideThatCouldNotBeSealedFailsResolution(t *testing.T) {
	r := resolverWith(nil)
	r.Set("anthropic", Secret{source: "in-process", length: len(probe), err: ErrLockedMemory})
	if _, err := r.Resolve("anthropic"); !errors.Is(err, ErrLockedMemory) {
		t.Fatalf("Resolve = %v, want the seal failure", err)
	}
}

// TestWatchAllUnarmsOnACredentialItCannotHold: a key that is set but cannot be
// sealed is still in this process's environment, where a child or an error
// body can echo it. Skipping it as though it were missing would leave the
// scrubber reporting clean text it never checked for that key.
func TestWatchAllUnarmsOnACredentialItCannotHold(t *testing.T) {
	r := resolverWith(nil)
	r.Set("anthropic", Secret{source: "in-process", length: len(probe), err: ErrLockedMemory})
	s := NewScrubber()
	s.WatchAll(r)
	assertWithheld(t, s.Clean("ordinary text"))

	// Missing credentials are not a failure to hold one.
	quiet := NewScrubber()
	quiet.WatchAll(resolverWith(nil))
	if got := quiet.Clean("ordinary text"); got != "ordinary text" {
		t.Fatalf("a resolver with no credentials unarmed the scrubber: %q", got)
	}
}
