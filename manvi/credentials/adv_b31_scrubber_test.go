package credentials

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// There is no Unwatch: a watched value is never removed, so "removed between
// the read check and the write lock" cannot happen. What can happen is a lost or
// doubled publish under contention, and a Clean that sees a value un-scrubbed
// after the Watch that published it returned.
func TestAdvB31OpsScrubberWatchCleanUnderContention(t *testing.T) {
	const distinct = 24
	value := func(k int) string { return fmt.Sprintf("advb31-credential-%02d-%s", k, strings.Repeat("x", k%7)) } // #nosec G101 -- fixtures
	s := NewScrubber()
	var wg sync.WaitGroup
	var lastCount atomic.Int64
	start := make(chan struct{})
	for i := range 1000 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			k := i % distinct
			v := value(k)
			switch i % 5 {
			case 0, 1:
				s.Watch(NewSecret(v, "adv"))
			case 2:
				s.WatchValue(v, "adv")
			case 3:
				// A Clean after this goroutine's own Watch returned must remove it.
				s.Watch(NewSecret(v, "adv"))
				if out := s.Clean("a " + v + " b"); strings.Contains(out, v) {
					t.Errorf("value %d still present after its Watch returned: %q", k, out)
				}
			case 4:
				_ = s.Clean(strings.Join([]string{value(0), value(1), value(2), value(k)}, " "))
				c := int64(s.Count())
				for {
					prev := lastCount.Load()
					if c >= prev || lastCount.CompareAndSwap(prev, c) {
						break
					}
				}
				if m := s.MaxLen(); m < 0 {
					t.Errorf("MaxLen %d", m)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	s.mu.RLock()
	held := len(s.values)
	unarmed := s.unarmed
	sizes := make([]int, len(s.values))
	for i, b := range s.values {
		sizes[i] = b.Size()
	}
	s.mu.RUnlock()
	if unarmed != nil {
		t.Fatalf("scrubber unarmed: %v", unarmed)
	}
	if held != distinct {
		t.Fatalf("holding %d values, want %d distinct (lost or doubled publish)", held, distinct)
	}
	for i := 1; i < len(sizes); i++ {
		if sizes[i-1] < sizes[i] {
			t.Fatalf("values not sorted longest first: %v", sizes)
		}
	}
	for k := range distinct {
		if out := s.Clean(value(k)); strings.Contains(out, value(k)) {
			t.Fatalf("value %d survived Clean", k)
		}
	}
}

// WatchAll re-arming the same resolver concurrently with Watch and Clean.
func TestAdvB31OpsScrubberWatchAllInterleaved(t *testing.T) {
	r := NewResolver()
	var gen atomic.Int64
	r.lookup = func(name string) (string, bool) {
		return fmt.Sprintf("advb31-resolver-%s-%d", name, gen.Load()%4), true
	}
	s := NewScrubber()
	var wg sync.WaitGroup
	for i := range 1000 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 4 {
			case 0:
				gen.Add(1)
				s.WatchAll(r)
			case 1:
				s.WatchValue(fmt.Sprintf("advb31-direct-value-%d", i%9), "adv")
			case 2:
				_ = s.Clean("some text advb31-resolver-anthropic-1 and more")
			case 3:
				_ = s.Count()
				_ = s.MaxLen()
			}
		}()
	}
	wg.Wait()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.unarmed != nil {
		t.Fatalf("unarmed: %v", s.unarmed)
	}
	seen := map[string]int{}
	for _, b := range s.values {
		seen[b.String()]++
	}
	for v, n := range seen {
		if n != 1 {
			t.Fatalf("value %q held %d times", v, n)
		}
	}
}
