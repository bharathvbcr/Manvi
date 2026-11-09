package devcouncil

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkGrepEndToEnd is the whole grep tool over n files with nothing
// refused, the common case, with the files aged past the stamp slack as they
// are in a real checkout. The same benchmark runs unchanged on the commit before
// the post-search check, to price what the check adds.
func BenchmarkGrepEndToEnd(b *testing.B) {
	for _, n := range []int{5000, 50000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			f := newFixture(b)
			for i := range n {
				benchPut(b, f.root, fmt.Sprintf("pkg%d/file%d.go", i%100, i),
					fmt.Sprintf("package p\n\nfunc F%d() int { return %d }\n", i, i))
			}
			ageTree(b)
			args := map[string]any{"pattern": `func F1234\b`}
			b.ResetTimer()
			for range b.N {
				if res := f.call("devcouncil_grep", args); res.IsError {
					b.Fatalf("grep failed: %s", res.Text)
				}
			}
		})
	}
}

// ageTree moves the stamp clock an hour on for the benchmark, the stand-in for
// files that were written long before the search.
func ageTree(b *testing.B) {
	b.Helper()
	prev := grepNow
	grepNow = func() time.Time { return time.Now().Add(time.Hour) }
	b.Cleanup(func() { grepNow = prev })
}
