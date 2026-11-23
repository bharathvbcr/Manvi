package devcouncil

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

// BenchmarkExecContainmentOverhead measures what one agent command pays for
// containment: the repository walk and match, profile rendering, and the
// sandbox-exec launch, against the same command run directly.
//
// The arms alternate within every iteration, so drift in machine load lands on
// both, and each is reported as its minimum and median over the iterations
// (min-of-N is the least noisy estimate of the cost itself). The repository is
// a synthetic tree of repoDirs x repoFiles ordinary files plus secret-named
// ones; a real checkout's walk cost scales with its entry count, so the sizes
// are sub-benchmarks.
//
//	go test ./devcouncil -run '^$' -bench ExecContainmentOverhead -benchtime 200x
func BenchmarkExecContainmentOverhead(b *testing.B) {
	for _, size := range []struct{ dirs, files int }{{10, 10}, {100, 50}} {
		b.Run(fmt.Sprintf("entries=%d", size.dirs*size.files), func(b *testing.B) {
			launcher, err := containmentLookup()
			if err != nil {
				b.Skipf("containment benchmark did not run: %v", err)
			}
			root := b.TempDir()
			files := map[string]string{".env": "x", "secrets/a.txt": "x", "deploy/server.key": "x"}
			for d := 0; d < size.dirs; d++ {
				for f := 0; f < size.files; f++ {
					files[fmt.Sprintf("pkg%03d/file%03d.go", d, f)] = "package x\n"
				}
			}
			writeTree(b, root, files)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			base := []string{"sh", "-c", "true"}
			run := func(argv []string) time.Duration {
				start := time.Now()
				_, code, timedOut, err := runArgv(ctx, root, argv)
				elapsed := time.Since(start)
				if err != nil || timedOut || code != 0 {
					b.Fatalf("%v: exit %d timedOut %v err %v", argv[0], code, timedOut, err)
				}
				return elapsed
			}
			if _, err := containArgv(ctx, root, nil, base); err != nil {
				b.Skipf("containment benchmark did not run: %v (launcher %s)", err, launcher)
			}

			off, on := make([]float64, 0, b.N), make([]float64, 0, b.N)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%2 == 0 {
					off = append(off, float64(run(base).Nanoseconds()))
				}
				start := time.Now()
				argv, err := containArgv(ctx, root, nil, base)
				if err != nil {
					b.Fatal(err)
				}
				run(argv)
				on = append(on, float64(time.Since(start).Nanoseconds()))
				if i%2 == 1 {
					off = append(off, float64(run(base).Nanoseconds()))
				}
			}
			b.StopTimer()
			slices.Sort(off)
			slices.Sort(on)
			b.ReportMetric(off[0], "off-min-ns")
			b.ReportMetric(on[0], "on-min-ns")
			b.ReportMetric(off[len(off)/2], "off-median-ns")
			b.ReportMetric(on[len(on)/2], "on-median-ns")
		})
	}
}
