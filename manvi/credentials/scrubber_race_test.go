package credentials

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Clean used to copy the slice header under the read lock and then walk the
// shared backing array after unlocking, while Watch re-sorted that same array in
// place under the write lock. Callers re-arm on every use while tool results
// and sub-agents Clean concurrently, so the two meet. Run with -race: the
// detector reports the walk against the sort.
func TestScrubberCleanDoesNotWalkAnArrayWatchSorts(t *testing.T) {
	s := NewScrubber()
	for i := 0; i < 4; i++ {
		s.WatchValue(fmt.Sprintf("seed-credential-value-%02d-%s", i, strings.Repeat("x", i)), "test")
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.Clean("some tool output with seed-credential-value-01-x in it")
			}
		}
	}()
	for i := 0; i < 200; i++ {
		s.WatchValue(fmt.Sprintf("later-credential-%03d-%s", i, strings.Repeat("y", i%7)), "test")
	}
	close(stop)
	wg.Wait()
}

// The same overlap, asked what it costs: a value already watched must never
// come out of Clean while Watch is adding longer ones that sort ahead of it. An
// in-place sort under a reader that skips an element it moved past would let it
// through; none were seen in ~130k calls on the old code, so this holds the
// property rather than reproducing a leak.
func TestScrubberCleanKeepsRemovingAWatchedValueWhileWatchAddsLongerOnes(t *testing.T) {
	s := NewScrubber()
	const secret = "AAAA-watched-credential-first" // #nosec G101 -- a fixture, not a credential
	s.WatchValue(secret, "test")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var runs, leaks atomic.Int64
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					out := s.Clean("tool output: " + secret + " end")
					runs.Add(1)
					if strings.Contains(out, secret) {
						leaks.Add(1)
					}
				}
			}
		}()
	}
	for i := 0; i < 400; i++ {
		// each longer value sorts ahead of the secret and moves it
		s.WatchValue(fmt.Sprintf("later-credential-%04d-%s", i, strings.Repeat("y", 10+i%40)), "test")
	}
	close(stop)
	wg.Wait()
	t.Logf("Clean calls=%d, outputs still containing the watched value=%d", runs.Load(), leaks.Load())
	if leaks.Load() > 0 {
		t.Fatalf("%d Clean results leaked a watched credential", leaks.Load())
	}
}

// BenchmarkCleanWithWatchedValues is Clean on the hot path it is on: a handful
// of watched values and a tool-result-sized text that carries one of them.
func BenchmarkCleanWithWatchedValues(b *testing.B) {
	s := NewScrubber()
	for i := 0; i < 8; i++ {
		s.WatchValue(fmt.Sprintf("bench-credential-value-%02d-%s", i, strings.Repeat("k", 8+i)), "bench")
	}
	text := strings.Repeat("ordinary tool output line without a secret\n", 200) +
		"leaked bench-credential-value-03-kkkkkkkkkkk here\n"
	b.ReportAllocs()
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Clean(text)
	}
}
