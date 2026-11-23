package credentials

import (
	"strings"
	"sync"
	"testing"
)

// Watch checks for a duplicate under the read lock and again before it
// publishes, so concurrent callers re-arming the same value leave exactly one
// copy and Clean still removes it.
func TestConcurrentWatchOfOneValueHoldsItOnce(t *testing.T) {
	const value = "concurrent-watch-credential-value" // #nosec G101 -- a fixture, not a credential
	s := NewScrubber()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.Watch(NewSecret(value, "test"))
		}()
	}
	close(start)
	wg.Wait()

	s.mu.RLock()
	held := len(s.values)
	s.mu.RUnlock()
	if held != 1 {
		t.Fatalf("%d copies of one value are held, want 1", held)
	}
	if out := s.Clean("before " + value + " after"); strings.Contains(out, value) || strings.HasPrefix(out, Withheld) {
		t.Fatalf("Clean = %q, want the value removed from an armed scrubber", out)
	}
}
