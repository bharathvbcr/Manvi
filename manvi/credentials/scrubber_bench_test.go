package credentials

import (
	"strings"
	"sync"
	"testing"
)

// BenchmarkCleanWhileWatchAll is what a rendered line, a log payload or a tool
// result pays for Clean while another goroutine re-arms the scrubber, as callers
// do on every use: every value WatchAll offers is already watched, so the
// re-arming only ever asks "is this one known".
func BenchmarkCleanWhileWatchAll(b *testing.B) {
	r := NewResolver()
	r.lookup = func(name string) (string, bool) { return "bench-credential-value-for-" + name, true }
	s := NewScrubber()
	s.WatchAll(r)
	text := strings.Repeat("tool output without any credential in it. ", 8)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.WatchAll(r)
			}
		}
	}()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = s.Clean(text)
		}
	})
	b.StopTimer()
	close(stop)
	wg.Wait()
}
